// Why this file exists (LU-6/LU-7).
//
// A run's log folder is decided ONCE, at enqueue, by a correlated subquery in the
// INSERT — not at log-write time. That choice is deliberate: `runs` has no
// foreign key to `jobs`, so a job deleted or pruned mid-run would leave the
// log writer with nothing to resolve, and the run's own output would move (or
// vanish) underneath it. Stamping makes the destination a property of the run.
//
// Because the stamp is a subquery buried inside a 20-column INSERT, it fails
// SILENTLY: a wrong `kind`, a swapped source/name argument pair, or a missing
// `deleted_at IS NULL` filter all still insert the row, just with a NULL or wrong
// entity_code. Nothing surfaces until an operator opens History and finds an
// empty log. Both outcomes are pinned here — the code when one exists, and NULL
// (the flat layout, which is the correct answer, not a failure) when it does not.
package scheduler

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/entitycode"
)

// runEntityCode reads a run's stamped folder code, distinguishing NULL (the flat
// layout) from a value. Returned as (code, isNull) because the two are different
// answers and a COALESCE would erase the distinction under test.
func runEntityCode(t *testing.T, pool *sql.DB, traceID string) (string, bool) {
	t.Helper()
	var code sql.NullString
	if err := pool.QueryRow(`SELECT entity_code FROM runs WHERE id=?`, traceID).Scan(&code); err != nil {
		t.Fatalf("read entity_code for %s: %v", traceID, err)
	}
	return code.String, !code.Valid
}

// TestEnqueueStampsTheJobsEntityCodeOnTheRun is the happy path: a job that has an
// allocated code hands that exact code to every run it produces, so the run's
// output lands in the job's folder.
func TestEnqueueStampsTheJobsEntityCodeOnTheRun(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedJobRow(t, pool, "coded", 1)
	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "coded", "uid-coded")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	traceID, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "coded", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, isNull := runEntityCode(t, pool, traceID)
	if isNull {
		t.Fatalf("run was stamped NULL though the job holds code %s — its log would go to the flat path while the reader looks in the folder", code)
	}
	if got != code {
		t.Errorf("run entity_code = %q, want the job's %q", got, code)
	}
	// The SQL-side printf('%08x', code) must agree with Go's Format, since the
	// reader builds the path from this string.
	if !entitycode.Valid(got) {
		t.Errorf("stamped code %q is not a well-formed folder name — the path builder will reject it", got)
	}
}

// TestEnqueueStampsNullWhenTheJobHasNoLiveCode pins the coexistence half
// (LU-Q8(a)). A job with nothing allocated — every job on a database that has not
// yet re-synced, and every pre-710 run — must produce a NULL stamp, which the
// path builder reads as the flat layout. A non-NULL guess here would send the run
// to a folder that nothing else knows about.
func TestEnqueueStampsNullWhenTheJobHasNoLiveCode(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedJobRow(t, pool, "uncoded", 1)
	// A code for a DIFFERENT tuple exists, so the subquery has rows to match
	// against and a missing filter would be visible rather than vacuously passing.
	if _, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "someone-else", "uid-someone-else"); err != nil {
		t.Fatalf("allocate decoy: %v", err)
	}
	if _, err := entitycode.Allocate(ctx, pool, entitycode.KindWorkflow, "git", "uncoded", "uid-wf-uncoded"); err != nil {
		t.Fatalf("allocate workflow of the same name: %v", err)
	}

	traceID, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "uncoded", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, isNull := runEntityCode(t, pool, traceID)
	if !isNull {
		t.Errorf("run entity_code = %q, want NULL — a job with no allocated code must use the flat layout (and the workflow of the same name must not leak its code across kinds)", got)
	}
}

// TestEnqueueIgnoresADeletedEntityCode is the LU-Q6(b) half at the enqueue site:
// once a code is stamped deleted, a run of a same-named job must NOT be routed
// into the dead entity's folder. The `deleted_at IS NULL` filter in the subquery
// is the only thing preventing that, and dropping it would merge a new job's
// history into its deleted predecessor's.
func TestEnqueueIgnoresADeletedEntityCode(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	seedJobRow(t, pool, "recycled", 1)
	dead, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "recycled", "uid-recycled")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := entitycode.MarkDeleted(ctx, pool, entitycode.KindJob, "uid-recycled"); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	traceID, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "recycled", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, isNull := runEntityCode(t, pool, traceID)
	if !isNull {
		t.Errorf("run entity_code = %q (dead code was %q), want NULL — a new run was routed into a deleted entity's folder", got, dead)
	}

	// Re-allocating gives a fresh code, and the next run picks THAT up.
	fresh, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "recycled", "uid-recycled")
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	traceID, err = EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "recycled", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue after re-allocate: %v", err)
	}
	got, _ = runEntityCode(t, pool, traceID)
	if got != fresh {
		t.Errorf("run entity_code = %q, want the freshly minted %q (not the dead %q)", got, fresh, dead)
	}
}

// TestEnqueueStampsPerJobSource keeps the dual-source namespaces apart at the
// enqueue site. `jobs` is keyed PRIMARY KEY (source, name), so a git `deploy` and
// an cronomicon `deploy` are two jobs; if the subquery ignored the source argument
// they would share one folder and interleave their run history.
func TestEnqueueStampsPerJobSource(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at)VALUES ('uid-'||'deploy', 'deploy','git','bash','Allow',1,'t')`); err != nil {
		t.Fatalf("seed git job: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled)VALUES ('uid-ama-deploy', 'deploy','cronomicon','bash','Allow',1)`); err != nil {
		t.Fatalf("seed cronomicon job: %v", err)
	}
	gitCode, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "deploy", "uid-deploy")
	if err != nil {
		t.Fatalf("allocate git: %v", err)
	}
	amaCode, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "cronomicon", "deploy", "uid-ama-deploy")
	if err != nil {
		t.Fatalf("allocate cronomicon: %v", err)
	}
	if gitCode == amaCode {
		t.Fatalf("registry handed both sources the same code %q", gitCode)
	}

	gitTrace, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "deploy", JobSource: "git", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue git run: %v", err)
	}
	amaTrace, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
		JobName: "deploy", JobSource: "cronomicon", RunType: "bash", TriggerKind: "manual", TriggeredBy: "tester",
	})
	if err != nil {
		t.Fatalf("enqueue cronomicon run: %v", err)
	}
	if got, _ := runEntityCode(t, pool, gitTrace); got != gitCode {
		t.Errorf("git run entity_code = %q, want %q", got, gitCode)
	}
	if got, _ := runEntityCode(t, pool, amaTrace); got != amaCode {
		t.Errorf("cronomicon run entity_code = %q, want %q", got, amaCode)
	}
}
