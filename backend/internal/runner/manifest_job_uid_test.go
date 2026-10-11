package runner

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// The agent's half of present defect 4. The manifest holds the run's job_uid
// and found the job by its name and source: a run of one of two agencies' jobs
// of a name was handed the other agency's command, and the other's timeout and
// environment passthrough list.

// seedTwin inserts a job built in the app, with what the manifest reads of it.
func seedTwin(t *testing.T, svc *Service, uid, name, runType, command string, timeout int, passthrough string) {
	t.Helper()
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(uid, name, source, run_type, command, timeout_seconds, env_passthrough, synced_at)
		VALUES (?, ?, 'cronomicon', ?, ?, ?, ?, ?)`,
		uid, name, runType, command, timeout, passthrough, now()); err != nil {
		t.Fatalf("seed job %s: %v", uid, err)
	}
}

// claimedRunOf inserts a claimed run of the job with that uid ("" for a run
// that records none).
func claimedRunOf(t *testing.T, svc *Service, jobUID, jobName, runType, runnerID string) string {
	t.Helper()
	id := db.NewTraceID()
	var uid any
	if jobUID != "" {
		uid = jobUID
	}
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, runner_id, executor,
		                 triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, ?, 'cronomicon', ?, ?, 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
		id, jobName, uid, runType, runnerID, ts, ts); err != nil {
		t.Fatalf("insert run of %s: %v", jobUID, err)
	}
	return id
}

func TestManifestIsOfTheRunsOwnJob(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-twins", "crn_run_twins"
	insertRunner(t, svc, runnerID, "twins", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)

	// Two agencies' jobs of one name. The older row is A's, which is the one a
	// lookup by name returns.
	seedTwin(t, svc, "uid-plan-a", "plan", "terraform", "plan\n-var=agency=a", 111, `["A_ONLY"]`)
	seedTwin(t, svc, "uid-plan-b", "plan", "terraform", "plan\n-var=agency=b", 222, `["B_ONLY"]`)

	for uid, want := range map[string]struct {
		body    string
		timeout int
		pass    string
	}{
		"uid-plan-a": {"plan\n-var=agency=a", 111, "A_ONLY"},
		"uid-plan-b": {"plan\n-var=agency=b", 222, "B_ONLY"},
	} {
		m := getManifest(t, svc, as, claimedRunOf(t, svc, uid, "plan", "terraform", runnerID), tok)
		if m.Body != want.body {
			t.Errorf("the run of %s is handed the body %q, want its own job's, %q", uid, m.Body, want.body)
		}
		if m.TimeoutSeconds != want.timeout {
			t.Errorf("the run of %s is handed the timeout %d, want its own job's, %d", uid, m.TimeoutSeconds, want.timeout)
		}
		if len(m.EnvPassthrough) != 1 || m.EnvPassthrough[0] != want.pass {
			t.Errorf("the run of %s is handed the passthrough list %v, want its own job's, [%s]", uid, m.EnvPassthrough, want.pass)
		}
	}

	// A run that records no uid, of a name two jobs hold, is handed neither's
	// command.
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	traceID := claimedRunOf(t, svc, "", "plan", "terraform", runnerID)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "agency=") {
		t.Errorf("a run with no uid, of a name two jobs hold, was answered %d: %s", rec.Code, rec.Body.String())
	}
}
