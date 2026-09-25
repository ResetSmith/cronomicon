package api_test

import (
	"testing"
)

// TestWorkflowRunDetailGraphAndContext exercises the Phase-3 run-detail serializer:
// WB-O3 (a graph reconstructed from the per-run snapshot with live status merged,
// plus real per-step stepType on the flat timeline) and WB-O1/D5 (a redacted
// resolved-context snapshot per step). It seeds a workflow_run with a snapshot whose
// node ids match the child runs', so the mapping is exercised end to end.
func TestWorkflowRunDetailGraphAndContext(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	const ts0 = "2026-01-01T00:00:00Z"

	// A stored secret supplies the redaction dictionary. (Plain env_vars values
	// no longer do — single-line Variables are log-visible, D7.)
	const secret = "supersecretpassword999"
	createStoredSecret(t, ts.URL, client, csrf, "DBPASS", secret)

	const wr = "wr-graph-1"
	snapshot := `[
		{"type":"job","name":"build","id":"r-build"},
		{"type":"parallel","jobs":[{"type":"job","name":"a","id":"r-a"},{"type":"job","name":"b","id":"r-b"}]},
		{"type":"branch","condition":{"type":"job_status","jobRef":"build"},
		 "pass":{"steps":[{"type":"job","name":"deploy","id":"r-deploy"}]},
		 "fail":{"steps":[{"type":"job","name":"rollback","id":"r-rollback"}]}}
	]`
	if _, err := pool.Exec(
		`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot)
		 VALUES (?, 1, 'wf', 'success', 't@example.com', 'manual', ?, ?)`,
		wr, ts0, snapshot); err != nil {
		t.Fatalf("seed workflow_run: %v", err)
	}

	seedChild := func(id, name, status, env, outputs string) {
		t.Helper()
		if _, err := pool.Exec(
			`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, workflow_run_id, env_json, outputs_json)
			 VALUES (?, ?, 'bash', ?, 't@example.com', 'workflow', ?, ?, ?, ?)`,
			id, name, status, ts0, wr, env, outputs); err != nil {
			t.Fatalf("seed child %s: %v", id, err)
		}
	}
	seedChild("r-build", "build", "success", `{"STAGE":"prod","DBPASS":"`+secret+`"}`, `{"version":"1.2.3"}`)
	seedChild("r-a", "a", "success", "", "")
	seedChild("r-b", "b", "success", "", "")
	seedChild("r-deploy", "deploy", "success", "", "")
	seedChild("r-rollback", "rollback", "skipped", "", "")

	var detail struct {
		Steps []struct {
			ID              string            `json:"id"`
			StepName        string            `json:"stepName"`
			StepType        string            `json:"stepType"`
			Status          string            `json:"status"`
			ContextSnapshot map[string]string `json:"contextSnapshot"`
		} `json:"steps"`
		Graph []struct {
			Type string                  `json:"type"`
			Name string                  `json:"name"`
			Jobs []struct{ Name string } `json:"jobs"`
			Pass []struct {
				Name, Status string
			} `json:"pass"`
			Fail []struct {
				Name, Status string
			} `json:"fail"`
			Condition *struct{ Label string } `json:"condition"`
		} `json:"graph"`
	}
	getJSON(t, client, ts.URL+"/api/v1/workflow-runs/"+wr, &detail)

	// ── WB-O3 graph: job → parallel(2) → branch(pass1/fail1 + condition) ─────────
	if len(detail.Graph) != 3 {
		t.Fatalf("graph len = %d, want 3 (%+v)", len(detail.Graph), detail.Graph)
	}
	if detail.Graph[0].Type != "job" || detail.Graph[0].Name != "build" {
		t.Errorf("graph[0] = %+v, want job/build", detail.Graph[0])
	}
	if detail.Graph[1].Type != "parallel" || len(detail.Graph[1].Jobs) != 2 {
		t.Errorf("graph[1] = %+v, want parallel with 2 jobs", detail.Graph[1])
	}
	br := detail.Graph[2]
	if br.Type != "branch" || len(br.Pass) != 1 || len(br.Fail) != 1 {
		t.Errorf("graph[2] = %+v, want branch with 1 pass + 1 fail", br)
	}
	if br.Condition == nil || br.Condition.Label == "" {
		t.Errorf("branch condition label missing: %+v", br.Condition)
	}
	if len(br.Fail) == 1 && br.Fail[0].Status != "skipped" {
		t.Errorf("untaken fail arm status = %q, want skipped", br.Fail[0].Status)
	}

	// ── WB-O3 flat steps: real stepType derived from the snapshot ────────────────
	stepType := map[string]string{}
	var build struct {
		ContextSnapshot map[string]string
	}
	for _, s := range detail.Steps {
		stepType[s.StepName] = s.StepType
		if s.StepName == "build" {
			build.ContextSnapshot = s.ContextSnapshot
		}
	}
	for name, want := range map[string]string{"build": "job", "a": "parallel", "deploy": "branch"} {
		if stepType[name] != want {
			t.Errorf("step %q stepType = %q, want %q", name, stepType[name], want)
		}
	}

	// ── WB-O1/D5 context: env + outputs merged, the secret redacted ──────────────
	if build.ContextSnapshot["STAGE"] != "prod" {
		t.Errorf("context STAGE = %q, want prod", build.ContextSnapshot["STAGE"])
	}
	if build.ContextSnapshot["version"] != "1.2.3" {
		t.Errorf("context output version = %q, want 1.2.3 (outputs merged)", build.ContextSnapshot["version"])
	}
	if build.ContextSnapshot["DBPASS"] != "[REDACTED]" {
		t.Errorf("context DBPASS = %q, want [REDACTED] (secret leaked)", build.ContextSnapshot["DBPASS"])
	}
}
