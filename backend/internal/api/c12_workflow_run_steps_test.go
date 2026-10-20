package api_test

import (
	"net/http"
	"testing"
)

// TestC12_AWorkflowRunIsJudgedByItsOwnStepsJobs: a workflow run's detail and
// cancel judge the jobs the run's steps name, a pinned step by uid and a
// name-only one in the run's own agency (present defect 91). They judged every
// agency's job of the step's name, so TAX committing a `dup` on `tax-hosts`
// hid FIN's own run from FIN's operator (404) and refused its cancel with a
// 403 that named TAX's scope.
func TestC12_AWorkflowRunIsJudgedByItsOwnStepsJobs(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-df', 'dup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-f', 'finflow', 'cronomicon', 'ag:FIN', '[{"type":"job","name":"dup","jobUid":"j-df"}]', '2026-01-01T00:00:00Z')`)
	pinned := `[{"type":"job","name":"dup","jobUid":"j-df"}]`
	byName := `[{"type":"job","name":"dup"}]`
	for _, run := range []struct{ id, steps string }{
		{"wr-pinned", pinned}, {"wr-pinned-c", pinned},
		{"wr-name", byName}, {"wr-name-c", byName},
	} {
		exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id, steps_snapshot)
		      VALUES (?, 'finflow', 'cronomicon', 'w-f', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'ag:FIN', ?)`, run.id, run.steps)
	}
	// TAX's namesake, on a scope FIN's people do not read.
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-dt', 'dup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '2026-01-01T00:00:00Z')`)

	for _, id := range []string{"wr-pinned", "wr-name"} {
		if got := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/"+id, gFinOperator, ""); got.Code != http.StatusOK {
			t.Errorf("GET %s by FIN's operator, TAX holding a `dup`: %d %s, want 200", id, got.Code, got.Body)
		}
	}
	for _, id := range []string{"wr-pinned-c", "wr-name-c"} {
		if got := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/"+id+"/cancel", gFinOperator, `{}`); got.Code != http.StatusAccepted {
			t.Errorf("cancel %s by FIN's operator, TAX holding a `dup`: %d %s, want 202", id, got.Code, got.Body)
		}
	}

	// The control: TAX's own run of its `dup` stays TAX's.
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-c12-t', 'taxflow', 'cronomicon', 'ag:TAX', '[{"type":"job","name":"dup"}]', '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id, steps_snapshot)
	      VALUES ('wr-c12-tax', 'taxflow', 'cronomicon', 'w-c12-t', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'ag:TAX', '[{"type":"job","name":"dup"}]')`)
	if got := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-c12-tax", gFinOperator, ""); got.Code != http.StatusNotFound {
		t.Errorf("GET TAX's run by FIN's operator: %d, want 404", got.Code)
	}
	if got := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/wr-c12-tax/cancel", gFinOperator, `{}`); got.Code != http.StatusNotFound {
		t.Errorf("cancel TAX's run by FIN's operator: %d, want 404", got.Code)
	}
}
