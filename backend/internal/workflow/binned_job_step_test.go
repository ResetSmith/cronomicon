package workflow_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// FX-A1 — a workflow step must not run a job the catalog says is off.
//
// The recycle-bin work added `deleted_at IS NULL` to the routes that ACT on a
// definition, and v0.57.32 finished the workflow trigger routes. But a workflow
// STEP resolves its job through resolveJobDef, which filtered neither deleted_at
// nor enabled — so binning a job stopped every direct way to run it and left the
// indirect one wide open. Every producer reached it: cron, a click, a reaction, a
// file arrival, all through a workflow that was itself perfectly live.
//
// Simply failing to resolve is NOT the fix, and this is the trap: an unresolved
// name falls back to a synthetic bash def carrying the workflow's own scope, so
// a filtered-out job would still have executed — as bash, at the wrong scope,
// which is worse than the bug. The step has to refuse explicitly, and record the
// refusal on a child run, because the orchestrator accounts for its steps by
// their child runs.
//
// None of these tests run a run-simulator. That is deliberate: a step that
// reaches 'queued' has already lost, and the absence of a simulator means any
// such step would hang rather than quietly pass.

func binJob(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	if _, err := pool.Exec(`UPDATE jobs SET deleted_at = '2026-08-12T00:00:00Z' WHERE name = ?`, name); err != nil {
		t.Fatalf("bin job %q: %v", name, err)
	}
}

func disableJob(t *testing.T, pool *sql.DB, name string) {
	t.Helper()
	if _, err := pool.Exec(`UPDATE jobs SET enabled = 0 WHERE name = ?`, name); err != nil {
		t.Fatalf("disable job %q: %v", name, err)
	}
}

// stepRun returns the child run a step produced, which must exist either way:
// a skipped step would leave the workflow waiting on a run that never was.
func stepRun(t *testing.T, pool *sql.DB, jobName string) (status, queuedReason string) {
	t.Helper()
	var qr sql.NullString
	err := pool.QueryRow(
		`SELECT status, queued_reason FROM runs WHERE job_name = ? ORDER BY created_at DESC LIMIT 1`,
		jobName).Scan(&status, &qr)
	if err != nil {
		t.Fatalf("no child run row for step %q (%v) — the step vanished instead of failing, "+
			"so the workflow is accounting for a run that never existed", jobName, err)
	}
	return status, qr.String
}

func triggerOneStep(t *testing.T, pool *sql.DB, jobName string) string {
	t.Helper()
	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: jobName}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	status, _ := waitWorkflowTerminal(t, pool, res.TraceID)
	return status
}

func TestWorkflowStep_RefusesABinnedJob(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "importer")
	binJob(t, pool, "importer")

	if status := triggerOneStep(t, pool, "importer"); status == "success" {
		t.Error("the workflow succeeded on a step whose job is in the recycle bin")
	}

	status, reason := stepRun(t, pool, "importer")
	if status == "queued" || status == "running" {
		t.Fatalf("child run status = %q — the binned job was dispatched to a runner", status)
	}
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if !strings.Contains(strings.ToLower(reason), "recycle bin") {
		t.Errorf("queued_reason = %q, want it to name the recycle bin — this row is the "+
			"only place an operator learns why the step failed", reason)
	}
}

func TestWorkflowStep_RefusesADisabledJob(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "importer")
	disableJob(t, pool, "importer")

	if status := triggerOneStep(t, pool, "importer"); status == "success" {
		t.Error("the workflow succeeded on a step whose job is disabled")
	}

	status, reason := stepRun(t, pool, "importer")
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if !strings.Contains(strings.ToLower(reason), "disabled") {
		t.Errorf("queued_reason = %q, want it to name the job as disabled", reason)
	}
}

// The refusal must not swallow the job's identity: run_type and scope are read
// from the real row so the terminal child run is honest, and so that
// Engine.JobScopes — which feeds workflowScopesPermit — still authorizes
// against the job's TRUE scope rather than against a synthetic bash default.
func TestWorkflowStep_BinnedJobStillReportsItsRealScope(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "scoped-importer", "python", "finance")
	binJob(t, pool, "scoped-importer")

	eng := workflow.New(pool, discardLog())
	scopes, err := eng.JobScopes(context.Background(),
		[]workflow.Step{{Type: "job", Name: "scoped-importer"}}, "amadeus")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	var found bool
	for _, s := range scopes {
		if s == "finance" {
			found = true
		}
	}
	if !found {
		t.Errorf("JobScopes = %v, want it to include \"finance\" — a binned job that reports "+
			"no scope authorizes the workflow trigger without ever consulting the job's own scope", scopes)
	}
}

// A live job is untouched by all of the above — the guard must not have made
// every step refuse.
func TestWorkflowStep_LiveJobStillDispatches(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "importer")

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "importer"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(string, int) string { return "success" })
	defer stop()

	if status, _ := waitWorkflowTerminal(t, pool, res.TraceID); status != "success" {
		t.Errorf("workflow status = %q, want success — a live job's step must still run", status)
	}
}

// ─── FX-A1 follow-ups from the adversarial review ────────────────────────────

// seedJobAtSource seeds a job at an explicit source, so a name can exist in BOTH
// namespaces — the collision the A11 precedence rule exists to resolve.
func seedJobAtSource(t *testing.T, pool *sql.DB, name, source, runType, scope string) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, synced_at)VALUES ('uid-'||?||'-'||?, ?, ?, ?, ?, 'Allow', '2026-01-01T00:00:00Z')`, source, name, name, source, runType, scope); err != nil {
		t.Fatalf("seed job %q@%s: %v", name, source, err)
	}
}

// Binning a job must REFUSE the step, never silently substitute the same-named
// job at the other source.
//
// This is the trap in the obvious fix. Filtering `enabled=1 AND deleted_at IS
// NULL` inside the per-source probe turns the A11 precedence loop from "the
// first source where the name EXISTS" into "the first source where it is
// RUNNABLE" — so binning the amadeus job does not stop the step, it hands it the
// git job of the same name: a different script, run type, scope and agency
// snapshot, reported as success. A11 resolves on existence, so the search has to
// STOP at the barred row rather than step over it.
func TestWorkflowStep_BinnedJobDoesNotFallThroughToTheOtherSource(t *testing.T) {
	pool := openPool(t)
	seedJobAtSource(t, pool, "deploy", "amadeus", "python", "finance")
	seedJobAtSource(t, pool, "deploy", "git", "bash", "ops")
	if _, err := pool.Exec(
		`UPDATE jobs SET deleted_at = '2026-08-12T00:00:00Z' WHERE name='deploy' AND source='amadeus'`); err != nil {
		t.Fatalf("bin amadeus twin: %v", err)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "release", WorkflowSource: "amadeus", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "deploy"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	if status, _ := waitWorkflowTerminal(t, pool, res.TraceID); status == "success" {
		t.Error("the workflow succeeded — the binned step was satisfied by the other source's job")
	}

	var source, runType, scope, status string
	if err := pool.QueryRow(
		`SELECT job_source, run_type, COALESCE(scope,''), status FROM runs WHERE job_name='deploy'`).
		Scan(&source, &runType, &scope, &status); err != nil {
		t.Fatalf("no child run row: %v", err)
	}
	if status != "failure" {
		t.Errorf("child run status = %q, want failure", status)
	}
	if source == "git" || runType == "bash" || scope == "ops" {
		t.Errorf("the step ran the GIT twin (source=%q run_type=%q scope=%q) — binning the "+
			"amadeus job silently substituted a different definition instead of refusing",
			source, runType, scope)
	}

	// JobScopes must not drift to the substituted job's scope either: it feeds
	// workflowScopesPermit, so a flip here authorizes against the wrong definition.
	scopes, err := eng.JobScopes(context.Background(),
		[]workflow.Step{{Type: "job", Name: "deploy"}}, "amadeus")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	for _, sc := range scopes {
		if sc == "ops" {
			t.Errorf("JobScopes = %v — authorization moved to the other source's job", scopes)
		}
	}
}

// A job marked best-effort must not become workflow-fatal by being binned.
// The refusal def has to carry continue_on_error (and the rest of the job's
// config), not just run_type and scope.
func TestWorkflowStep_BinnedJobHonoursContinueOnError(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "notify")
	seedJob(t, pool, "after")
	if _, err := pool.Exec(
		`UPDATE jobs SET continue_on_error = 1, deleted_at = '2026-08-12T00:00:00Z' WHERE name='notify'`); err != nil {
		t.Fatalf("bin notify: %v", err)
	}

	eng := workflow.New(pool, discardLog())
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "notify"}, {Type: "job", Name: "after"}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, res.TraceID, func(string, int) string { return "success" })
	defer stop()
	waitWorkflowTerminal(t, pool, res.TraceID)

	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='after'`).Scan(&n)
	if n == 0 {
		t.Error("the walk halted at a binned continue_on_error job — binning a step the " +
			"operator marked best-effort must not become fatal to the whole workflow")
	}
}

// A catalog refusal is not transient, so it must not be retried: every attempt
// re-reads the same barred row and inserts another identical terminal run.
func TestWorkflowStep_BinnedJobIsNotRetried(t *testing.T) {
	pool := openPool(t)
	seedJob(t, pool, "importer")
	binJob(t, pool, "importer")

	eng := workflow.New(pool, discardLog())
	three := 3
	res, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "parent", WorkflowID: 1, TriggeredBy: "t@example.com",
		Steps: []workflow.Step{{Type: "job", Name: "importer", Retries: &three}},
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	waitWorkflowTerminal(t, pool, res.TraceID)

	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='importer'`).Scan(&n)
	if n != 1 {
		t.Errorf("child runs = %d, want 1 — a binned job cannot become runnable between "+
			"attempts, so retrying it just multiplies identical failures", n)
	}
}
