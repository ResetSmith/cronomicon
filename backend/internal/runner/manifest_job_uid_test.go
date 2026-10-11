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

// GR-22: the manifest sends the agent to the repository the run was pinned to
// (runs.checkout_repo), whose commit the pin is. Until 2.4.0 it always named
// Global's URL. A run that records no repository (one queued before 2.4.0) is
// Global's, as every run then was.
func TestManifestNamesTheRepositoryTheRunWasPinnedTo(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.GitLabBaseURL = "https://gitlab.example/infra/global-defs.git"
	as := authSvc(t, svc)
	runnerID, tok := "runner-repo", "crn_run_repo"
	insertRunner(t, svc, runnerID, "repo", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES
		('repo-a', 'ag-a', 'https://gitlab.example/agency-a/defs.git', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(uid, name, source, run_type, command, script_path, project_root, synced_at)
		VALUES ('uid-proj', 'proj', 'cronomicon', 'ansible', '- hosts: all', 'scripts/proj/site.yml', 'scripts/proj', ?)`, now()); err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	run := func(repo any) string {
		t.Helper()
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, runner_id, executor,
			                 checkout_repo, checkout_sha, checkout_entry, triggered_by, trigger_kind, started_at, created_at)
			VALUES (?, 'proj', 'cronomicon', 'uid-proj', 'ansible', 'running', ?, 'runner', ?, ?, 'scripts/proj/site.yml',
			        'test', 'manual', ?, ?)`, id, runnerID, repo, sha, now(), now()); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}
	for _, c := range []struct {
		repo any
		url  string
	}{
		{"repo-a", "https://gitlab.example/agency-a/defs.git"},
		{"global", "https://gitlab.example/infra/global-defs.git"},
		{nil, "https://gitlab.example/infra/global-defs.git"},
	} {
		m := getManifest(t, svc, as, run(c.repo), tok)
		if m.Checkout == nil || m.Checkout.Repo != c.url || m.Checkout.SHA != sha {
			t.Errorf("a run pinned to the repository %v is sent to %+v, want %s at %s", c.repo, m.Checkout, c.url, sha)
		}
	}

	// A run pinned to a repository that is no longer connected has nowhere to
	// be sent, and is not sent to Global's.
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	traceID := run("repo-gone")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_checkout_repo") {
		t.Errorf("a run pinned to a repository that is gone was answered %d: %s", rec.Code, rec.Body.String())
	}
}
