// Package entitycode allocates and resolves the stable per-entity codes that
// name run-log folders (LU-6).
//
// # Why a registry table rather than a column
//
// `jobs` and `workflows` have no surrogate key. What the API exposes as `id` is
// SQLite's implicit rowid — a physical storage address, not an identity. It is
// destroyed by a sync prune-and-re-add, by any table-rebuild migration (170 and
// 490 both did `INSERT INTO jobs_new SELECT …; DROP TABLE jobs;` without
// preserving rowids), and potentially by the nightly `VACUUM INTO` restore path.
// A directory tree keyed on rowid would silently reshuffle.
//
// The obvious fix — add an AUTOINCREMENT column — is illegal here: AUTOINCREMENT
// requires the column be `INTEGER PRIMARY KEY`, there can be exactly one per
// table, and both tables already have `PRIMARY KEY (source, name)`, which is the
// entire premise of the dual-source model. Hence a separate table whose sole
// INTEGER PRIMARY KEY can carry AUTOINCREMENT.
//
// AUTOINCREMENT is doing real work: it guarantees SQLite never reuses a freed
// value, so a newly created entity can never inherit a dead one's folder — the
// property that makes it safe to leave dead folders in place (LU-Q5(a)).
//
// # Keyed on (kind, source, name)
//
// All three are required. `PRIMARY KEY (source, name)` on both tables means the
// git and cronomicon namespaces are deliberately disjoint, and a job and a workflow
// may freely share a name. So job/git/deploy, job/cronomicon/deploy and
// workflow/git/deploy are three distinct entities that coexist with three
// distinct codes.
//
// # Temporal recurrence
//
// A tuple may recur over the system's lifetime: an entity is deleted, and later
// something with the same (kind, source, name) is created. Per LU-Q6(b) that
// gets a FRESH code, so a plain UNIQUE(kind, source, name) cannot be used — it
// would block the second allocation. The uniqueness is instead a partial index
// over live rows only, and history accumulates unconstrained.
//
// Crucially, only an explicit operator delete counts as "deleted" (§5.6.1): a
// GitLab sync prune must NOT stamp deleted_at. A git job disappears from a sync
// for reasons nobody chose — a repo reorganisation, a rename, a transient YAML
// validation failure, a branch switch, a partial clone — and returns on the next
// good sync. Treating that as a deletion would strand a folder and split the
// job's log history on every such cycle. See MarkDeleted.
package entitycode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// DB is the subset of *sql.DB / *sql.Tx these functions need. Every allocation
// site is already inside a transaction — the git sync wraps its whole DB phase in
// one, and both compose handlers open their own — and an allocation must commit
// or roll back with the row it belongs to. Taking the interface is what lets the
// same code serve both.
type DB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Kinds. The registry is shared by both definition tables.
const (
	KindJob      = "job"
	KindWorkflow = "workflow"
)

// SystemCode is the bucket for run logs with no owning definition — today only
// the SSH "Test connection" probe, which records a run but belongs to no job.
// It is a fixed name rather than an allocated code so it can never collide with
// one: allocated codes are 8 lowercase hex characters, and this is not.
const SystemCode = "_system"

// Format renders an allocated code as its folder name. Eight hex characters
// covers the full 32-bit space; a smaller space was considered and rejected
// (16 bits is 65,536 values — a birthday collision around 256 entities if
// assigned randomly, or a hard lifetime ceiling if assigned sequentially).
func Format(code int64) string { return fmt.Sprintf("%08x", code) }

// Valid reports whether s is a well-formed folder name for a run log — either an
// allocated code or the system bucket. Used to keep an unexpected value from
// reaching a filesystem path, in the same spirit as runner.ValidTraceID.
func Valid(s string) bool {
	if s == SystemCode {
		return true
	}
	if len(s) != 8 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Lookup returns the live code for the definition, or "" when none is
// allocated. Keyed on the uid (R2-5): under per-agency naming a (source, name)
// pair may match two definitions, and a shared log folder — or one department
// reading the other's — is exactly what this registry exists to prevent.
func Lookup(ctx context.Context, db DB, kind, uid string) (string, error) {
	var code int64
	err := db.QueryRowContext(ctx, `
		SELECT code FROM entity_codes
		WHERE kind = ? AND uid = ? AND deleted_at IS NULL`,
		kind, uid).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup entity code: %w", err)
	}
	return Format(code), nil
}

// Allocate returns the live code for the tuple, minting one if none exists.
//
// Idempotent, and safe to call on every sync: the common path is a single
// indexed SELECT that finds the existing row. Only a genuinely new entity pays
// for an INSERT.
//
// The INSERT tolerates a conflict rather than assuming the preceding SELECT
// still holds — two syncs, or a sync racing an in-app create, can reach the
// INSERT together, and the partial unique index will reject the loser. Losing
// that race is not an error: the winner allocated the same tuple, and the
// re-read below returns its code.
func Allocate(ctx context.Context, db DB, kind, source, name, uid string) (string, error) {
	if code, err := Lookup(ctx, db, kind, uid); err != nil || code != "" {
		return code, err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_codes (kind, source, name, created_at, uid)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`,
		kind, source, name, nowRFC3339(), uid)
	if err != nil {
		return "", fmt.Errorf("allocate entity code: %w", err)
	}
	code, err := Lookup(ctx, db, kind, uid)
	if err != nil {
		return "", err
	}
	if code == "" {
		// The INSERT reported success (or a tolerated conflict) yet no live row
		// exists — the tuple would have to have been deleted between the two
		// statements. Surface it rather than returning "" and silently writing
		// logs to the flat fallback path.
		return "", fmt.Errorf("allocate entity code: no live row for %s/%s/%s after insert", kind, source, name)
	}
	return code, nil
}

// MarkDeleted stamps the live row for the tuple as deleted, so the next create
// of the same tuple mints a fresh code (LU-Q6(b)).
//
// Call this ONLY from an explicit operator delete. A GitLab sync prune must not
// (§5.6.1) — see the package comment. The stamp is also what frees the partial
// unique index; skipping it on a real delete would make the next create of that
// name fail with a constraint violation.
//
// A tuple with no live row is not an error: deleting something that was never
// allocated (or was already stamped) is a no-op, which keeps the caller's delete
// path idempotent.
func MarkDeleted(ctx context.Context, db DB, kind, uid string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE entity_codes SET deleted_at = ?
		WHERE kind = ? AND uid = ? AND deleted_at IS NULL`,
		nowRFC3339(), kind, uid)
	if err != nil {
		return fmt.Errorf("mark entity code deleted: %w", err)
	}
	return nil
}

// Entity is a registry row, for describing a folder to a human.
type Entity struct {
	Code      string  `json:"code"`
	Kind      string  `json:"kind"`
	Source    string  `json:"source"`
	Name      string  `json:"name"`
	CreatedAt string  `json:"createdAt"`
	DeletedAt *string `json:"deletedAt,omitempty"`
}

// Describe returns the registry row behind a folder name, or nil when the code
// is unknown (or is the system bucket, which has no row).
func Describe(ctx context.Context, db DB, code string) (*Entity, error) {
	// Gate on the full shape, not just parseability. A bare Sscanf would happily
	// read "12" as 0x12 and return a completely unrelated entity — the folder name
	// has to be exactly what Format produced, or it names nothing.
	if code == "" || code == SystemCode || !Valid(code) {
		return nil, nil
	}
	n, perr := strconv.ParseUint(code, 16, 32)
	if perr != nil {
		return nil, nil //nolint:nilerr // an unparseable folder name simply has no row
	}
	e := Entity{Code: code}
	var deletedAt sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT kind, source, name, created_at, deleted_at FROM entity_codes WHERE code = ?`,
		int64(n)).Scan(&e.Kind, &e.Source, &e.Name, &e.CreatedAt, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("describe entity code: %w", err)
	}
	if deletedAt.Valid && deletedAt.String != "" {
		e.DeletedAt = &deletedAt.String
	}
	return &e, nil
}

// nowRFC3339 is a seam for tests.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }
