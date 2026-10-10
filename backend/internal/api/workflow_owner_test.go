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

	// Two agencies' jobs: not for somebody who administers both, directly or
	// through a sub-workflow.
	for name, body := range map[string]string{"span-flow": spanning, "span-child-flow": viaChild} {
		if code := post(both, body, name); code != http.StatusForbidden {
			t.Errorf("an administrator of both agencies saving %s = %d, want 403", name, code)
		}
		if got := owner(name); got != "(not saved)" {
			t.Errorf("the refused workflow %s was saved, as %q's", name, got)
		}
		// A global administrator saves it, and it is Global's.
		if code := post(gRoot, body, name); code/100 != 2 {
			t.Errorf("a global administrator saving %s = %d, want 2xx", name, code)
		}
		if got := owner(name); got != "global" {
			t.Errorf("the workflow %s, whose jobs are two agencies', is %q's, want Global's", name, got)
		}
	}

	// A job with no scope is Global's, and so is a workflow that runs it; one
	// that runs nothing is Global's too, and anybody who may compose saves it.
	if code := post(gRoot, unscoped, "free-flow"); code/100 != 2 || owner("free-flow") != "global" {
		t.Errorf("a workflow of an unscoped job: %d, %q's; want saved, Global's", code, owner("free-flow"))
	}
	// An agency's job beside an unscoped one is two agencies': the unscoped
	// job is Global's, so the workflow is not that agency's.
	if code := post(gRoot, withFree, "fin-and-free-flow"); code/100 != 2 || owner("fin-and-free-flow") != "global" {
		t.Errorf("a workflow of FIN's job and an unscoped job: %d, %q's; want saved, Global's", code, owner("fin-and-free-flow"))
	}
	if code := post(gFinAdmin, empty, "empty-flow"); code/100 != 2 || owner("empty-flow") != "global" {
		t.Errorf("a workflow with no steps: %d, %q's; want saved, Global's", code, owner("empty-flow"))
	}

	// The owner follows the steps: saved again with one agency's jobs, the
	// spanning workflow is that agency's.
	id := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'span-flow'`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+id, gRoot, fmt.Sprintf(oneAgency, "span-flow")); rec.Code/100 != 2 {
		t.Fatalf("saving the workflow again with one agency's jobs = %d (%s)", rec.Code, rec.Body)
	}
	if got := owner("span-flow"); got != "ag:FIN" {
		t.Errorf("after it was saved with FIN's jobs alone the workflow is %q's, want ag:FIN's", got)
	}
	// And the other way: FIN's workflow edited to span is refused for an
	// administrator of both, and stays FIN's.
	fin := rowID(t, pool, `SELECT rowid FROM workflows WHERE name = 'fin-flow'`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+fin, both, fmt.Sprintf(spanning, "fin-flow")); rec.Code != http.StatusForbidden {
		t.Errorf("an administrator of both agencies making FIN's workflow span = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if got := owner("fin-flow"); got != "ag:FIN" {
		t.Errorf("after the refused edit the workflow is %q's", got)
	}
}
