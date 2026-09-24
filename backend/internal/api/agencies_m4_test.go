package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAgencyM4Visibility covers the M4 visibility surfaces: the per-agency online
// runner coverage count on GET /agencies, and the read-time stuck-run signal on a
// queued runner run whose agency has no online runner.
func TestAgencyM4Visibility(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, _ := devLoginWithCSRF(t, ts)

	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a1','alpha','t')`)
	exec(`INSERT INTO agencies(id,name,created_at) VALUES('a2','beta','t')`)
	// r1 advertises the bash capability — the requirements-aware stuck-run probe
	// (Phase 4) checks run_type ∈ caps, so an eligible runner must claim the type.
	exec(`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at) VALUES('r1','m','online','["bash"]','t','t')`)
	exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES('r1','a1')`) // alpha has one online runner

	// ── Coverage counts on GET /agencies ─────────────────────────────────────────
	resp, err := client.Get(ts.URL + "/api/v1/agencies")
	if err != nil {
		t.Fatalf("GET agencies: %v", err)
	}
	var ags []struct {
		Name              string `json:"name"`
		OnlineRunnerCount int    `json:"onlineRunnerCount"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ags)
	resp.Body.Close()
	counts := map[string]int{}
	for _, a := range ags {
		counts[a.Name] = a.OnlineRunnerCount
	}
	if counts["alpha"] != 1 {
		t.Errorf("alpha onlineRunnerCount = %d, want 1", counts["alpha"])
	}
	if counts["beta"] != 0 {
		t.Errorf("beta onlineRunnerCount = %d, want 0", counts["beta"])
	}

	// ── Stuck-run signal on the run detail ────────────────────────────────────────
	// T3.6/T3.7 — the run carries an agency SET (mig. 680), materialized into
	// run_agencies (mig. 690); the scalar runs.agency was dropped in migration 700.
	exec(`INSERT INTO runs(id,job_name,run_type,scope,status,triggered_by,trigger_kind,executor,agencies_json,created_at)
	      VALUES('stuck','j','bash','prod','queued','t','manual','runner','["beta"]','t')`)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('stuck','beta')`)
	exec(`INSERT INTO runs(id,job_name,run_type,scope,status,triggered_by,trigger_kind,executor,agencies_json,created_at)
	      VALUES('ok','j','bash','prod','queued','t','manual','runner','["alpha"]','t')`)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('ok','alpha')`)

	reason := func(id string) string {
		t.Helper()
		resp, err := client.Get(ts.URL + "/api/v1/runs/" + id)
		if err != nil {
			t.Fatalf("GET run %s: %v", id, err)
		}
		var m struct {
			StatusReason string `json:"statusReason"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
		return m.StatusReason
	}
	if r := reason("stuck"); !strings.Contains(r, "beta") {
		t.Errorf("stuck run statusReason = %q, want it to name agency beta", r)
	}
	if r := reason("ok"); r != "" {
		t.Errorf("healthy run statusReason = %q, want empty (an online runner is eligible)", r)
	}
}
