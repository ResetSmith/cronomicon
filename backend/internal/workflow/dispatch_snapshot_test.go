package workflow_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// RR-0b / RR-0c — a workflow step's child run must carry the same dispatch-
// policy snapshot every other producer writes: requires_json (with the RA-20a
// become-file arm), checkout_sha / checkout_entry, and the job's runner_tag
// pin. claimRun and the manifest read these from the RUN row, so a column this
// INSERT omits is a policy the step does not have — before the fix a step whose
// job required vault, a collection or a runner tag was handed to any runner in
// the agency. Same shape as target_host_test.go, the v0.53.0 precedent for
// exactly this miss pattern.

type dispatchJob struct {
	name, uid    string
	requires     string // jobs.requires_json ('[]' for none)
	becomeSecret string
	runnerTag    string
	projectRoot  string
	scriptPath   string
}

func seedDispatchJob(t *testing.T, pool *sql.DB, j dispatchJob) {
	t.Helper()
	nz := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	_, err := pool.ExecContext(context.Background(), `
		INSERT INTO jobs (name, source, uid, run_type, concurrency_policy, synced_at,
		                  requires_json, become_password_secret, runner_tag, project_root, script_path)
		VALUES (?, 'git', ?, 'ansible', 'Allow', '2026-01-01T00:00:00Z', ?, ?, ?, ?, ?)
	`, j.name, j.uid, j.requires, nz(j.becomeSecret), nz(j.runnerTag), nz(j.projectRoot), nz(j.scriptPath))
	if err != nil {
		t.Fatalf("seed job %q: %v", j.name, err)
	}
}

type childRow struct {
	requires, checkoutSHA, checkoutEntry, runnerTag sql.NullString
}

func triggerAndReadChild(t *testing.T, pool *sql.DB, jobName string) childRow {
	t.Helper()
	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "wf-" + jobName,
		WorkflowID:   1,
		Steps:        []workflow.Step{{Type: "job", Name: jobName}},
		TriggeredBy:  "tester@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	var r childRow
	if err := pool.QueryRowContext(context.Background(), `
		SELECT requires_json, checkout_sha, checkout_entry, runner_tag
		  FROM runs WHERE workflow_run_id = ? AND job_name = ?`, result.TraceID, jobName,
	).Scan(&r.requires, &r.checkoutSHA, &r.checkoutEntry, &r.runnerTag); err != nil {
		t.Fatalf("fetch child run: %v", err)
	}
	return r
}

func TestRunJob_RequiresSnapshotted(t *testing.T) {
	pool := openPool(t)
	seedDispatchJob(t, pool, dispatchJob{name: "needs-vault", uid: "u-nv", requires: `["vault","collection:community.vmware"]`})
	r := triggerAndReadChild(t, pool, "needs-vault")
	if !r.requires.Valid || !strings.Contains(r.requires.String, "vault") || !strings.Contains(r.requires.String, "collection:community.vmware") {
		t.Fatalf("requires_json = %v, want the job's tokens", r.requires)
	}
}

func TestRunJob_BecomeFileInjected(t *testing.T) {
	pool := openPool(t)
	seedDispatchJob(t, pool, dispatchJob{name: "sudo-job", uid: "u-sj", requires: `[]`, becomeSecret: "vault:become"})
	r := triggerAndReadChild(t, pool, "sudo-job")
	if !r.requires.Valid || !strings.Contains(r.requires.String, "become-file") {
		t.Fatalf("requires_json = %v, want become-file (RA-20a)", r.requires)
	}
}

func TestRunJob_NoRequiresIsNull(t *testing.T) {
	pool := openPool(t)
	seedDispatchJob(t, pool, dispatchJob{name: "plain", uid: "u-pl", requires: `[]`})
	r := triggerAndReadChild(t, pool, "plain")
	if r.requires.Valid {
		t.Fatalf("requires_json = %q, want NULL so the claim predicate keeps its short-circuit", r.requires.String)
	}
	if r.checkoutSHA.Valid || r.checkoutEntry.Valid {
		t.Fatalf("body-only job must leave checkout_* NULL, got sha=%v entry=%v", r.checkoutSHA, r.checkoutEntry)
	}
}

func TestRunJob_CheckoutPinned(t *testing.T) {
	pool := openPool(t)
	if _, err := pool.Exec(`INSERT OR REPLACE INTO git_sync_state (id, last_sha) VALUES (1, 'abc123')`); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	seedDispatchJob(t, pool, dispatchJob{name: "proj-job", uid: "u-pj", requires: `[]`, projectRoot: "playbooks/site", scriptPath: "site.yml"})
	r := triggerAndReadChild(t, pool, "proj-job")
	if r.checkoutSHA.String != "abc123" || r.checkoutEntry.String != "site.yml" {
		t.Fatalf("checkout snapshot = sha:%v entry:%v, want abc123 / site.yml", r.checkoutSHA, r.checkoutEntry)
	}
}

func TestRunJob_RunnerTagInherited(t *testing.T) {
	pool := openPool(t)
	seedDispatchJob(t, pool, dispatchJob{name: "gpu-job", uid: "u-gpu", requires: `[]`, runnerTag: "gpu"})
	r := triggerAndReadChild(t, pool, "gpu-job")
	if r.runnerTag.String != "gpu" {
		t.Fatalf("runner_tag = %v, want gpu — a pinned job must stay pinned as a workflow step (RT)", r.runnerTag)
	}
}

func TestRunJob_NoRunnerTagIsNull(t *testing.T) {
	pool := openPool(t)
	seedDispatchJob(t, pool, dispatchJob{name: "anywhere", uid: "u-any", requires: `[]`})
	r := triggerAndReadChild(t, pool, "anywhere")
	if r.runnerTag.Valid {
		t.Fatalf("runner_tag = %q, want NULL for an unpinned job", r.runnerTag.String)
	}
}
