package api_test

import (
	"net/http"
	"testing"
)

// 2.5.3 CA3, question 204 (the owner's answer, as recommended): once the local
// runner is an agency's, that agency's administrators review its host keys, as
// for their own agents; before, and for any other agency, they may not. The
// gates were already the runner's owner's (requireRunnerOwner); what changed is
// who the owner can be.
func TestCA3TheLocalRunnersAgencyReviewsItsHostKeys(t *testing.T) {
	h, pool := gateServer(t)
	var id string
	if err := pool.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&id); err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	keys := func(who string) int {
		t.Helper()
		return gateReq(t, h, http.MethodGet, "/api/v1/runners/"+id+"/host-keys", who, ``).Code
	}
	if code := keys(gFinAdmin); code != http.StatusForbidden {
		t.Errorf("FIN's administrator reading the unassigned local runner's host keys = %d, want 403", code)
	}
	if r := gateReq(t, h, http.MethodPut, "/api/v1/local-runner", gRoot, `{"ownerAgency":"ag:FIN"}`); r.Code != http.StatusOK {
		t.Fatalf("giving the local runner to FIN = %d %s", r.Code, r.Body)
	}
	if code := keys(gFinAdmin); code != http.StatusOK {
		t.Errorf("FIN's administrator reading FIN's local runner's host keys = %d, want 200", code)
	}
	if code := keys(gTaxAdmin); code != http.StatusForbidden {
		t.Errorf("TAX's administrator reading FIN's local runner's host keys = %d, want 403", code)
	}
	// Its switch stays a global administrator's (CA-11).
	if r := gateReq(t, h, http.MethodPut, "/api/v1/local-runner", gFinAdmin, `{"enabled":true}`); r.Code != http.StatusForbidden {
		t.Errorf("FIN's administrator turning FIN's local runner on = %d, want 403", r.Code)
	}
}

// A request is judged whole before anything is written (the review of CA3):
// one that would change the agency and fails on another field changes
// nothing. And a change of agency is refused while it runs a run, and while a
// scope is bound to it (whose binding would otherwise open the scope).
func TestCA3AChangeOfTheLocalRunnersAgencyIsJudgedWhole(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	var id string
	if err := pool.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	owner := func() string {
		t.Helper()
		var o string
		if err := pool.QueryRow(`SELECT owner_agency FROM runners WHERE id = ?`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	put := func(body string) (int, string) {
		t.Helper()
		r := gateReq(t, h, http.MethodPut, "/api/v1/local-runner", gRoot, body)
		return r.Code, errCode(r.Body.Bytes())
	}
	if code, ec := put(`{"ownerAgency":"ag:FIN","maxConcurrent":0}`); code != 422 || ec != "validation_error" {
		t.Errorf("a change of agency with a bad concurrency = %d %s, want 422 validation_error", code, ec)
	}
	if code, ec := put(`{"ownerAgency":"","enabled":true}`); code != 422 || ec != "local_runner_unassigned" {
		t.Errorf("taking it from every agency and turning it on = %d %s, want 422 local_runner_unassigned", code, ec)
	}
	if o := owner(); o != "global" {
		t.Fatalf("a refused request changed its agency to %q", o)
	}

	if code, ec := put(`{"ownerAgency":"ag:FIN"}`); code != 200 {
		t.Fatalf("giving it to FIN = %d %s", code, ec)
	}
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, runner_id)
	      VALUES ('r-on', 'x', 'bash', 'running', 'u', 'manual', 't', ?)`, id)
	if code, ec := put(`{"ownerAgency":"ag:TAX"}`); code != 409 || ec != "local_runner_busy" {
		t.Errorf("a change of agency while it runs a run = %d %s, want 409 local_runner_busy", code, ec)
	}
	exec(`UPDATE runs SET status = 'success' WHERE id = 'r-on'`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc:fin', ?, 'Local runner', 't')`, id)
	if code, ec := put(`{"ownerAgency":"ag:TAX"}`); code != 409 || ec != "local_runner_bound" {
		t.Errorf("a change of agency while a FIN scope is bound to it = %d %s, want 409 local_runner_bound", code, ec)
	}
	if o := owner(); o != "ag:FIN" {
		t.Errorf("a refused change moved it to %q", o)
	}
}
