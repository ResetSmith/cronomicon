package runner

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// TestManifestOfACheckoutRunQueuedBeforeALaterSync (present defect 23; in the
// released code, traced by reading in Phase R0). A checkout run was queued and
// pinned; before an agent took it, a sync moved the entry file, or removed the
// job. The agent's manifest fetch answered 500 "command resolution failed":
// the entry was read from the working tree, where it no longer was, although
// the commit the run is pinned to holds it.
//
// The entry is read at the run's pinned commit, and the run is handed out as
// it was queued. A run whose job has gone is refused, and told why.
func TestManifestOfACheckoutRunQueuedBeforeALaterSync(t *testing.T) {
	// The run is the agency's, pinned to its repository (seedOpsRepo): the
	// built-in agency has no repository since 2.5.3. A repository's clone is
	// the directory named by its id beside Global's (gitlab.CloneDirFor).
	root := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(root, "job-definitions"))
	clone := filepath.Join(root, opsRepoID)

	svc := newTestService(t)
	seedOpsRepo(t, svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-pinned", "crn_run_pinned"
	insertRunner(t, svc, runnerID, "pinned", "online", []string{"ansible", "bash", "checkout"})
	bindRunnerToken(t, svc, tok, runnerID)

	first := gittest.CommitObjects(t, clone, map[string]string{"scripts/proj/site.yml": "- hosts: all # first\n"}, nil)
	second := gittest.CommitObjects(t, clone, map[string]string{"scripts/proj/main.yml": "- hosts: all # second\n"}, nil)
	gittest.Synced(t, svc.db, opsRepoID, second)
	// The job as the second sync left it.
	if _, err := svc.db.Exec(`
		INSERT INTO jobs (uid, name, source, repo_id, run_type, script_path, project_root, synced_at)
		VALUES ('uid-proj', 'proj', 'git', '` + opsRepoID + `', 'ansible', 'scripts/proj/main.yml', 'scripts/proj', 't')`); err != nil {
		t.Fatalf("seed the job: %v", err)
	}
	pinned := func(jobUID, jobName, sha, entry string) string {
		t.Helper()
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
			                 checkout_repo, checkout_sha, checkout_entry, triggered_by, trigger_kind, started_at, created_at, agencies_json)
			VALUES (?, ?, 'git', ?, 'ansible', ?, 'running', ?, 'runner', ?, ?, ?, 'test', 'manual', ?, ?, ?)`,
			id, jobName, jobUID, opsScopeName, runnerID, opsRepoID, sha, entry, now(), now(), `["`+opsAgencyName+`"]`); err != nil {
			t.Fatalf("insert the checkout run: %v", err)
		}
		return id
	}
	fetch := func(id string) (int, string) {
		t.Helper()
		h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id+"/manifest", nil)
		req.SetPathValue("traceId", id)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	// Queued at the first commit; the second sync has moved its entry since.
	m := getManifest(t, svc, as, pinned("uid-proj", "proj", first, "scripts/proj/site.yml"), tok)
	if m.Checkout == nil || m.Checkout.SHA != first || m.Checkout.Entry != "scripts/proj/site.yml" {
		t.Fatalf("the run is not handed out as it was pinned: %+v", m.Checkout)
	}
	if m.Body != "- hosts: all # first\n" {
		t.Errorf("the body of a run pinned to the first commit = %q, want its entry at that commit", m.Body)
	}
	// Queued at the second.
	if m := getManifest(t, svc, as, pinned("uid-proj", "proj", second, "scripts/proj/main.yml"), tok); m.Body != "- hosts: all # second\n" {
		t.Errorf("the body of a run pinned to the second commit = %q", m.Body)
	}

	// A sync removes the job. A run of it that was already queued is refused,
	// and the answer says why; so is a run that is not a checkout run.
	gone := pinned("uid-proj", "proj", second, "scripts/proj/main.yml")
	if _, err := svc.db.Exec(`INSERT INTO jobs (uid, name, source, run_type, command, synced_at) VALUES ('uid-plain', 'plain', 'git', 'bash', 'true', 't')`); err != nil {
		t.Fatal(err)
	}
	plain := db.NewTraceID()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, runner_id, executor, triggered_by, trigger_kind, started_at, created_at)
		VALUES (?, 'plain', 'git', 'uid-plain', 'bash', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
		plain, runnerID, now(), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`DELETE FROM jobs WHERE uid IN ('uid-proj', 'uid-plain')`); err != nil {
		t.Fatal(err)
	}
	for what, id := range map[string]string{"a checkout run": gone, "a body run": plain} {
		code, answer := fetch(id)
		if code != http.StatusConflict || !strings.Contains(answer, "job_gone") || !strings.Contains(answer, "no longer exists") {
			t.Errorf("%s whose job has gone = %d %s, want 409 job_gone and the reason", what, code, answer)
		}
	}
}
