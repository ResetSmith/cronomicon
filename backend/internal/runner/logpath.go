package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ResetSmith/cronomicon/internal/entitycode"
)

// Log file/dir modes, matching what the run-log writers already used.
const (
	logDirMode  os.FileMode = 0o750
	logFileMode os.FileMode = 0o640
)

// MetaFileName is the per-folder sidecar describing which entity owns it.
const MetaFileName = "_meta.json"

// LogPath returns the file a run's log lives in (LU-7).
//
// With an entity code:  {logDir}/{code}/{traceID}.log
// Without one:          {logDir}/{traceID}.log
//
// The empty-code case is not a fallback search — it is the permanent, correct
// answer for every run enqueued before migration 710. Those runs carry a NULL
// entity_code, their logs were written flat, and they stay there (LU-Q8(a)).
// Because the code travels on the run row, the reader always knows which layout
// applies without probing the filesystem, which is what makes coexistence free.
//
// One file per run is deliberate, over the per-job aggregate that was
// considered: it keeps a single writer per file, which preserves the chunked
// resume protocol, the O_TRUNC-on-rewind behaviour, the per-file size cap and
// the by-trace-id read route for free, and rules out interleaving between
// concurrent runs entirely. An append-only aggregate cannot be rewound, so every
// crashed-handler retransmit or 409-driven replay would duplicate content into
// it.
func LogPath(logDir, entityCode, traceID string) (string, error) {
	if !ValidTraceID(traceID) {
		return "", ErrBadTraceID
	}
	if entityCode == "" {
		return filepath.Join(logDir, traceID+".log"), nil
	}
	if !entitycode.Valid(entityCode) {
		return "", fmt.Errorf("%w: %q", ErrBadEntityCode, entityCode)
	}
	return filepath.Join(logDir, entityCode, traceID+".log"), nil
}

// ErrBadEntityCode rejects a folder name that did not come from the registry.
var ErrBadEntityCode = errors.New("invalid entity code")

// EnsureLogDir creates the directory a run's log will be written into.
//
// Using an opaque server-assigned code as the directory name is what makes this
// safe. Git-sourced job and workflow names are entirely unvalidated — upsertJobs
// inserts the YAML's `metadata.name` verbatim with no regex, allowlist, length
// limit or trim — so a job named `../../etc/cron.d/x` is reachable by anyone
// with merge rights on the synced repo, and an empty name falls back to a value
// that yields ".". Naming folders after entities directly would have made log
// storage the enforcement point for that; a code sidesteps it entirely.
func EnsureLogDir(logDir, entityCode string) error {
	if entityCode == "" {
		return os.MkdirAll(logDir, logDirMode)
	}
	if !entitycode.Valid(entityCode) {
		return fmt.Errorf("%w: %q", ErrBadEntityCode, entityCode)
	}
	return os.MkdirAll(filepath.Join(logDir, entityCode), logDirMode)
}

// WriteEntityMeta writes the folder's _meta.json sidecar if it is absent.
//
// `logs/a3f2c1d0/` tells a human nothing, and since a deleted entity's folder
// stays exactly where it is (LU-Q5(a)) there is no naming convention
// distinguishing a live entity's folder from a dead one's. The sidecar is the
// only way to answer "what is this folder, and is it still live?" from the log
// tree alone — which matters because that tree can be archived, shipped or
// mounted somewhere the database is not.
//
// Best-effort by design: the caller is on a run's write path, and failing to
// write an explanatory file must never fail the run. It is written once, on
// folder creation, and skipped cheaply thereafter — the stat is the guard, so a
// hot path costs one syscall rather than a query.
//
// Note this is deliberately NOT named "*.log": LU-11 classifies it separately so
// it isn't counted as a run log, and LU-1's reaper only removes *.log, so a
// folder's explanation outlives the logs it explains.
func WriteEntityMeta(ctx context.Context, db *sql.DB, logDir, entityCode string) {
	syncEntityMeta(ctx, db, logDir, entityCode, false)
}

// RefreshEntityMeta re-writes an EXISTING sidecar from the registry, picking up
// the deleted_at stamp so the log tree itself says the entity is gone (LU-Q5(a)
// keeps the folder in place, so without this there is nothing on disk to
// distinguish a dead entity's folder from a live one's).
//
// Unlike WriteEntityMeta it never creates the file: a folder that was never
// written to has no sidecar and no logs, and materialising a directory just to
// hold a tombstone for an entity that never ran would be noise.
func RefreshEntityMeta(ctx context.Context, db *sql.DB, logDir, entityCode string) {
	syncEntityMeta(ctx, db, logDir, entityCode, true)
}

// syncEntityMeta serialises the registry row into the folder's sidecar.
// requireExisting=false creates it when absent (folder creation); true updates
// only what is already there (delete stamping).
//
// Best-effort throughout: callers are on a run's write path or a delete path,
// and neither may fail because an explanatory file could not be written.
func syncEntityMeta(ctx context.Context, db *sql.DB, logDir, entityCode string, requireExisting bool) {
	if entityCode == "" || !entitycode.Valid(entityCode) {
		return
	}
	path := filepath.Join(logDir, entityCode, MetaFileName)
	// The stat is the guard that keeps the creation path cheap: on the hot path
	// the sidecar already exists, so this costs one syscall rather than a query.
	_, statErr := os.Stat(path)
	if requireExisting != (statErr == nil) {
		return
	}
	ent, err := entitycode.Describe(ctx, db, entityCode)
	if err != nil || ent == nil {
		return
	}
	b, err := json.MarshalIndent(ent, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(b, '\n'), logFileMode)
}
