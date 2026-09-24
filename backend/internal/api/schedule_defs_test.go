package api_test

import (
	"context"
	"testing"
)

// TestScheduleDefsCatalog exercises the read-only first-class Schedules API
// (A10a, Phase 1): the paginated list with usedByCount, the detail endpoint's
// usedBy reverse index, the source filter, and a 404.
func TestScheduleDefsCatalog(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// Two git schedules: nightly (referenced by a job + a workflow) + orphan.
	seed(`INSERT INTO schedules(name, source, description, cron, env, content_hash, source_path, synced_at)
	      VALUES('nightly','git','Every night','0 0 2 * * *','{"TIER":"prod"}','sha256:aaa','schedules/nightly.yaml','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO schedules(name, source, cron, content_hash, source_path, synced_at)
	      VALUES('orphan','git','0 0 5 * * *','sha256:bbb','schedules/orphan.yaml','2026-01-01T00:00:00Z')`)

	// A job + a workflow reference 'nightly' (ref-expanded entries carry source_ref,
	// the exact reverse index — D1c). An inline entry that merely shares the name
	// would have source_ref NULL and must NOT be counted.
	seed(`INSERT INTO jobs(name, run_type, command, content_hash, synced_at)
	      VALUES('backup','bash','echo hi','sha256:ccc','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO definition_schedules(owner_kind, owner_name, name, cron, position, source_ref)
	      VALUES('job','backup','nightly','0 0 2 * * *',0,'nightly')`)
	seed(`INSERT INTO workflows(name, steps, synced_at) VALUES('pipeline','[]','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO definition_schedules(owner_kind, owner_name, name, cron, position, source_ref)
	      VALUES('workflow','pipeline','nightly','0 0 2 * * *',0,'nightly')`)

	client := devLoginClient(t, ts)

	// ── List ────────────────────────────────────────────────────────────────────
	var list struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			Name        string            `json:"name"`
			Source      string            `json:"source"`
			Cron        string            `json:"cron"`
			Env         map[string]string `json:"env"`
			ContentHash string            `json:"contentHash"`
			UsedByCount int               `json:"usedByCount"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs", &list)
	if list.TotalItems != 2 {
		t.Fatalf("totalItems = %d, want 2", list.TotalItems)
	}
	got := map[string]int{}
	for _, it := range list.Items {
		got[it.Name] = it.UsedByCount
		if it.Source != "git" {
			t.Errorf("schedule %q source = %q, want git", it.Name, it.Source)
		}
		if it.ContentHash == "" {
			t.Errorf("schedule %q missing contentHash", it.Name)
		}
	}
	if got["nightly"] != 2 {
		t.Errorf("nightly usedByCount = %d, want 2 (job + workflow)", got["nightly"])
	}
	if got["orphan"] != 0 {
		t.Errorf("orphan usedByCount = %d, want 0", got["orphan"])
	}

	// ── Source filter ────────────────────────────────────────────────────────────
	var amadeusList struct {
		TotalItems int `json:"totalItems"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs?source=amadeus", &amadeusList)
	if amadeusList.TotalItems != 0 {
		t.Errorf("amadeus-source list = %d, want 0", amadeusList.TotalItems)
	}

	// ── Detail + usedBy ───────────────────────────────────────────────────────────
	var detail struct {
		Name   string            `json:"name"`
		Cron   string            `json:"cron"`
		Env    map[string]string `json:"env"`
		UsedBy []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"usedBy"`
	}
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs/nightly", &detail)
	if detail.Cron != "0 0 2 * * *" {
		t.Errorf("nightly cron = %q", detail.Cron)
	}
	if detail.Env["TIER"] != "prod" {
		t.Errorf("nightly env TIER = %q, want prod", detail.Env["TIER"])
	}
	if len(detail.UsedBy) != 2 {
		t.Fatalf("nightly usedBy = %d, want 2", len(detail.UsedBy))
	}
	kinds := map[string]string{}
	for _, u := range detail.UsedBy {
		kinds[u.Kind] = u.Name
	}
	if kinds["job"] != "backup" || kinds["workflow"] != "pipeline" {
		t.Errorf("usedBy = %+v, want job:backup + workflow:pipeline", detail.UsedBy)
	}

	// ── 404 ────────────────────────────────────────────────────────────────────────
	resp, err := client.Get(ts.URL + "/api/v1/schedule-defs/nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("missing schedule status = %d, want 404", resp.StatusCode)
	}
}
