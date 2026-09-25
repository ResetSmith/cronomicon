package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/settings"
)

// appZone resolves the zone the app schedules in, so a fixture that means
// "today" means the same day the gate does.
func appZone(t *testing.T, pool *sql.DB) *time.Location {
	t.Helper()
	gs, err := settings.GetGlobalSettings(context.Background(), pool)
	if err != nil {
		return time.Local
	}
	return settings.ResolveEffectiveTimezone(gs)
}

// FX-C — the behavioural half of the producer × gate matrix, for the two
// producers that live behind HTTP.
//
// The policy table itself is pinned in scheduler/gate_test.go. What is pinned
// HERE is that these routes actually consult it, because a correct table nobody
// calls fixes nothing — and "nobody calls it" is precisely the shape of the bug
// this band exists to close: the job trigger never read paused_jobs at all,
// while its workflow twin did, so the asymmetry read as complete.

func pauseJobRow(t *testing.T, pool *sql.DB, source, name string) {
	t.Helper()
	if _, err := pool.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES (?, 'job', ?, 'op@example.com', '2026-08-12T00:00:00Z')`, source, name); err != nil {
		t.Fatalf("pause %s: %v", name, err)
	}
}

// FX-Q1, both halves in one test because they are one decision: the human
// click overrides a pause, the machine token does not.
//
// The token route delegates to the SAME handler as the UI button, which is how
// it silently inherited an exemption nobody meant to grant it. A service
// account cannot see the confirmation dialog that makes the override a choice,
// so for it a pause is simply a pause.
func TestPausedJob_ClickOverridesButTokenDoesNot(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	jobID := createRHJob(t, ts, client, csrf, "billing")
	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "ci", "role": "admin", "allScopes": true,
	})
	// The machine route additionally requires the job to opt in (requestable).
	if _, err := pool.Exec(`UPDATE jobs SET requestable = 1 WHERE name = 'billing'`); err != nil {
		t.Fatalf("mark requestable: %v", err)
	}

	// Control: while it is live, both routes work. Whatever they answer after the
	// pause cannot then be blamed on scopes, CSRF, requestable or the role.
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/run", csrf, map[string]any{}); code >= 300 {
		t.Fatalf("control: running a LIVE job from the UI = %d: %s", code, b)
	}
	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/billing")
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		t.Fatalf("control: token-triggering a LIVE job = %d", resp.StatusCode)
	}

	pauseJobRow(t, pool, "cronomicon", "billing")

	t.Run("the human click still runs it", func(t *testing.T) {
		code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/run", csrf, map[string]any{})
		if code >= 300 {
			t.Errorf("clicking Run on a paused job = %d: %s\n"+
				"FX-Q1: pause stops automation, not the operator standing in front of it — "+
				"the UI names the pause in a confirmation first", code, b)
		}
	})

	t.Run("the machine token does not", func(t *testing.T) {
		resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/billing")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("token-triggering a PAUSED job = %d, want 409\n"+
				"a service account cannot see the confirmation that makes the override "+
				"deliberate, so it must not inherit the click's exemption", resp.StatusCode)
		}
		// The CODE, not just the status: a cap or Forbid conflict is also 409, and
		// either would satisfy a status-only assertion while the pause gate was gone.
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if body.Code != "job_paused" {
			t.Errorf("refusal code = %q, want job_paused — some OTHER 409 is not this gate", body.Code)
		}
	})
}

// The workflow trigger already refused a paused workflow before this band. It is
// asserted here anyway, as the control that keeps the job asymmetry above
// honest: if this ever starts passing a paused workflow through, the pair of
// tests says which direction drifted.
func TestPausedWorkflow_TriggerStillRefuses(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "step-one")

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "step-one"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)

	if _, err := pool.Exec(`
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at)
		VALUES ('cronomicon', 'workflow', 'release', 'op@example.com', '2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("pause workflow: %v", err)
	}

	// 409 specifically: a 404 would also be >= 400 and would mean the fixture's id
	// was wrong rather than that the pause held.
	if code, b := rhDo(t, client, http.MethodPost,
		ts.URL+"/api/v1/workflows/"+itoa(wf.ID)+"/trigger", csrf, map[string]any{}); code != http.StatusConflict {
		t.Errorf("triggering a paused workflow = %d, want 409: %s", code, b)
	}
}

// A fleet-wide calendar freeze binds the HTTP producers too (FX-Q2). Neither
// consulted calendars at all before this band: a change freeze a click could
// walk through is not a freeze.
func TestGlobalFreeze_BlocksTheManualTrigger(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "billing")

	// Control first: no freeze, the run starts.
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/run", csrf, map[string]any{}); code >= 300 {
		t.Fatalf("control: running with no freeze = %d: %s", code, b)
	}

	// A GLOBAL calendar listing today as a suppressed day — the fleet-wide freeze.
	// Today in the application zone, since that is the day the gate computes
	// (computing it in UTC is the FX-B #11 defect class, invisible on a UTC box).
	today := time.Now().In(appZone(t, pool)).Format("2006-01-02")
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/calendars", csrf, map[string]any{
		"name": "change-freeze", "global": true,
		"days": []map[string]string{{"day": today}},
	}); code != http.StatusCreated {
		t.Fatalf("seed global freeze calendar = %d: %s", code, b)
	}

	code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/run", csrf, map[string]any{})
	if code < 400 {
		t.Errorf("running during a fleet-wide freeze = %d, want a refusal: %s\n"+
			"FX-Q2: the global tier binds every producer, including a click", code, b)
	}
}

// FX-C — the workflow trigger is bound by the fleet-wide freeze too.
//
// Without this it is a ONE-CLICK BYPASS of the job gate: a frozen job refuses,
// and a workflow wrapping that same job fans out normally. The producer with the
// largest blast radius must not be the one the freeze does not bind.
func TestGlobalFreeze_BlocksTheWorkflowTrigger(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "step-one")

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "step-one"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)
	trigger := ts.URL + "/api/v1/workflows/" + itoa(wf.ID) + "/trigger"

	// Control: no freeze, it triggers.
	if code, b := rhDo(t, client, http.MethodPost, trigger, csrf, map[string]any{}); code >= 300 {
		t.Fatalf("control: triggering with no freeze = %d: %s", code, b)
	}

	today := time.Now().In(appZone(t, pool)).Format("2006-01-02")
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/calendars", csrf, map[string]any{
		"name": "change-freeze", "global": true,
		"days": []map[string]string{{"day": today}},
	}); code != http.StatusCreated {
		t.Fatalf("seed global freeze calendar = %d: %s", code, b)
	}

	if code, b := rhDo(t, client, http.MethodPost, trigger, csrf, map[string]any{}); code != http.StatusConflict {
		t.Errorf("triggering a workflow during a fleet-wide freeze = %d, want 409: %s\n"+
			"a freeze the workflow trigger walks through is a one-click bypass of the job gate", code, b)
	}

	// But PARKING one for after the freeze is allowed: promotion re-judges it.
	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	if code, b := rhDo(t, client, http.MethodPost, trigger, csrf, map[string]any{"runAt": future}); code >= 300 {
		t.Errorf("scheduling a workflow run for after the freeze = %d, want it accepted: %s\n"+
			"the freeze is re-judged at the promotion instant, so refusing here stops an "+
			"operator parking the post-freeze cutover run", code, b)
	}
}
