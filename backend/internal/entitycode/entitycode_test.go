// Why this file exists (LU-6).
//
// The entity-code registry is the identity layer under the whole run-log folder
// tree, and every one of its properties is a silent-failure property: nothing
// crashes when it breaks, logs simply land in the wrong folder, or a dead
// entity's history merges into a new one's. Specifically:
//
//   - Allocate is called on EVERY git sync for EVERY definition. If it were not
//     idempotent the registry would grow a row per sync and the folder a job
//     writes into would change under it.
//   - The (kind, source, name) key is three-part because the git and cronomicon
//     namespaces are disjoint and a job and a workflow may share a name. Collapse
//     any part of that key and two distinct entities share a log folder.
//   - The delete/recreate cycle must mint a FRESH code (LU-Q6(b)) while KEEPING
//     the old row, because the old folder is left on disk and must stay
//     attributable.
//   - Valid() is the last guard before an untrusted-ish string becomes a
//     filesystem path component.
//
// None of that is observable from the outside until an operator opens a log
// folder and finds the wrong job's output in it, so it is asserted here directly.
package entitycode

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// migratedPool opens a fully-migrated temp-FILE SQLite database. A file (never
// ":memory:") because the pool holds several connections and an in-memory DB is
// per-connection — the concurrency test below would otherwise see a different,
// unmigrated database on each goroutine.
func migratedPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "entitycode.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// rowsFor counts every registry row for a tuple, live and historical.
func rowsFor(t *testing.T, pool *sql.DB, kind, source, name string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM entity_codes WHERE kind=? AND source=? AND name=?`,
		kind, source, name).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// TestAllocateIsIdempotentForOneTuple is the property that makes it safe to call
// Allocate unconditionally on every sync: repeated calls return the SAME code and
// leave exactly one row behind. Without it the registry would grow without bound
// and — worse — a job's log folder would move on every sync.
func TestAllocateIsIdempotentForOneTuple(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	first, err := Allocate(ctx, pool, KindJob, "git", "deploy", "uid-deploy")
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if first == "" {
		t.Fatal("first allocate returned an empty code")
	}
	for i := range 5 {
		got, err := Allocate(ctx, pool, KindJob, "git", "deploy", "uid-deploy")
		if err != nil {
			t.Fatalf("allocate #%d: %v", i+2, err)
		}
		if got != first {
			t.Fatalf("allocate #%d = %q, want the original %q — a job's log folder moved between syncs", i+2, got, first)
		}
	}
	if n := rowsFor(t, pool, KindJob, "git", "deploy"); n != 1 {
		t.Errorf("registry holds %d rows for one tuple, want 1 — Allocate is inserting on every call", n)
	}
	// And Lookup agrees, so the read path resolves the same folder.
	got, err := Lookup(ctx, pool, KindJob, "uid-deploy")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got != first {
		t.Errorf("Lookup = %q, want %q", got, first)
	}
}

// TestAllocateKeepsKindAndSourceNamespacesDisjoint pins the three-part key. Both
// definition tables are keyed PRIMARY KEY (source, name), so job/git/deploy,
// job/cronomicon/deploy and workflow/git/deploy are three entities that legitimately
// coexist. If the registry keyed on name alone (or on source+name) they would
// share one code and therefore one log folder, silently interleaving three
// different entities' run history.
func TestAllocateKeepsKindAndSourceNamespacesDisjoint(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	tuples := []struct{ kind, source, name string }{
		{KindJob, "git", "deploy"},
		{KindJob, "cronomicon", "deploy"},
		{KindWorkflow, "git", "deploy"},
	}
	seen := map[string]string{}
	for _, tp := range tuples {
		code, err := Allocate(ctx, pool, tp.kind, tp.source, tp.name, "uid-"+tp.kind+"-"+tp.source+"-"+tp.name)
		if err != nil {
			t.Fatalf("allocate %s/%s/%s: %v", tp.kind, tp.source, tp.name, err)
		}
		label := tp.kind + "/" + tp.source + "/" + tp.name
		if prev, dup := seen[code]; dup {
			t.Fatalf("%s got code %q, already held by %s — two distinct entities would share a log folder", label, code, prev)
		}
		seen[code] = label
	}
	if len(seen) != 3 {
		t.Fatalf("got %d distinct codes for 3 distinct entities: %v", len(seen), seen)
	}
}

// TestMarkDeletedThenAllocateMintsFreshCodeAndKeepsHistory is LU-Q6(b): a
// recreated entity is deliberately a DIFFERENT entity. The new code must differ
// (otherwise the new job inherits the old one's folder and its logs) and the old
// row must survive (otherwise the folder left on disk becomes unattributable).
func TestMarkDeletedThenAllocateMintsFreshCodeAndKeepsHistory(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	old, err := Allocate(ctx, pool, KindJob, "cronomicon", "reindex", "uid-reindex")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := MarkDeleted(ctx, pool, KindJob, "uid-reindex"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	// Once stamped there is no live row, so the tuple resolves to nothing.
	if got, err := Lookup(ctx, pool, KindJob, "uid-reindex"); err != nil || got != "" {
		t.Fatalf("Lookup after delete = %q, %v; want \"\", nil — a deleted row is still being resolved", got, err)
	}

	fresh, err := Allocate(ctx, pool, KindJob, "cronomicon", "reindex", "uid-reindex")
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if fresh == old {
		t.Errorf("recreated entity reused code %q — it would inherit the deleted entity's log folder", old)
	}
	if n := rowsFor(t, pool, KindJob, "cronomicon", "reindex"); n != 2 {
		t.Errorf("registry holds %d rows for the tuple, want 2 (one historical, one live) — the old era is not attributable", n)
	}
	// Exactly one of the two is live; the other carries the tombstone.
	var live, dead int
	if err := pool.QueryRow(`
		SELECT SUM(deleted_at IS NULL), SUM(deleted_at IS NOT NULL)
		FROM entity_codes WHERE kind=? AND source=? AND name=?`,
		KindJob, "cronomicon", "reindex").Scan(&live, &dead); err != nil {
		t.Fatalf("count live/dead: %v", err)
	}
	if live != 1 || dead != 1 {
		t.Errorf("live=%d dead=%d, want 1/1", live, dead)
	}
}

// TestMarkDeletedOnUnknownTupleIsANoOp keeps the caller's delete path idempotent.
// The delete handler stamps unconditionally, and a job that predates the registry
// (or was already deleted, or is being retried) has no live row. Returning an
// error there would fail an otherwise-correct DELETE and roll its transaction back.
func TestMarkDeletedOnUnknownTupleIsANoOp(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	if err := MarkDeleted(ctx, pool, KindJob, "uid-never-existed"); err != nil {
		t.Errorf("MarkDeleted on an unallocated tuple = %v, want nil", err)
	}
	if n := rowsFor(t, pool, KindJob, "cronomicon", "never-existed"); n != 0 {
		t.Errorf("MarkDeleted created %d rows for an unknown tuple, want 0", n)
	}
	// Twice in a row is also fine.
	if _, err := Allocate(ctx, pool, KindJob, "cronomicon", "twice", "uid-twice"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	for i := range 2 {
		if err := MarkDeleted(ctx, pool, KindJob, "uid-twice"); err != nil {
			t.Errorf("MarkDeleted #%d = %v, want nil", i+1, err)
		}
	}
	if n := rowsFor(t, pool, KindJob, "cronomicon", "twice"); n != 1 {
		t.Errorf("double MarkDeleted left %d rows, want 1", n)
	}
}

// TestLookupReturnsEmptyForUnknownAndDeletedTuples pins the "no code" answer as a
// value rather than an error, because that is what the enqueue path reads as "use
// the flat layout". A deleted row must not resolve either — otherwise a newly
// created job of the same name would be routed to the dead entity's folder before
// its own code is allocated.
func TestLookupReturnsEmptyForUnknownAndDeletedTuples(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	got, err := Lookup(ctx, pool, KindJob, "uid-no-such-job")
	if err != nil {
		t.Fatalf("Lookup on unknown tuple returned an error: %v", err)
	}
	if got != "" {
		t.Errorf("Lookup on unknown tuple = %q, want \"\"", got)
	}

	if _, err := Allocate(ctx, pool, KindWorkflow, "git", "nightly", "uid-nightly"); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := MarkDeleted(ctx, pool, KindWorkflow, "uid-nightly"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	got, err = Lookup(ctx, pool, KindWorkflow, "uid-nightly")
	if err != nil {
		t.Fatalf("Lookup after delete returned an error: %v", err)
	}
	if got != "" {
		t.Errorf("Lookup skipped the deleted_at filter: got %q, want \"\"", got)
	}
}

// TestFormatRendersEightLowercaseHex pins the folder-name shape. Every other
// guard (Valid, the path builder, the settings classifier) is written against
// exactly this format, and the scheduler's INSERT re-implements it in SQL as
// printf('%08x', code) — so a change here silently desynchronises Go and SQL.
func TestFormatRendersEightLowercaseHex(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{1, "00000001"},
		{15, "0000000f"},
		{255, "000000ff"},
		{0xABCDEF, "00abcdef"},
		{0xFFFFFFFF, "ffffffff"},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("Format(%d) = %q, want %q", c.in, got, c.want)
		}
		if got := Format(c.in); got != strings.ToLower(got) {
			t.Errorf("Format(%d) = %q, want lowercase", c.in, got)
		}
		if !Valid(Format(c.in)) {
			t.Errorf("Format(%d) = %q, which Valid rejects — the two disagree", c.in, Format(c.in))
		}
	}
}

// TestValidAcceptsOnlyCodesAndTheSystemBucket is the path-safety guard. The value
// it screens becomes a directory name, and git job/workflow names are entirely
// unvalidated on the way in — so the rejection cases here (empty, wrong length,
// uppercase, non-hex, separators, "..") are what keep a traversal out of the log
// tree, not a nicety.
func TestValidAcceptsOnlyCodesAndTheSystemBucket(t *testing.T) {
	accept := []string{"00000000", "0000000f", "deadbeef", "ffffffff", SystemCode}
	for _, s := range accept {
		if !Valid(s) {
			t.Errorf("Valid(%q) = false, want true", s)
		}
	}
	reject := []string{
		"",           // no code at all is the flat layout, not a folder name
		"0000000",    // 7 chars
		"000000000",  // 9 chars
		"DEADBEEF",   // uppercase: would collide with its lowercase twin on a case-insensitive FS
		"deadbeeg",   // 'g' is not hex
		"dead beef",  // space
		"../../etc",  // traversal
		"..",         // traversal
		".",          // current dir
		"dead/beef",  // separator
		"dead\\beef", // separator (windows-style)
		"..dead..",   // 8 chars, but dots
		"/0000000",   // absolute-path escape
		"_System",    // near-miss on the system bucket
		"_system2",   // 8 chars but not hex, not the bucket
	}
	for _, s := range reject {
		if Valid(s) {
			t.Errorf("Valid(%q) = true, want false — this string would become a log directory name", s)
		}
	}
}

// TestDescribeRoundTripsARegistryRow covers the sidecar's data source: _meta.json
// is the ONLY way to tell what a folder is from the log tree alone (the tree can
// be archived or mounted where the database is not), so Describe must return the
// full tuple and, once stamped, the deletion — a dead entity's folder is left in
// place and is otherwise indistinguishable from a live one's.
func TestDescribeRoundTripsARegistryRow(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	code, err := Allocate(ctx, pool, KindWorkflow, "git", "release-train", "uid-release-train")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	ent, err := Describe(ctx, pool, code)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if ent == nil {
		t.Fatal("Describe returned nil for a code it just allocated")
	}
	if ent.Code != code || ent.Kind != KindWorkflow || ent.Source != "git" || ent.Name != "release-train" {
		t.Errorf("Describe = %+v, want code=%s kind=workflow source=git name=release-train", ent, code)
	}
	if ent.CreatedAt == "" {
		t.Error("Describe returned an empty createdAt")
	}
	if ent.DeletedAt != nil {
		t.Errorf("a live entity reports deletedAt = %q", *ent.DeletedAt)
	}

	if err := MarkDeleted(ctx, pool, KindWorkflow, "uid-release-train"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	ent, err = Describe(ctx, pool, code)
	if err != nil {
		t.Fatalf("describe after delete: %v", err)
	}
	if ent == nil {
		t.Fatal("Describe returned nil for a deleted entity — the folder on disk becomes unattributable")
	}
	if ent.DeletedAt == nil || *ent.DeletedAt == "" {
		t.Error("Describe did not surface deletedAt after MarkDeleted — the log tree cannot say the entity is gone")
	}
}

// TestDescribeReturnsNilForNonRegistryCodes keeps the sidecar writer quiet rather
// than erroring on folder names that legitimately have no row: the system bucket
// (which is a fixed name, not an allocation), a code whose entity predates the
// registry, and anything unparseable found in the tree. All three are best-effort
// paths on a run's write path, so "no row" must be a value, not a failure.
func TestDescribeReturnsNilForNonRegistryCodes(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	for _, code := range []string{"", SystemCode, "zzzzzzzz", "not-a-code", "00000001"} {
		ent, err := Describe(ctx, pool, code)
		if err != nil {
			t.Errorf("Describe(%q) returned an error: %v", code, err)
		}
		if ent != nil {
			t.Errorf("Describe(%q) = %+v, want nil", code, ent)
		}
	}
}

// TestAllocateUnderConcurrencyYieldsOneCode is the property the ON CONFLICT
// tolerance exists for. Two syncs, or a sync racing an in-app create, can reach
// the INSERT together; the partial unique index rejects the loser, and losing
// that race must NOT be an error and must NOT mint a second code. Run under -race.
func TestAllocateUnderConcurrencyYieldsOneCode(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()

	const n = 16
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		codes []string
		errs  []error
	)
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			code, err := Allocate(ctx, pool, KindJob, "git", "hot-tuple", "uid-hot-tuple")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			codes = append(codes, code)
		})
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("%d of %d concurrent Allocate calls failed; first: %v", len(errs), n, errs[0])
	}
	if len(codes) != n {
		t.Fatalf("got %d codes from %d calls", len(codes), n)
	}
	for i, c := range codes {
		if c != codes[0] {
			t.Fatalf("concurrent Allocate #%d returned %q, want the same %q as everyone else", i, c, codes[0])
		}
	}
	if got := rowsFor(t, pool, KindJob, "git", "hot-tuple"); got != 1 {
		t.Errorf("%d concurrent allocations produced %d registry rows, want 1", n, got)
	}
}
