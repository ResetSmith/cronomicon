package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// 2.5.0 C11, found by C11's re-review (defect 76): the Workflows LIST's last run was
// still by name (the detail was fixed in C11.2).
func TestC11_TheWorkflowListsLastRunIsThisWorkflows(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-df', 'dupflow', 'cronomicon', 'ag:FIN', '[]', '2026-01-01T00:00:00Z'),
	        ('w-dt', 'dupflow', 'cronomicon', 'ag:TAX', '[]', '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id) VALUES
	        ('wr-dt', 'dupflow', 'cronomicon', 'w-dt', 'failure', 'x', 'manual', '2026-03-07T07:07:07Z', 'ag:TAX')`)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/workflows?q=dupflow", gFinViewer, "")
	if rec.Code != 200 {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "2026-03-07T07:07:07Z") {
		t.Errorf("FIN's workflow list shows TAX's run of the same name: %s", rec.Body)
	}
	fin := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = ?`, "w-df")
	d := gateReq(t, h, http.MethodGet, "/api/v1/workflows/"+fin, gFinViewer, "")
	if strings.Contains(d.Body.String(), "2026-03-07T07:07:07Z") {
		t.Errorf("detail shows TAX's run: %s", d.Body)
	}
}

// 2.5.0 C11, found by C11's re-review (defect 79): the composer resolved a name-only
// step across agencies, so FIN's own workflow whose step names a job TAX also
// holds was refused its save, delete and re-pin with a 403 naming TAX's scope.
func TestC11_TheComposerResolvesAStepInItsWorkflowsAgency(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	// TAX's first (lower rowid), then FIN's.
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-dt', 'dup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-df', 'dup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-pre', 'preflow', 'cronomicon', 'ag:FIN', '[{"type":"job","name":"dup"}]', '2026-01-01T00:00:00Z')`)
	wf := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = ?`, "w-pre")

	// Engine-consistent read gates (Home): visible to FIN's viewer.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/workflow-tags/"+wf, gFinViewer, `{"tags":[]}`); rec.Code != 200 {
		t.Errorf("FIN viewer tags FIN's workflow = %d %s", rec.Code, rec.Body)
	}
	// Trigger (engine-consistent walk).
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows/"+wf+"/trigger", gFinAdmin, `{}`); rec.Code/100 != 2 {
		t.Errorf("FIN admin triggers FIN's workflow = %d %s", rec.Code, rec.Body)
	}
	// Edit the pre-2.4 graph: the replaced graph is resolved by stepJobScope.
	rec := gateReq(t, h, http.MethodPut, "/api/v1/workflows/"+wf, gFinAdmin, `{"steps":[{"type":"job","name":"dup","jobUid":"j-df"}]}`)
	t.Logf("FIN admin edit FIN's pre-2.4 workflow = %d %s", rec.Code, rec.Body)
	if rec.Code/100 != 2 {
		t.Errorf("FIN admin edit of FIN's own workflow refused: %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/workflows/"+wf, gFinAdmin, ""); rec.Code/100 != 2 {
		t.Errorf("FIN admin delete of FIN's own workflow refused: %d %s", rec.Code, rec.Body)
	}
	// A new API save with a name-only step.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"apiflow","steps":[{"type":"job","name":"dup"}]}`)
	t.Logf("FIN admin API create name-only = %d %s", rec.Code, rec.Body)
	if rec.Code/100 != 2 {
		t.Errorf("FIN admin API create with name-only step on FIN's own job refused: %d %s", rec.Code, rec.Body)
	}
}
