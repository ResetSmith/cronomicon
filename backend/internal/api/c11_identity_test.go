package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
)

// 2.5.0 C11.2 (the pre-release review's 5 to 8): the object by its identity,
// not its name. Names are per agency since 2.4.0 and an assignment keeps the
// name, so FIN and TAX each holding a job called `backup` is ordinary.

// seedNamesakes gives FIN and TAX each a job built in the app called backup,
// FIN's run older and running, TAX's newer and queued with a reason that names
// TAX's runner.
func seedNamesakes(t *testing.T, exec func(string, ...any)) {
	t.Helper()
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, enabled, created_at) VALUES
	        ('j-bf', 'backup', 'cronomicon', 'bash', 'fin-hosts', 'ag:FIN', 'true', 1, '2026-01-01T00:00:00Z'),
	        ('j-bt', 'backup', 'cronomicon', 'bash', 'tax-hosts', 'ag:TAX', 'true', 1, '2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runs (id, job_name, job_uid, job_source, run_type, status, triggered_by, trigger_kind, created_at, scope, agencies_json, queued_reason) VALUES
	        ('rb-fin', 'backup', 'j-bf', 'cronomicon', 'bash', 'running', 'x', 'manual', '2026-03-01T00:00:00Z', 'fin-hosts', '["FIN"]', NULL),
	        ('rb-tax', 'backup', 'j-bt', 'cronomicon', 'bash', 'queued',  'x', 'manual', '2026-03-02T00:00:00Z', 'tax-hosts', '["TAX"]', 'waiting for a TAX-SECRET-runner runner')`)
	exec(`INSERT INTO file_watch_sightings (id, job_source, job_name, job_uid, path, size_bytes, mtime, seen_at) VALUES
	        ('fs-tax', 'cronomicon', 'backup', 'j-bt', '/srv/tax/TAX-SECRET.csv', 1, '2026-03-02T00:00:00Z', '2026-03-02T00:00:00Z'),
	        ('fs-fin', 'cronomicon', 'backup', 'j-bf', '/srv/fin/ours.csv',       1, '2026-03-01T00:00:00Z', '2026-03-01T00:00:00Z')`)
}

func TestC11_KillStopsThisJobsRun(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedNamesakes(t, exec)
	fin := rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = ?`, "j-bf")
	status := func(id string) string {
		var s string
		if err := pool.QueryRow(`SELECT status FROM runs WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// FIN's own administrator stops FIN's run (it was refused, naming tax-hosts).
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+fin+"/kill", gFinAdmin, `{}`); rec.Code != http.StatusAccepted {
		t.Fatalf("FIN's admin stopping FIN's backup = %d, want 202 (%s)", rec.Code, rec.Body)
	}
	if status("rb-fin") != "killed" || status("rb-tax") != "queued" {
		t.Errorf("after Kill on FIN's backup: FIN's run %s, TAX's run %s; want killed and queued (the newest run of the NAME was TAX's)",
			status("rb-fin"), status("rb-tax"))
	}
}

func TestC11_ALastRunIsThisJobs(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	seedNamesakes(t, exec)
	fin := rowID(t, pool, `SELECT rowid FROM jobs WHERE uid = ?`, "j-bf")

	detail := gateReq(t, h, http.MethodGet, "/api/v1/jobs/"+fin, gFinViewer, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("FIN's viewer reading FIN's backup = %d (%s)", detail.Code, detail.Body)
	}
	var d struct {
		Status       string  `json:"status"`
		LastRunAt    *string `json:"lastRunAt"`
		QueuedReason *string `json:"queuedReason"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.LastRunAt == nil || *d.LastRunAt != "2026-03-01T00:00:00Z" || d.Status != "running" || d.QueuedReason != nil {
		t.Errorf("FIN's backup reads last run %v, status %q, queued reason %v; want FIN's run (2026-03-01, running, no reason)",
			deref(d.LastRunAt), d.Status, deref(d.QueuedReason))
	}
	if strings.Contains(detail.Body.String(), "TAX-SECRET") {
		t.Errorf("FIN's backup carries TAX's run: %s", detail.Body)
	}

	list := gateReq(t, h, http.MethodGet, "/api/v1/jobs?pageSize=200", gFinViewer, "")
	var l struct {
		Items []struct {
			UID       string  `json:"uid"`
			Status    string  `json:"status"`
			LastRunAt *string `json:"lastRunAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &l); err != nil {
		t.Fatalf("%v: %s", err, list.Body)
	}
	found := false
	for _, it := range l.Items {
		if it.UID == "j-bf" {
			found = true
			if it.LastRunAt == nil || *it.LastRunAt != "2026-03-01T00:00:00Z" || it.Status != "running" {
				t.Errorf("the list's FIN backup reads last run %v, status %q; want FIN's run", deref(it.LastRunAt), it.Status)
			}
		}
	}
	if !found {
		t.Fatalf("FIN's backup is not in FIN's viewer's list: %s", list.Body)
	}

	sightings := gateReq(t, h, http.MethodGet, "/api/v1/jobs/"+fin+"/file-sightings", gFinViewer, "")
	if sightings.Code != http.StatusOK || strings.Contains(sightings.Body.String(), "TAX-SECRET") || !strings.Contains(sightings.Body.String(), "ours.csv") {
		t.Errorf("FIN's backup's file sightings = %d %s; want FIN's only", sightings.Code, sightings.Body)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// A token resolves a name among its own agency's definitions (question 107).
func TestC11_ATokenResolvesItsOwnAgencysName(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-t', 'Tax', 't'), ('ag-f', 'Fin', 't')`)
	exec(`INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES
	        ('sc-t', 'web', 'cronomicon', 't', 'ag-t'), ('sc-f', 'web', 'cronomicon', 't', 'ag-f')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, concurrency_policy, enabled, requestable, created_at) VALUES
	        ('j-t', 'backup', 'cronomicon', 'bash', 'web', 'ag-t', 'Allow', 1, 1, 't'),
	        ('j-f', 'backup', 'cronomicon', 'bash', 'web', 'ag-f', 'Allow', 1, 1, 't')`)
	_, taxToken := mintAccount(t, ts, client, csrf, map[string]any{"name": "tax-bot", "role": "admin", "agencyId": "ag-t"})
	_, allToken := mintAccount(t, ts, client, csrf, map[string]any{"name": "all-bot", "role": "admin", "allScopes": true})
	ran := func() string {
		var uid string
		_ = pool.QueryRow(`SELECT COALESCE(job_uid, '') FROM runs WHERE job_name = 'backup' ORDER BY created_at DESC, rowid DESC LIMIT 1`).Scan(&uid)
		return uid
	}
	trigger := func(token string) (int, string) {
		resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/backup")
		defer resp.Body.Close()
		var buf bytes.Buffer
		buf.ReadFrom(resp.Body)
		return resp.StatusCode, buf.String()
	}

	// TAX's token: TAX's backup; FIN's of the name is nothing to it.
	if code, body := trigger(taxToken); code != http.StatusAccepted || ran() != "j-t" {
		t.Errorf("TAX's token triggering backup = %d %s, ran %q; want 202 and TAX's job", code, body, ran())
	}
	// A token of every agency: Global has none of the name, so the two
	// agencies' are ambiguous to it, as before.
	if code, body := trigger(allToken); code != http.StatusConflict || !strings.Contains(body, "ambiguous_name") {
		t.Errorf("the all-agencies token with two agencies' backup = %d %s; want 409 ambiguous_name", code, body)
	}
	// ...and Global's own, once there is one, first. Until 2.5.3 it ran; since
	// CA-1 Global's job runs nothing, so the name resolves to it (not to the
	// two agencies' ambiguity) and the run is refused, with nothing enqueued.
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, concurrency_policy, enabled, requestable, created_at)
	      VALUES ('j-g', 'backup', 'cronomicon', 'bash', NULL, NULL, 'Allow', 1, 1, 't')`)
	if code, body := trigger(allToken); code != http.StatusConflict || !strings.Contains(body, "run_refused") ||
		!strings.Contains(body, "built-in administration agency runs nothing") || ran() != "j-t" {
		t.Errorf("the all-agencies token with Global's backup = %d %s, ran %q; want 409 run_refused (Global's job runs nothing) and no run", code, body, ran())
	}
}

// The reaper's purger takes the uid (defect 78): two agencies' binned
// definitions of one name are purged one by one, each by its identity.
func TestC11_TheReaperPurgesNamesakesOneByOne(t *testing.T) {
	_, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, owner_agency, command, created_at, deleted_at) VALUES
	        ('j-cf', 'cleanup', 'cronomicon', 'bash', 'ag:FIN', 'true', 't', '2026-01-01T00:00:00Z'),
	        ('j-ct', 'cleanup', 'cronomicon', 'bash', 'ag:TAX', 'true', 't', '2026-01-01T00:00:00Z')`)
	left := func() int { return count(t, pool, `SELECT COUNT(*) FROM jobs WHERE name = 'cleanup'`) }

	// By name alone, as the reaper called it: refused, every night.
	if _, err := api.PurgeDefinition(context.Background(), pool, t.TempDir(), "job", "cleanup", ""); err == nil || left() != 2 {
		t.Errorf("a purge by name of two agencies' binned cleanup: err %v, %d left; want refused, both left", err, left())
	}
	if ok, err := api.PurgeDefinition(context.Background(), pool, t.TempDir(), "job", "cleanup", "j-ct"); !ok || err != nil || left() != 1 {
		t.Errorf("purging TAX's by its uid: %v %v, %d left; want it purged, FIN's left", ok, err, left())
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM jobs WHERE uid = 'j-cf'`); n != 1 {
		t.Errorf("FIN's binned cleanup went with TAX's")
	}
}
