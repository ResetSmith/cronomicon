package runref

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// ownerMatch is the WHERE fragment every binding read and delete keys on: the
// two-arm shape the R2 band standardised (same CASE form as the scheduler's and
// the reaction path's job reads). When the owner carries a uid that uid IS the
// match — no name arm at all, so a same-named sibling's rows are invisible here.
// When it does not, the legacy (kind, source, name) arm serves script owners
// (permanently name-identified) and callers that never resolved a row.
//
// The arms are exclusive by construction — the CASE picks one — so no caller can
// accidentally get the UNION, which is precisely what the pre-R2F-1 name-only
// query returned for two same-named cronomicon siblings.
const ownerMatch = `CASE WHEN ? != '' THEN owner_uid = ?
		      ELSE owner_kind = ? AND owner_source = ? AND owner_name = ? END`

// ownerArgs binds ownerMatch's five parameters, in order.
func ownerArgs(owner Owner) []any {
	uid := owner.UIDKey()
	return []any{uid, uid, owner.Kind, owner.Source, owner.Name}
}

// resolveUniqueJobUID returns the uid of the ONE job with this (name, source), or
// "" when there is none or more than one. Deliberately not a "pick the first":
// ambiguity is answered with silence, never with a choice the caller did not make.
func resolveUniqueJobUID(ctx context.Context, database *sql.DB, name, source string) string {
	var uid string
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(uid), '') FROM jobs WHERE name = ? AND source = ?`,
		name, source).Scan(&n, &uid); err != nil || n != 1 {
		return ""
	}
	return uid
}

// ListBindings returns an owner's declared reference bindings, sorted (kind, name)
// by the query. The derived Reference is populated on each row. Never nil.
func ListBindings(ctx context.Context, database *sql.DB, owner Owner) ([]Binding, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT ref_kind, ref_name, COALESCE(alias,'') FROM reference_bindings
		 WHERE `+ownerMatch+`
		 ORDER BY ref_kind, ref_name, alias`,
		ownerArgs(owner)...)
	if err != nil {
		return nil, fmt.Errorf("list reference bindings: %w", err)
	}
	defer rows.Close()
	out := []Binding{}
	for rows.Next() {
		var k, n, a string
		if err := rows.Scan(&k, &n, &a); err != nil {
			return nil, err
		}
		// Reference stays the row's OWN derived form — the binding's identity, which
		// is what the authoring surfaces label the chip with. The aliased destination
		// is InjectReference(), derived at the injection seam, never stored.
		b := Binding{Kind: Kind(k), Name: n, As: a}
		b.Reference = b.Kind.Reference(n)
		out = append(out, b)
	}
	return out, rows.Err()
}

// AuditBindingSet renders a change_log `details` string for a binding-set write:
// the resulting set as sorted "kind:name" entries (RA-7: "kind:name→alias" when
// the binding is aliased — an audit that recorded only the destination could not
// answer WHOSE credential a shared job body was wired to), NEVER values (bindings
// have no values). M7: a full-replace PUT is last-writer-wins, so a stale editor tab can
// silently resurrect a just-revoked binding; recording the RESULTING set makes the
// change visible and diffable in the audit (and distinguishes an empty-set revoke
// from a mere tag edit, which both otherwise log "updated" with no detail). bs is
// assumed already sorted (ListBindings orders by kind, name).
func AuditBindingSet(bs []Binding) string {
	if len(bs) == 0 {
		return "bindings: (none)"
	}
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		entry := string(b.Kind) + ":" + b.Name
		if b.As != "" {
			entry += "→" + b.As
		}
		parts = append(parts, entry)
	}
	return "bindings: " + strings.Join(parts, ", ")
}

// MaxBindingsPerOwner caps the reference bindings a single owner (job/script) may
// declare (L9). A full-replace PUT otherwise inserts every deduped row, so a
// ManageEnvVars caller could push tens of thousands of rows per request; a run
// declaring more than a couple hundred references is a mistake, not a use case.
const MaxBindingsPerOwner = 256

// ReplaceBindings sets an owner's bindings to exactly `bindings` (full replace:
// DELETE all rows for the owner, INSERT the validated+deduped set, one tx). Each
// binding's kind must be valid and its bare name — and its alias, when it declares
// one — must pass the derived-reference charset (envref); secret names
// additionally carry the reserved-KEK bar. An invalid entry, an alias collision
// (RA-Q2), or more than MaxBindingsPerOwner rows aborts the whole replace and
// returns an *envref.Error the API maps to 422. The single standalone tx keeps the
// SQLite pool deadlock-safe.
func ReplaceBindings(ctx context.Context, database *sql.DB, owner Owner, bindings []Binding, actor string) error {
	// L9: bound the work before validating/inserting — reject an oversized set early.
	if len(bindings) > MaxBindingsPerOwner {
		return &envref.Error{Msg: fmt.Sprintf("too many bindings: %d exceeds the per-owner limit of %d", len(bindings), MaxBindingsPerOwner)}
	}
	seen := map[string]bool{}
	clean := make([]Binding, 0, len(bindings))
	for _, b := range bindings {
		if err := ValidateBinding(b); err != nil {
			return err
		}
		// RA-4: kind+name+alias, so the same row aliased twice survives as two
		// bindings (it lands two keys) while an exact repeat collapses. The PK
		// widened to match in migration 820 — dedupe and the constraint must agree
		// or a legitimate set becomes a constraint violation.
		key := DedupeKey(b)
		if seen[key] {
			continue
		}
		seen[key] = true
		clean = append(clean, b)
	}
	// RA-Q2: two bindings that would land on the same injected key. Refused rather
	// than silently last-wins — see CheckAliasCollisions.
	if err := CheckAliasCollisions(clean); err != nil {
		return err
	}

	// R2F-1 — the uid stamped on every inserted row. Normally the caller's: it knows
	// which job it fetched, and a name subquery under duplicates is a guess that
	// could file one twin's credentials under the other's identity.
	//
	// A job owner arriving WITHOUT one still gets a resolve, but only when the name
	// resolves to exactly one job. That is not a guess, and it is load-bearing: the
	// delete trigger (1050) cascades a job's bindings by owner_uid ALONE, so a
	// NULL-uid row outlives its owner and a later same-named job silently inherits
	// the grant — the leak the cascade exists to prevent. Under duplicates it stays
	// NULL, which is the legacy shared name-keyed namespace such a caller always
	// had; no in-tree writer is in that position (both go through a fetched job row).
	ownerUID := owner.UIDKey()
	if ownerUID == "" && owner.Kind == "job" {
		ownerUID = resolveUniqueJobUID(ctx, database, owner.Name, owner.Source)
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin binding replace: %w", err)
	}
	defer tx.Rollback()

	// R2F-1 — the delete half of the full replace clears only THIS owner's rows.
	// Keyed by name it cleared both same-named siblings', so saving one twin's set
	// silently destroyed the other's: cross-agency data loss on an ordinary edit.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reference_bindings WHERE `+ownerMatch, ownerArgs(owner)...); err != nil {
		return fmt.Errorf("clear bindings: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, b := range clean {
		if _, err := tx.ExecContext(ctx,
			// R2-2: owner_uid is stamped for JOB owners only — a script's identity
			// is still its name (scripts keep PRIMARY KEY (name) and stay outside
			// AF-4b), which UIDKey enforces. See ownerUID above for where it comes
			// from and why the caller's beats a subquery's.
			`INSERT INTO reference_bindings
			   (owner_kind, owner_source, owner_name, ref_kind, ref_name, alias, created_by, created_at, owner_uid)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`,
			owner.Kind, owner.Source, owner.Name, string(b.Kind), b.Name, b.As, actor, now,
			ownerUID); err != nil {
			return fmt.Errorf("insert binding: %w", err)
		}
	}
	return tx.Commit()
}
