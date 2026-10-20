package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestC12_TheListsCountAndProjectThisAgencysSchedules (present defect 98,
// question 118): the jobs and workflows lists matched a row's schedule entries
// by its (source, name). Two agencies' `backup` then shared them: FIN's row
// counted TAX's entries and showed the earlier of both next fires, and a
// paused TAX `backup` hid FIN's next run. Entries are their owner's by uid.
func TestC12_TheListsCountAndProjectThisAgencysSchedules(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-sf', 'backup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '2026-01-01T00:00:00Z'),
	        ('j-st', 'backup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO workflows (uid, name, source, owner_agency, steps, created_at) VALUES
	        ('w-sf', 'nightly-flow', 'cronomicon', 'ag:FIN', '[]', '2026-01-01T00:00:00Z'),
	        ('w-st', 'nightly-flow', 'cronomicon', 'ag:TAX', '[]', '2026-01-01T00:00:00Z')`)
	// FIN fires once a year; TAX every minute, twice over.
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
	        ('cronomicon', 'job', 'backup', 'yearly', '0 3 1 1 *', 0, 'j-sf'),
	        ('cronomicon', 'job', 'backup', 'often-a', '* * * * *', 0, 'j-st'),
	        ('cronomicon', 'job', 'backup', 'often-b', '* * * * *', 1, 'j-st'),
	        ('cronomicon', 'workflow', 'nightly-flow', 'yearly', '0 3 1 1 *', 0, 'w-sf'),
	        ('cronomicon', 'workflow', 'nightly-flow', 'often', '* * * * *', 0, 'w-st')`)

	type row struct {
		ID            any     `json:"id"`
		UID           string  `json:"uid"`
		ScheduleCount int     `json:"scheduleCount"`
		NextRunAt     *string `json:"nextRunAt"`
	}
	list := func(path string) map[string]row {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, gRoot, "")
		var page struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		out := map[string]row{}
		for _, it := range page.Items {
			out[it.UID] = it
		}
		return out
	}

	jobs := list("/api/v1/jobs?q=backup&pageSize=50")
	fin, tax := jobs["j-sf"], jobs["j-st"]
	if fin.ScheduleCount != 1 || tax.ScheduleCount != 2 {
		t.Errorf("schedule counts: FIN %d, TAX %d; want 1 and 2", fin.ScheduleCount, tax.ScheduleCount)
	}
	if fin.NextRunAt == nil || tax.NextRunAt == nil || *fin.NextRunAt == *tax.NextRunAt {
		t.Errorf("next runs: FIN %v, TAX %v; FIN's is its own yearly fire, not TAX's", deref(fin.NextRunAt), deref(tax.NextRunAt))
	}
	wfs := list("/api/v1/workflows?q=nightly-flow&pageSize=50")
	if f, x := wfs["w-sf"], wfs["w-st"]; f.NextRunAt == nil || x.NextRunAt == nil || *f.NextRunAt == *x.NextRunAt {
		t.Errorf("workflow next runs: FIN %v, TAX %v; FIN's is its own", deref(f.NextRunAt), deref(x.NextRunAt))
	}

	// TAX pauses its `backup`: FIN's still projects.
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES ('cronomicon', 'job', 'backup', 'x', '2026-01-01T00:00:00Z', 'j-st')`)
	jobs = list("/api/v1/jobs?q=backup&pageSize=50")
	if jobs["j-sf"].NextRunAt == nil {
		t.Errorf("TAX's pause hid FIN's next run")
	}
	if jobs["j-st"].NextRunAt != nil {
		t.Errorf("TAX's paused job projects a next run: %s", *jobs["j-st"].NextRunAt)
	}
}
