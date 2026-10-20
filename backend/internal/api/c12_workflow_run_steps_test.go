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

// TestC12_AChildRunThatCannotBeReadRefuses (C12.5's review): the run's child
// runs are each checked by their scope, and one that could not be read was
// left out of the list, so the detail and the cancel passed. A child on TAX's
// scope: readable, FIN's operator gets 404 and 403; unreadable (its duration
// holds text, so its scan fails), 200 and 202 until now. An unparseable step
// snapshot refuses too.
func TestC12_AChildRunThatCannotBeReadRefuses(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	for _, wr := range [][2]string{{"wr-c12-bad", "[]"}, {"wr-c12-badc", "[]"}, {"wr-c12-snap", "not json"}} {
		exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id, steps_snapshot)
		      VALUES (?, 'finflow', 'cronomicon', 'w-none', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'ag:FIN', ?)`, wr[0], wr[1])
	}
	exec(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, scope, agencies_json, workflow_run_id, duration_ms) VALUES
	      ('rc-c12-bad',  's0-tax-job', 'j-tax', 'bash', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'tax-hosts', '["TAX"]', 'wr-c12-bad', 'garbage'),
	      ('rc-c12-badc', 's0-tax-job', 'j-tax', 'bash', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'tax-hosts', '["TAX"]', 'wr-c12-badc', 'garbage')`)
	if got := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-c12-bad", gFinOperator, ""); got.Code == http.StatusOK {
		t.Errorf("GET a run whose child cannot be read: 200, want a refusal")
	}
	if got := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/wr-c12-badc/cancel", gFinOperator, `{}`); got.Code/100 == 2 {
		t.Errorf("cancel a run whose child cannot be read: %d, want a refusal", got.Code)
	}
	if got := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-c12-snap", gFinOperator, ""); got.Code == http.StatusOK {
		t.Errorf("GET a run whose step snapshot cannot be parsed: 200, want a refusal")
	}
}

// TestC12_AStepCheckThatCannotReadRefuses (C12's code review): a failed read of
// a step's job was skipped, so the scope it would have named went unchecked and
// the run's detail and cancel passed. A failed read refuses.
func TestC12_AStepCheckThatCannotReadRefuses(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id, steps_snapshot)
	      VALUES ('wr-c12-noread', 'finflow', 'cronomicon', 'w-none', 'running', 'x', 'manual', '2026-03-07T07:07:07Z', 'ag:FIN',
	              '[{"type":"job","name":"secret-step","jobUid":"j-secret"}]')`)
	// The step's job cannot be read: its table is gone from under the query.
	exec(`ALTER TABLE jobs RENAME TO jobs_unreadable`)
	if got := gateReq(t, h, http.MethodGet, "/api/v1/workflow-runs/wr-c12-noread", gFinOperator, ""); got.Code == http.StatusOK {
		t.Errorf("GET a run whose step's job cannot be read: 200, want a refusal")
	}
	if got := gateReq(t, h, http.MethodPost, "/api/v1/workflows/runs/wr-c12-noread/cancel", gFinOperator, `{}`); got.Code/100 == 2 {
		t.Errorf("cancel a run whose step's job cannot be read: %d, want a refusal", got.Code)
	}
}
