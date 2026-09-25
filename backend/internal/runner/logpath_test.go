// Why this file exists (LU-7).
//
// LogPath is the single place that decides where a run's bytes land, and it has
// to serve two layouts at once: the foldered one migration 710 introduced, and
// the flat one every pre-710 run already uses. The flat branch is NOT a fallback
// or a legacy shim — it is the permanent, correct answer for a run whose
// entity_code is NULL (LU-Q8(a): no backfill, the layouts coexist forever). If it
// ever stopped being honoured, every historical run's log would 404 in the UI
// while still sitting on disk.
//
// The rejection cases are load-bearing for a different reason. Git job and
// workflow names are inserted verbatim from synced YAML with no validation at
// all, so naming folders after entities would have made log storage the
// enforcement point for `../../etc/cron.d/x`. Using an opaque server-assigned
// code sidesteps that — but only if nothing else can reach the directory-name
// slot, which is what ErrBadEntityCode is checked for here.
//
// The _meta.json sidecar is the only thing that explains a folder from the log
// tree alone (that tree gets archived and mounted where the database is not), and
// its two writers have deliberately asymmetric create/update semantics that are
// easy to collapse into one by accident.
package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
)

// logPathPool opens a migrated temp-FILE database for the sidecar tests, which
// need a real entity_codes registry to describe.
func logPathPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "logpath.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestLogPathRoutesByEntityCodePresence pins both layouts side by side: a code
// selects {dir}/{code}/{trace}.log, and no code selects the flat
// {dir}/{trace}.log. Asserting them together is the point — the decision is made
// from the run row alone, with no filesystem probing, which is what makes the two
// layouts free to coexist.
func TestLogPathRoutesByEntityCodePresence(t *testing.T) {
	dir := "/var/lib/amadeus/logs"
	trace := "abc123def456"

	got, err := LogPath(dir, "a3f2c1d0", trace)
	if err != nil {
		t.Fatalf("foldered LogPath: %v", err)
	}
	if want := filepath.Join(dir, "a3f2c1d0", trace+".log"); got != want {
		t.Errorf("LogPath with a code = %q, want %q", got, want)
	}

	got, err = LogPath(dir, "", trace)
	if err != nil {
		t.Fatalf("flat LogPath: %v", err)
	}
	if want := filepath.Join(dir, trace+".log"); got != want {
		t.Errorf("LogPath with no code = %q, want the flat %q — every pre-710 run's log would become unreachable", got, want)
	}

	// The system bucket is a folder like any other, so ssh-test runs group too.
	got, err = LogPath(dir, entitycode.SystemCode, trace)
	if err != nil {
		t.Fatalf("system-bucket LogPath: %v", err)
	}
	if want := filepath.Join(dir, entitycode.SystemCode, trace+".log"); got != want {
		t.Errorf("LogPath with the system bucket = %q, want %q", got, want)
	}
}

// TestLogPathRejectsUnsafeTraceIDsAndEntityCodes covers both path components.
// Either one becomes a filesystem path element, and either one could otherwise
// escape the log directory entirely — so both guards are asserted, including that
// they report distinguishable errors (the ingest handler maps a bad trace to 400
// and a bad code to a 500-class fault, which are genuinely different bugs).
func TestLogPathRejectsUnsafeTraceIDsAndEntityCodes(t *testing.T) {
	dir := t.TempDir()

	for _, trace := range []string{"", "../../etc/passwd", "..", ".hidden", "a/b", "a b"} {
		if _, err := LogPath(dir, "a3f2c1d0", trace); !errors.Is(err, ErrBadTraceID) {
			t.Errorf("LogPath(trace=%q) error = %v, want ErrBadTraceID", trace, err)
		}
	}

	for _, code := range []string{"../etc", "..", "/etc", "dead/beef", "DEADBEEF", "deadbeeg", "0000000", "000000000"} {
		got, err := LogPath(dir, code, "abc123def456")
		if !errors.Is(err, ErrBadEntityCode) {
			t.Errorf("LogPath(code=%q) = %q, err %v; want ErrBadEntityCode", code, got, err)
		}
		if got != "" {
			t.Errorf("LogPath(code=%q) returned a path %q alongside its error", code, got)
		}
	}

	// Concretely: the traversal case must not be able to name a file outside dir.
	if p, err := LogPath(dir, "../etc", "abc123def456"); err == nil {
		if rel, rerr := filepath.Rel(dir, p); rerr == nil && !filepath.IsAbs(rel) && rel[0] != '.' {
			t.Fatalf("traversal code produced an in-tree path %q", p)
		}
		t.Fatalf("traversal entity code was accepted, yielding %q", p)
	}
}

// TestEnsureLogDirCreatesTheFolderWithRestrictiveModeAndIsIdempotent covers the
// per-run call: it runs on every ingest chunk, so it must be cheap to repeat, and
// run logs can contain operator output that was only partially redacted, so the
// directory must not be world-readable.
func TestEnsureLogDirCreatesTheFolderWithRestrictiveModeAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	const code = "a3f2c1d0"

	for i := range 3 {
		if err := EnsureLogDir(dir, code); err != nil {
			t.Fatalf("EnsureLogDir call #%d: %v", i+1, err)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, code))
	if err != nil {
		t.Fatalf("folder was not created: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", filepath.Join(dir, code))
	}
	if perm := fi.Mode().Perm(); perm != 0o750 {
		t.Errorf("folder mode = %04o, want 0750 — run logs would be readable beyond the service account", perm)
	}

	// The empty code creates the log root itself, which is the flat layout's
	// equivalent and must keep working.
	flatRoot := filepath.Join(t.TempDir(), "nested", "logs")
	if err := EnsureLogDir(flatRoot, ""); err != nil {
		t.Fatalf("EnsureLogDir with no code: %v", err)
	}
	if fi, err := os.Stat(flatRoot); err != nil || !fi.IsDir() {
		t.Errorf("flat log root not created: %v", err)
	}

	for _, bad := range []string{"../escape", "..", "dead/beef", "DEADBEEF"} {
		if err := EnsureLogDir(dir, bad); !errors.Is(err, ErrBadEntityCode) {
			t.Errorf("EnsureLogDir(code=%q) = %v, want ErrBadEntityCode", bad, err)
		}
	}
	// Nothing new landed in the log root: the only entry is the one legitimate
	// folder created above. Checked by listing rather than by stat-ing each
	// rejected name, because a name like ".." stats to the parent directory
	// whether or not anything was created.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read log root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != code {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("log root holds %v, want only [%s] — a rejected code created a directory", names, code)
	}
}

// readMeta parses a folder's sidecar, failing the test if it is absent.
func readMeta(t *testing.T, dir, code string) entitycode.Entity {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, code, MetaFileName))
	if err != nil {
		t.Fatalf("read %s: %v", MetaFileName, err)
	}
	var e entitycode.Entity
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("parse %s (%s): %v", MetaFileName, b, err)
	}
	return e
}

// TestWriteEntityMetaCreatesTheSidecarOnceFromTheRegistry pins the create-only
// semantics. It is called on EVERY ingest chunk, so re-writing an existing
// sidecar would mean a query and a write per chunk on the hot path; the stat is
// the guard. The unknown-code case must be silent because this runs on a run's
// write path and an explanatory file may never fail a run.
func TestWriteEntityMetaCreatesTheSidecarOnceFromTheRegistry(t *testing.T) {
	pool := logPathPool(t)
	ctx := context.Background()
	dir := t.TempDir()

	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "deploy", "uid-deploy")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := EnsureLogDir(dir, code); err != nil {
		t.Fatalf("ensure log dir: %v", err)
	}

	WriteEntityMeta(ctx, pool, dir, code)
	ent := readMeta(t, dir, code)
	if ent.Code != code || ent.Kind != entitycode.KindJob || ent.Source != "git" || ent.Name != "deploy" {
		t.Errorf("sidecar = %+v, want code=%s kind=job source=git name=deploy", ent, code)
	}
	if ent.DeletedAt != nil {
		t.Errorf("live entity's sidecar carries deletedAt = %q", *ent.DeletedAt)
	}

	// A second call must leave the file exactly as it found it. Overwriting is
	// checked by content, not mtime, so the assertion holds on coarse clocks.
	sentinel := []byte(`{"code":"handwritten"}`)
	metaPath := filepath.Join(dir, code, MetaFileName)
	if err := os.WriteFile(metaPath, sentinel, 0o640); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}
	WriteEntityMeta(ctx, pool, dir, code)
	after, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("re-read sidecar: %v", err)
	}
	if string(after) != string(sentinel) {
		t.Errorf("WriteEntityMeta overwrote an existing sidecar (%s) — it would re-query and re-write on every ingest chunk", after)
	}

	// An unknown code (a folder whose entity predates the registry, or the system
	// bucket, which has no row) writes nothing and does not panic.
	for _, unknown := range []string{"00ffff00", entitycode.SystemCode, "", "../escape"} {
		if unknown != "" && entitycode.Valid(unknown) {
			if err := EnsureLogDir(dir, unknown); err != nil {
				t.Fatalf("ensure %q: %v", unknown, err)
			}
		}
		WriteEntityMeta(ctx, pool, dir, unknown)
		if _, err := os.Stat(filepath.Join(dir, unknown, MetaFileName)); err == nil {
			t.Errorf("WriteEntityMeta wrote a sidecar for the unknown code %q", unknown)
		}
	}
}

// TestRefreshEntityMetaUpdatesOnlyAnExistingSidecar is the delete-path half.
// LU-Q5(a) leaves a deleted entity's folder exactly where it is, so without the
// deletedAt stamp there is nothing on disk distinguishing a dead entity's folder
// from a live one's. But it must never CREATE the file: an entity that never ran
// has no folder, and materialising one just to hold a tombstone is noise the
// reaper would then have to consider.
func TestRefreshEntityMetaUpdatesOnlyAnExistingSidecar(t *testing.T) {
	pool := logPathPool(t)
	ctx := context.Background()
	dir := t.TempDir()

	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "cronomicon", "reindex", "uid-reindex")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := EnsureLogDir(dir, code); err != nil {
		t.Fatalf("ensure log dir: %v", err)
	}
	WriteEntityMeta(ctx, pool, dir, code)

	if err := entitycode.MarkDeleted(ctx, pool, entitycode.KindJob, "uid-reindex"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	RefreshEntityMeta(ctx, pool, dir, code)

	ent := readMeta(t, dir, code)
	if ent.DeletedAt == nil || *ent.DeletedAt == "" {
		t.Errorf("sidecar = %+v, want deletedAt stamped — the log tree cannot say this folder's entity is gone", ent)
	}
	if ent.Name != "reindex" || ent.Kind != entitycode.KindJob {
		t.Errorf("refresh lost the identity fields: %+v", ent)
	}

	// A never-written-to folder gets nothing: no sidecar, and no directory
	// conjured to hold one.
	other, err := entitycode.Allocate(ctx, pool, entitycode.KindWorkflow, "cronomicon", "never-ran", "uid-never-ran")
	if err != nil {
		t.Fatalf("allocate other: %v", err)
	}
	if err := entitycode.MarkDeleted(ctx, pool, entitycode.KindWorkflow, "uid-never-ran"); err != nil {
		t.Fatalf("mark other deleted: %v", err)
	}
	RefreshEntityMeta(ctx, pool, dir, other)
	if _, err := os.Stat(filepath.Join(dir, other, MetaFileName)); err == nil {
		t.Errorf("RefreshEntityMeta created a sidecar for a folder that never held a log")
	}
	if _, err := os.Stat(filepath.Join(dir, other)); err == nil {
		t.Errorf("RefreshEntityMeta created the folder %s for an entity that never ran", other)
	}
}
