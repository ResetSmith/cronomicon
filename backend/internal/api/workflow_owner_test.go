package api_test

import (
	"fmt"
	"net/http"
	"testing"
)

// GR-6, GR-7 (2.4.0) — a workflow built in the app belongs to the one agency
// its jobs belong to, written each time it is saved. One whose jobs are
// several agencies' is Global's, and only a global administrator saves it:
// holding compose in each of the agencies is no longer enough.
//
// Until 2.4.0 a workflow had no recorded owner, and anybody who held compose
// in each of two agencies could save a workflow that ran jobs of both.
func TestAWorkflowBelongsToTheAgencyOfItsJobs(t *testing.T) {
	h, pool := gateServer(t) // fin-hosts is FIN's, tax-hosts is TAX's
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled, created_at) VALUES
	      ('fin-job',  'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('fin-job2', 'cronomicon', 'bash', 'fin-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('tax-job',  'cronomicon', 'bash', 'tax-hosts', 1, '2026-01-01T00:00:00Z'),
	      ('free-job', 'cronomicon', 'bash', NULL,        1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (name, source, steps, enabled, created_at)
	      VALUES ('tax-child', 'cronomicon', '[{"type":"job","name":"tax-job"}]', 1, '2026-01-01T00:00:00Z')`)
	owner := func(name string) string {
		t.Helper()
		var o string
		if err := pool.QueryRow(`SELECT owner_agency FROM workflows WHERE name = ? AND source = 'cronomicon'`, name).Scan(&o); err != nil {
			return "(not saved)"
		}
		return o
	}
	both := gFinAdmin + "," + gTaxAdmin
	const (
		oneAgency = `{"name":"%s","steps":[{"type":"job","name":"fin-job"},{"type":"job","name":"fin-job2"}]}`
		spanning  = `{"name":"%s","steps":[{"type":"job","name":"fin-job"},{"type":"job","name":"tax-job"}]}`
		viaChild  = `{"name":"%s","steps":[{"type":"job","name":"fin-job"},{"type":"workflow","name":"go","workflow":"tax-child"}]}`
		unscoped  = `{"name":"%s","steps":[{"type":"job","name":"free-job"}]}`
		withFree  = `{"name":"%s","steps":[{"type":"job","name":"fin-job"},{"type":"job","name":"free-job"}]}`
		empty     = `{"name":"%s","steps":[]}`
	)
	post := func(who, body, name string) int {
		t.Helper()
		return gateReq(t, h, http.MethodPost, "/api/v1/workflows", who, fmt.Sprintf(body, name)).Code
	}

	// One agency's jobs: that agency's, saved by its own administrator.
	if code := post(gFinAdmin, oneAgency, "fin-flow"); code/100 != 2 {
		t.Fatalf("FIN's administrator saving a workflow of FIN's jobs = %d, want 2xx", code)
	}
	if got := owner("fin-flow"); got != "ag:FIN" {
		t.Errorf("a workflow of FIN's jobs is %q's, want ag:FIN's", got)
	}

	// Two agencies' jobs, directly or through a sub-workflow: refused for
	// everyone, a global administrator included (§0a, IN-8; until then the
	// global administrator saved it, and it was Global's).
	for name, body := range map[string]string{"span-flow": spanning, "span-child-flow": viaChild} {
		for who, as := range map[string]string{both: "an administrator of both agencies", gRoot: "a global administrator"} {
			if code := post(who, body, name); code != http.StatusConflict {
				t.Errorf("%s saving %s = %d, want 409", as, name, code)
			}
			if got := owner(name); got != "(not saved)" {
				t.Errorf("the refused workflow %s was saved, as %q's", name, got)
			}
		}
	}

	// A job with no scope is Global's, and so is a workflow that runs it. One
	// that runs nothing is its creator's agency's (question 52; Global's until
	// 2.4.2, whoever built it).
	if code := post(gRoot, unscoped, "free-flow"); code/100 != 2 || owner("free-flow") != "global" {
		t.Errorf("a workflow of an unscoped job: %d, %q's; want saved, Global's", code, owner("free-flow"))
	}
	// An agency's job beside an unscoped one is two agencies': the unscoped
	// job is Global's, so the workflow spans, and is refused (§0a).
	if code := post(gRoot, withFree, "fin-and-free-flow"); code != http.StatusConflict || owner("fin-and-free-flow") != "(not saved)" {
		t.Errorf("a workflow of FIN's job and an unscoped job: %d, %q's; want 409, not saved", code, owner("fin-and-free-flow"))
	}
	if code := post(gFinAdmin, empty, "empty-flow"); code/100 != 2 || owner("empty-flow") != "ag:FIN" {
		t.Errorf("a workflow with no steps: %d, %q's; want saved, FIN's", code, owner("empty-flow"))
	}

	// The owner follows the steps: saved again with one agency's jobs, the
	// spanning workflow is that agency's. (It is seeded as an upgrade leaves
	// one: nobody saves one since §0a.)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency)
	      VALUES ('wf-span', 'span-flow', 'cronomicon', '[{"type":"job","name":"fin-job"},{"type":"job","name":"tax-job"}]', 1, 't', 'global')`)
	id := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'span-flow'`)
	// Settling it on one agency changes its agency: a global administrator's
	// repair (question 46), refused without the flag.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+id, gRoot, fmt.Sprintf(oneAgency, "span-flow")); rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "repair_required" {
		t.Errorf("settling the spanning workflow without a repair = %d (%s), want 409 repair_required", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+id, gRoot, asRepair(fmt.Sprintf(oneAgency, "span-flow"))); rec.Code/100 != 2 {
		t.Fatalf("saving the workflow again with one agency's jobs = %d (%s)", rec.Code, rec.Body)
	}
	if got := owner("span-flow"); got != "ag:FIN" {
		t.Errorf("after it was saved with FIN's jobs alone the workflow is %q's, want ag:FIN's", got)
	}
	// And the other way: FIN's workflow edited to span is refused for an
	// administrator of both, and stays FIN's.
	fin := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'fin-flow'`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+fin, both, fmt.Sprintf(spanning, "fin-flow")); rec.Code != http.StatusConflict {
		t.Errorf("an administrator of both agencies making FIN's workflow span = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if got := owner("fin-flow"); got != "ag:FIN" {
		t.Errorf("after the refused edit the workflow is %q's", got)
	}
}
