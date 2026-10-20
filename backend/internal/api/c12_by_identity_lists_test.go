package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestC12_TheListsNameThisAgencysJob (present defect 97, question 116): two
// list columns still found a job by name across every agency.
//
//   - The workflow list's agencies: a FIN workflow whose step TAX also has a
//     job of showed TAX among its agencies.
//   - History's jobId: FIN's run of `dup` linked to TAX's `dup`.
//
// A step's job is the one it pins, else its name's in the workflow's agency; a
// run's job is the one its uid names, else its name's in the run's agency.
func TestC12_TheListsNameThisAgencysJob(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	// FIN's first, TAX's last: a by-name map kept whichever it read last.
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-df', 'dup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-dt', 'dup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '2026-01-01T00:00:00Z')`)
	finJob := rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = ?`, "j-df")

	// The workflow list.
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-c12-name', 'c12-byname', 'cronomicon', 'ag:FIN', '[{"type":"job","name":"dup"}]', '2026-01-01T00:00:00Z'),
	        ('w-c12-pin',  'c12-pinned', 'cronomicon', 'ag:FIN', '[{"type":"job","name":"dup","jobUid":"j-df"}]', '2026-01-01T00:00:00Z')`)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/workflows?q=c12-", gRoot, "")
	var wl struct {
		Items []struct {
			Name     string   `json:"name"`
			Agencies []string `json:"agencies"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wl); err != nil || len(wl.Items) != 2 {
		t.Fatalf("workflow list = %d %s", rec.Code, rec.Body)
	}
	for _, it := range wl.Items {
		if len(it.Agencies) != 1 || it.Agencies[0] != "FIN" {
			t.Errorf("workflow %s: agencies %v, want [FIN]", it.Name, it.Agencies)
		}
	}

	// History: a run that records its job's uid, and one from before 1010 that does not.
	exec(`INSERT INTO runs (id, job_name, job_uid, job_source, run_type, status, triggered_by, trigger_kind, created_at, scope, agencies_json, owner_agency) VALUES
	        ('r-c12-uid',  'dup', 'j-df', 'cronomicon', 'bash', 'success', 'x', 'manual', '2026-03-07T07:07:07Z', 'fin-hosts', '["FIN"]', 'ag:FIN'),
	        ('r-c12-name', 'dup', NULL,   'cronomicon', 'bash', 'success', 'x', 'manual', '2026-03-07T07:07:08Z', 'fin-hosts', '["FIN"]', 'ag:FIN')`)
	rec = gateReq(t, h, http.MethodGet, "/api/v1/runs?pageSize=200", gRoot, "")
	var rl struct {
		Items []struct {
			ID    string `json:"traceId"`
			JobID int64  `json:"jobId"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rl); err != nil {
		t.Fatalf("runs list = %d %s", rec.Code, rec.Body)
	}
	want := map[string]bool{"r-c12-uid": true, "r-c12-name": true}
	for _, it := range rl.Items {
		if !want[it.ID] {
			continue
		}
		delete(want, it.ID)
		if got := itoa(it.JobID); got != finJob {
			t.Errorf("run %s: jobId %s, want FIN's job %s", it.ID, got, finJob)
		}
	}
	if len(want) != 0 {
		t.Errorf("runs missing from the list: %v", want)
	}
}
