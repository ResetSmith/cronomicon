package workflow_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// ─── PS-1b: every job node must be reachable by the step walkers ──────────────
//
// PS-1 (v0.55.17) taught the engine that a parallel arm may be a serial chain,
// but it only taught SOME of the walkers. collectStepRefs — the walk that feeds
// lookupJobDefs — kept reading a parallel arm's own name instead of recursing,
// and had no sequence case at all. Two consequences, both silent:
//
//   - a job inside a sequence arm never reached lookupJobDefs, so runJob fell
//     back to a synthetic jobDef{runType:"bash", scope:<workflow's>} and ran the
//     step as bash under the wrong scope, ignoring its real run_type,
//     target_host, ssh identity, timeout, retries and concurrency key;
//   - the same absence kept it out of Engine.JobScopes, so workflowScopesPermit
//     authorized trigger/patch/cancel without ever consulting that job's scope —
//     an RBAC hole, not just a dispatch bug.
//
// The same walkers also switched on Step.Type raw while ValidateSteps legislates
// that an omitted type means "job", so a bare {name: deploy} validated fine, got
// no node ID and was silently skipped at execution. stepKind() now resolves that
// in one place. These tests pin both halves through exported surfaces.

func seedJobFull(t *testing.T, pool *sql.DB, name, runType, scope string) {
	t.Helper()
	_, err := pool.ExecContext(context.Background(), `
		INSERT OR IGNORE INTO jobs (name, run_type, scope, concurrency_policy, synced_at)
		VALUES (?, ?, ?, 'Allow', '2026-01-01T00:00:00Z')
	`, name, runType, scope)
	if err != nil {
		t.Fatalf("seed job %q: %v", name, err)
	}
}

// TestSequenceArmJob_ResolvesItsOwnJobDef is the dispatch half of the defect: the
// job inside a sequence arm must run as ITS OWN run_type and scope, not the
// bash/parent-scope fallback runJob applies when jobDefs misses.
func TestSequenceArmJob_ResolvesItsOwnJobDef(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "chain-head", "python", "prod")
	seedJobFull(t, pool, "chain-tail", "powershell", "prod")
	seedJobFull(t, pool, "solo", "bash", "staging")

	steps := []workflow.Step{
		{Type: "parallel", Label: "arms", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{
				{Type: "job", Name: "chain-head"},
				{Type: "job", Name: "chain-tail"},
			}},
			{Type: "job", Name: "solo"},
		}},
	}
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	eng := workflow.New(pool, discardLog())
	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "ps1b-wf", WorkflowID: 1, Steps: steps,
		Scope: "workflow-scope", TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, result.TraceID, func(string, int) string { return "success" })
	defer stop()

	if status, _ := waitWorkflowTerminal(t, pool, result.TraceID); status != "success" {
		t.Fatalf("workflow status = %q, want success", status)
	}

	want := map[string]struct{ runType, scope string }{
		"chain-head": {"python", "prod"},
		"chain-tail": {"powershell", "prod"},
		"solo":       {"bash", "staging"},
	}
	rows, err := pool.Query(
		`SELECT job_name, run_type, COALESCE(scope,'') FROM runs WHERE workflow_run_id = ?`, result.TraceID)
	if err != nil {
		t.Fatalf("query child runs: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var name, runType, scope string
		if err := rows.Scan(&name, &runType, &scope); err != nil {
			t.Fatalf("scan: %v", err)
		}
		w, ok := want[name]
		if !ok {
			t.Errorf("unexpected child run %q", name)
			continue
		}
		seen[name] = true
		if runType != w.runType {
			t.Errorf("%s run_type = %q, want %q (the bash fallback means jobDefs missed it)", name, runType, w.runType)
		}
		if scope != w.scope {
			t.Errorf("%s scope = %q, want %q (the workflow's scope means jobDefs missed it)", name, scope, w.scope)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("no child run for %q", name)
		}
	}
}

// TestJobScopes_IncludesSequenceArmJobs is the authorization half: a scope that
// never reaches JobScopes is a scope workflowScopesPermit never checks.
func TestJobScopes_IncludesSequenceArmJobs(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "arm-job", "bash", "restricted")
	seedJobFull(t, pool, "leaf-job", "bash", "ordinary")

	steps := []workflow.Step{
		{Type: "parallel", Jobs: []workflow.Step{
			{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "arm-job"}}},
			{Type: "job", Name: "leaf-job"},
		}},
	}
	eng := workflow.New(pool, discardLog())
	scopes, err := eng.JobScopes(context.Background(), steps, "git")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	assertHasScopes(t, scopes, "restricted", "ordinary")
}

// TestJobScopes_IncludesStandaloneSequenceJobs covers the top-level sequence —
// the same walk gap, reached without a parallel wrapper.
func TestJobScopes_IncludesStandaloneSequenceJobs(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "seq-job", "bash", "restricted")

	steps := []workflow.Step{
		{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "seq-job"}}},
	}
	eng := workflow.New(pool, discardLog())
	scopes, err := eng.JobScopes(context.Background(), steps, "git")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	assertHasScopes(t, scopes, "restricted")
}

// TestJobScopes_SequenceInsideBranchArm nests the sequence one level deeper, so
// the fix is a genuine recursion rather than a single added case.
func TestJobScopes_SequenceInsideBranchArm(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "gate", "bash", "ordinary")
	seedJobFull(t, pool, "deep-job", "bash", "restricted")

	steps := []workflow.Step{
		{Type: "job", Name: "gate"},
		{Type: "branch",
			Condition: &workflow.Condition{Type: "job_status", JobRef: "gate"},
			Pass: &workflow.Branch{Steps: []workflow.Step{
				{Type: "parallel", Jobs: []workflow.Step{
					{Type: "sequence", Steps: []workflow.Step{{Type: "job", Name: "deep-job"}}},
				}},
			}},
			Fail: &workflow.Branch{Steps: []workflow.Step{}},
		},
	}
	eng := workflow.New(pool, discardLog())
	scopes, err := eng.JobScopes(context.Background(), steps, "git")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	assertHasScopes(t, scopes, "restricted", "ordinary")
}

// TestUntypedStep_IsAJobEverywhere pins stepKind: ValidateSteps already accepts a
// bare {name: …} as a job, so the walkers must agree — otherwise the step gets no
// node ID, no job def, and never executes.
func TestUntypedStep_IsAJobEverywhere(t *testing.T) {
	pool := openPool(t)
	seedJobFull(t, pool, "bare", "python", "restricted")

	steps := []workflow.Step{{Name: "bare"}} // no Type — legal per ValidateSteps
	if errs := workflow.ValidateSteps(steps); len(errs) != 0 {
		t.Fatalf("ValidateSteps: %+v", errs)
	}

	if got := workflow.FlattenSteps(steps); len(got) != 1 || got[0] != "bare" {
		t.Errorf("FlattenSteps = %v, want [bare]", got)
	}

	eng := workflow.New(pool, discardLog())
	scopes, err := eng.JobScopes(context.Background(), steps, "git")
	if err != nil {
		t.Fatalf("JobScopes: %v", err)
	}
	assertHasScopes(t, scopes, "restricted")

	result, err := eng.Trigger(context.Background(), workflow.TriggerParams{
		WorkflowName: "untyped-wf", WorkflowID: 1, Steps: steps, TriggeredBy: "t@example.com",
	})
	if err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	stop := simulateRuns(pool, result.TraceID, func(string, int) string { return "success" })
	defer stop()
	if status, _ := waitWorkflowTerminal(t, pool, result.TraceID); status != "success" {
		t.Fatalf("workflow status = %q, want success", status)
	}

	var n int
	var runType string
	if err := pool.QueryRow(
		`SELECT COUNT(*), COALESCE(MAX(run_type),'') FROM runs WHERE workflow_run_id = ? AND job_name = 'bare'`,
		result.TraceID).Scan(&n, &runType); err != nil {
		t.Fatalf("count child runs: %v", err)
	}
	if n != 1 {
		t.Fatalf("child runs for the untyped step = %d, want 1 (0 means walkSteps skipped it)", n)
	}
	if runType != "python" {
		t.Errorf("run_type = %q, want python", runType)
	}
}

func assertHasScopes(t *testing.T, got []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, s := range got {
		have[s] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("JobScopes = %v, missing %q", got, w)
		}
	}
}
