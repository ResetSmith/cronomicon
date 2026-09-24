package workflow

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/rowgolden"
)

// RR-1 — byte-identity goldens for the two `runs` writers in this package
// (the run-row-unification plan):
//
//	W4  Engine.runJob       every workflow step child run
//	W5  Engine.markSkipped  a step skipped by branching
//
// Companion to internal/scheduler/run_row_golden_test.go; same contract: the
// COMPLETE row, every NULL, cut after RR-0 so it describes the fixed writer
// (requires_json / checkout_* / runner_tag now present on child runs).
//
// Regenerate with: ROWGOLDEN_UPDATE=1 go test ./internal/workflow -run RunRowGolden

var goldenVolatile = map[string]string{
	"id":              "<id>",
	"created_at":      "<ts>",
	"started_at":      "<ts>",
	"completed_at":    "<ts>",
	"workflow_run_id": "<wf-id>",
}

func goldenPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "golden.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// seedGoldenJob mirrors the scheduler package's seed: every column a writer
// reads. Kept literally in step with it so the two packages' goldens describe
// the SAME job and RR-2 can compare across them.
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
		                  requires_json, become_password_secret, runner_tag, env_json,
		                  ssh_user, ssh_credential)
		VALUES ('golden-job', 'git', 'uid-golden', 'bash', 'prod', 'db-1.internal', 'Forbid', '2026-01-01T00:00:00Z',
		        'scripts/site.sh', 'sha256:0123', 'playbooks/site', 'site.yml',
		        '["vault","collection:community.vmware"]', 'vault:become', 'gpu', '{"JOB_LEVEL":"1"}',
		        'deploy', 'cred-1')`)
	mustExec(`INSERT INTO entity_codes (kind, source, name, uid, created_at) VALUES ('job', 'git', 'golden-job', 'uid-golden', '2026-01-01T00:00:00Z')`)
}

func TestRunRowGolden_W4_RunJob(t *testing.T) {
	pool := goldenPool(t)
	seedGoldenJob(t, pool)
	eng := New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), TriggerParams{
		WorkflowName:   "golden-wf",
		WorkflowSource: "git",
		WorkflowID:     1,
		Scope:          "prod",
		Steps:          []Step{{Type: "job", Name: "golden-job"}},
		TriggeredBy:    "alice@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	// The walk runs in a goroutine and inserts the child row before it starts
	// waiting on it (the target_host_test precedent).
	time.Sleep(150 * time.Millisecond)
	row := rowgolden.Snapshot(t, pool, "runs", "workflow_run_id = ? AND job_name = ?", result.TraceID, "golden-job")
	rowgolden.Compare(t, "runrow_w4_run_job", rowgolden.Normalize(row, goldenVolatile))
}

func TestRunRowGolden_W5_MarkSkipped(t *testing.T) {
	pool := goldenPool(t)
	seedGoldenJob(t, pool)
	e := New(pool, discardLog())
	if _, err := pool.Exec(`
		INSERT INTO workflow_runs (id, workflow_name, status, triggered_by, trigger_kind, created_at)
		VALUES ('wf-trace', 'golden-wf', 'running', 'alice@example.com', 'manual', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed workflow_runs: %v", err)
	}
	jobDefs := map[string]jobDef{
		"name:golden-job": {
			runType: "bash", scope: "prod", source: "git", targetHost: "db-1.internal",
			sshUser: "deploy", sshCred: "cred-1", uid: "uid-golden", concurrencyKey: "ck-golden",
		},
	}
	e.markSkipped(context.Background(), "wf-trace", "golden-wf",
		Step{Name: "golden-job", NodeID: "node-golden"}, "alice@example.com", jobDefs)
	row := rowgolden.Snapshot(t, pool, "runs", "id = ?", "node-golden")
	rowgolden.Compare(t, "runrow_w5_mark_skipped", rowgolden.Normalize(row, goldenVolatile))
}
