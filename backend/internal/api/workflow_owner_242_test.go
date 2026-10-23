package api_test

import (
	"net/http"
	"testing"
)

// Present defect 52 (found by the review of 2.4.x): a save that changes a
// workflow's agency asked for a repair only when the stored graph's jobs still
// resolved. Binning FIN's jobs first (any FIN composer may) made FIN's workflow
// TAX's for an administrator of both, with no repair and no audit row.
func TestRV_BinningAWorkflowsJobsDoesNotMakeItsAgencyFree(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	both := gFinAdmin + "," + gTaxAdmin
	req := func(who, method, path, body string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, body).Code
	}
	for _, j := range []struct{ who, name, scope string }{
		{gFinAdmin, "fin-a", "fin-hosts"}, {gTaxAdmin, "tax-a", "tax-hosts"},
	} {
		if c := req(j.who, http.MethodPost, "/api/v1/jobs", `{"name":"`+j.name+`","scriptRef":"backup-db","scope":"`+j.scope+`"}`); c/100 != 2 {
			t.Fatalf("creating %s = %d", j.name, c)
		}
	}
	if c := req(gFinAdmin, http.MethodPost, "/api/v1/workflows", `{"name":"nightly-flow","steps":[{"type":"job","name":"fin-a"}]}`); c/100 != 2 {
		t.Fatalf("FIN's workflow = %d", c)
	}
	finA := rowID(t, pool, `SELECT rowid FROM jobs WHERE name = 'fin-a'`)
	if c := req(gFinAdmin, http.MethodDelete, "/api/v1/jobs/"+finA, ""); c/100 != 2 {
		t.Fatalf("FIN bins fin-a = %d", c)
	}
	wf := "/api/v1/workflows/" + rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'nightly-flow'`)
	onTax := `{"name":"nightly-flow","steps":[{"type":"job","name":"tax-a"}]}`
	if c := req(both, http.MethodPut, wf, onTax); c != http.StatusForbidden {
		t.Errorf("an administrator of both agencies making FIN's workflow TAX's after binning its jobs = %d, want 403", c)
	}
	if c := req(gRoot, http.MethodPut, wf, onTax); c != http.StatusConflict {
		t.Errorf("a global administrator doing it without a repair = %d, want 409 repair_required", c)
	}
	var owner string
	if err := pool.QueryRow(`SELECT owner_agency FROM workflows WHERE name = 'nightly-flow'`).Scan(&owner); err != nil || owner != "ag:FIN" {
		t.Errorf("the workflow is %q's (%v), want FIN's", owner, err)
	}
}

// Question 52: a workflow with no jobs is its creator's agency's, not
// Global's. FIN may empty its own workflow and keep it; another agency's
// composer may not edit or bin FIN's empty workflow; a global administrator
// names the agency, or builds Global's.
func TestRV_AWorkflowWithNoJobsIsItsCreatorsAgencys(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)
	agenciesGetTheirOwnRepository(t, pool)
	req := func(who, method, path, body string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, body).Code
	}
	owner := func(name string) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(`SELECT owner_agency FROM workflows WHERE name = ?`, name).Scan(&o); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return o
	}
	path := func(name string) string {
		return "/api/v1/workflows/" + rowID(t, pool, `SELECT rowid FROM workflows WHERE name = '`+name+`'`)
	}
	if c := req(gFinAdmin, http.MethodPost, "/api/v1/jobs", `{"name":"fin-a","scriptRef":"backup-db","scope":"fin-hosts"}`); c/100 != 2 {
		t.Fatalf("fin-a = %d", c)
	}

	if c := req(gFinAdmin, http.MethodPost, "/api/v1/workflows", `{"name":"flow","steps":[{"type":"job","name":"fin-a"}]}`); c/100 != 2 {
		t.Fatalf("FIN's flow = %d", c)
	}
	if c := req(gFinAdmin, http.MethodPut, path("flow"), `{"name":"flow","steps":[]}`); c != http.StatusOK || owner("flow") != "ag:FIN" {
		t.Errorf("FIN emptying its own workflow = %d, %s's; want 200, FIN's", c, owner("flow"))
	}
	if c := req(gTaxAdmin, http.MethodPut, path("flow"), `{"name":"flow","steps":[],"description":"taken"}`); c != http.StatusForbidden {
		t.Errorf("TAX editing FIN's empty workflow = %d, want 403", c)
	}
	if c := req(gTaxAdmin, http.MethodDelete, path("flow"), ""); c != http.StatusForbidden {
		t.Errorf("TAX binning FIN's empty workflow = %d, want 403", c)
	}

	if c := req(gRoot, http.MethodPost, "/api/v1/workflows", `{"name":"global-draft","steps":[]}`); c/100 != 2 || owner("global-draft") != "global" {
		t.Errorf("a global administrator's empty workflow = %d, %s's; want Global's", c, owner("global-draft"))
	}
	if c := req(gRoot, http.MethodPost, "/api/v1/workflows", `{"name":"tax-draft","steps":[],"ownerAgency":"ag:TAX"}`); c/100 != 2 || owner("tax-draft") != "ag:TAX" {
		t.Errorf("a global administrator's empty workflow named for TAX = %d, %s's", c, owner("tax-draft"))
	}
	if c := req(gFinAdmin, http.MethodPost, "/api/v1/workflows", `{"name":"fin-names-tax","steps":[],"ownerAgency":"ag:TAX"}`); c != http.StatusForbidden {
		t.Errorf("FIN naming TAX for its new workflow = %d, want 403", c)
	}
	// A global administrator's draft takes the agency of its first jobs: it
	// held no step, so nothing moves.
	if c := req(gRoot, http.MethodPut, path("global-draft"), `{"name":"global-draft","steps":[{"type":"job","name":"fin-a"}]}`); c != http.StatusOK || owner("global-draft") != "ag:FIN" {
		t.Errorf("filling a global administrator's empty draft with FIN's job = %d, %s's; want 200, FIN's", c, owner("global-draft"))
	}
}
