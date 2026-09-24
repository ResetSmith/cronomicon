package scheduler

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/rowgolden"
)

// RR-1 — byte-identity goldens for the three `runs` writers in this package
// (the run-row-unification plan):
//
//	W1  EnqueueRun         cron fire
//	W2  EnqueueRunWithID   manual / token / file-watch / promoted-pending
//	W3  recordSkippedFire  a suppressed scheduled fire
//
// Each golden is the COMPLETE row every column and every NULL. RR-2 will
// collapse these writers into one; these goldens are the only proof the
// refactor changed nothing. They were cut AFTER RR-0 so they describe the fixed
// behaviour (become-file injected on both queued paths).
//
// The job is seeded with every column a writer can read from it, so the
// goldens exercise every jobs-table subquery (entity_code, script_ref,
// content_hash, checkout_sha/entry, requires_json + RA-20a arm, runner_tag
// inherit). A golden cut against a bare job would pass on a writer that
// dropped any of those.
//
// Regenerate with: ROWGOLDEN_UPDATE=1 go test ./internal/scheduler -run RunRowGolden
// A golden diff is a behaviour diff — review it as such.

// goldenVolatile masks generated ids and wall-clock stamps. NULLs are kept.
var goldenVolatile = map[string]string{
	"id":           "<id>",
	"created_at":   "<ts>",
	"started_at":   "<ts>",
	"completed_at": "<ts>",
}

// seedGoldenJob writes a job carrying EVERY column the runs writers read.
func seedGoldenJob(t *testing.T, pool *sql.DB) {
	t.Helper()
	ctx := context.Background()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mustExec(`INSERT OR REPLACE INTO git_sync_state (id, last_sha) VALUES (1, 'deadbeefcafe')`)
	mustExec(`
		INSERT INTO jobs (name, source, uid, run_type, scope, target_host, concurrency_policy, synced_at,
		                  script_ref, content_hash, project_root, script_path,
		                  requires_json, become_password_secret, runner_tag, env_json)
		VALUES ('golden-job', 'git', 'uid-golden', 'ansible', 'prod', 'db-1.internal', 'Forbid', '2026-01-01T00:00:00Z',
		        'scripts/site.yml', 'sha256:0123', 'playbooks/site', 'site.yml',
		        '["vault","collection:community.vmware"]', 'vault:become', 'gpu', '{"JOB_LEVEL":"1"}')`)
	// entity_codes row so the entity_code subquery resolves to a value, not NULL.
	mustExec(`INSERT INTO entity_codes (kind, source, name, uid, created_at) VALUES ('job', 'git', 'golden-job', 'uid-golden', '2026-01-01T00:00:00Z')`)
}

// richParams populates EVERY EnqueueParams field so the golden proves each one
// lands. RunnerTag is left nil here (inherit) and set explicitly in a variant.
func richParams(triggerKind string) EnqueueParams {
	return EnqueueParams{
		JobName:        "golden-job",
		JobSource:      "git",
		JobUID:         "uid-golden",
		RunType:        "ansible",
		Scope:          "prod",
		TargetHost:     "db-1.internal",
		TriggerKind:    triggerKind,
		TriggeredBy:    "alice@example.com",
		ConcurrencyKey: "ck-golden",
		Executor:       "runner",
		ScheduleName:   "nightly",
		EnvJSON:        `{"A":"1"}`,
		OverrideJSON:   `{"env":{"B":"2"}}`,
		SSHUser:        "deploy",
		SSHCredential:  "cred-1",
		AgenciesJSON:   `["finance"]`,
		ScheduledFor:   "2026-02-01T00:00:00Z",
		Priority:       5,
		ReactionDepth:  2,
		ReactedToRunID: "run-parent",
	}
}

func TestRunRowGolden_W1_EnqueueRun(t *testing.T) {
	pool := mustPool(t)
	seedGoldenJob(t, pool)
	if err := EnqueueRun(context.Background(), pool, richParams("scheduled")); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	row := rowgolden.Snapshot(t, pool, "runs", "job_name = ?", "golden-job")
	rowgolden.Compare(t, "runrow_w1_enqueue_run", rowgolden.Normalize(row, goldenVolatile))
}

func TestRunRowGolden_W2_EnqueueRunWithID(t *testing.T) {
	pool := mustPool(t)
	seedGoldenJob(t, pool)
	id, err := EnqueueRunWithID(context.Background(), pool, richParams("manual"))
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	row := rowgolden.Snapshot(t, pool, "runs", "id = ?", id)
	rowgolden.Compare(t, "runrow_w2_enqueue_run_with_id", rowgolden.Normalize(row, goldenVolatile))
}

// W2 variant: the manual trigger's explicit-empty runner tag (RT-2 "run this
// unpinned even though the job is pinned"). The only VALUES-level difference
// between W1 and W2 today is that W2 can carry this rung; the golden pins that
// an explicit "" wins over the job's 'gpu' and lands as NULL, not ”.
func TestRunRowGolden_W2_ExplicitUnpin(t *testing.T) {
	pool := mustPool(t)
	seedGoldenJob(t, pool)
	p := richParams("manual")
	p.RunnerTag = new("")
	id, err := EnqueueRunWithID(context.Background(), pool, p)
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	row := rowgolden.Snapshot(t, pool, "runs", "id = ?", id)
	rowgolden.Compare(t, "runrow_w2_explicit_unpin", rowgolden.Normalize(row, goldenVolatile))
}

// W1 and W2 share a column list and are maintained by hand; RR-0a was the
// last time they drifted. This asserts the two rows are identical apart from
// the columns that legitimately differ by trigger kind.
func TestRunRowGolden_W1W2_Parity(t *testing.T) {
	pool := mustPool(t)
	seedGoldenJob(t, pool)
	ctx := context.Background()
	if err := EnqueueRun(ctx, pool, richParams("scheduled")); err != nil {
		t.Fatalf("EnqueueRun: %v", err)
	}
	// Both rows live in one DB, and uq_runs_active_concurrency (PP-L8) refuses a
	// second active run on the same key — so the second gets its own key, and
	// the column is dropped from the comparison (its own golden pins it).
	p2 := richParams("manual")
	p2.ConcurrencyKey = "ck-golden-2"
	id, err := EnqueueRunWithID(ctx, pool, p2)
	if err != nil {
		t.Fatalf("EnqueueRunWithID: %v", err)
	}
	w1 := rowgolden.Normalize(rowgolden.Snapshot(t, pool, "runs", "trigger_kind = ?", "scheduled"), goldenVolatile)
	w2 := rowgolden.Normalize(rowgolden.Snapshot(t, pool, "runs", "id = ?", id), goldenVolatile)
	for _, legit := range []string{"trigger_kind", "concurrency_key"} {
		delete(w1, legit)
		delete(w2, legit)
	}
	if a, b := rowgolden.Render(w1), rowgolden.Render(w2); a != b {
		t.Errorf("EnqueueRun and EnqueueRunWithID wrote different rows for the same params:\n--- W1\n%s--- W2\n%s", a, b)
	}
}

func TestRunRowGolden_W3_RecordSkippedFire(t *testing.T) {
	pool := mustPool(t)
	seedGoldenJob(t, pool)
	p := richParams("scheduled")
	wrote, err := recordSkippedFire(context.Background(), pool, p, skipRecord{
		Reason:   "suppressed by working calendar",
		Mode:     dedupeDay,
		Calendar: "holidays",
		Day:      "2026-01-15",
		Loc:      time.UTC,
		At:       "2026-01-15T09:00:00Z", // deterministic; the missed-run detector's override
	})
	if err != nil {
		t.Fatalf("recordSkippedFire: %v", err)
	}
	if !wrote {
		t.Fatal("recordSkippedFire de-duped a first skip; the golden needs a row")
	}
	row := rowgolden.Snapshot(t, pool, "runs", "job_name = ? AND status = 'skipped'", "golden-job")
	rowgolden.Compare(t, "runrow_w3_record_skipped_fire", rowgolden.Normalize(row, goldenVolatile))
}
