package runner

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// TestManifestRefusesACheckoutRunWithNoPinnedCommit (Phase R0's item 5; in the
// released code, and reachable since a repository has a state row of its own).
// Checkout mode is chosen by the run's commit being there. A run of a project
// job queued while its repository had not been synced records its entry, its
// repository and no commit, and fell through to a body run: the agent was
// handed the entry file alone, without its tree, and ran it.
//
// The run is refused, and told why. Everything a body run needs is in place
// here, so that the refusal is the only thing that stops it.
func TestManifestRefusesACheckoutRunWithNoPinnedCommit(t *testing.T) {
	// The job and its runs are the agency's, of its repository (seedOpsRepo):
	// the built-in agency has no repository since 2.5.3. A repository's clone
	// is the directory named by its id beside Global's (gitlab.CloneDirFor).
	root := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(root, "job-definitions"))
	clone := filepath.Join(root, opsRepoID)

	svc := newTestService(t)
	seedOpsRepo(t, svc)
	as := authSvc(t, svc)
	runnerID, tok := "runner-unpinned", "crn_run_unpinned"
	insertRunner(t, svc, runnerID, "unpinned", "online", []string{"ansible", "checkout"})
	bindRunnerToken(t, svc, tok, runnerID)

	sha := gittest.CommitSynced(t, svc.db, opsRepoID, clone, map[string]string{"scripts/proj/site.yml": "- hosts: all\n"})
	if _, err := svc.db.Exec(`
		INSERT INTO jobs (uid, name, source, repo_id, run_type, script_path, project_root, synced_at)
		VALUES ('uid-proj', 'proj', 'git', '` + opsRepoID + `', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 't')`); err != nil {
		t.Fatalf("seed the job: %v", err)
	}
	run := func(repo, commit, entry any) string {
		t.Helper()
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
			                 checkout_repo, checkout_sha, checkout_entry, triggered_by, trigger_kind, started_at, created_at, agencies_json)
			VALUES (?, 'proj', 'git', 'uid-proj', 'ansible', ?, 'running', ?, 'runner', ?, ?, ?, 'test', 'manual', ?, ?, ?)`,
			id, opsScopeName, runnerID, repo, commit, entry, now(), now(), `["`+opsAgencyName+`"]`); err != nil {
			t.Fatalf("insert the run: %v", err)
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

	for what, id := range map[string]string{
		// As the run-row writer leaves it since 2.4.0 for a repository that has
		// not been synced: the repository and the entry, and no commit.
		"a run that records its repository, its entry and no commit": run(opsRepoID, nil, "scripts/proj/site.yml"),
		// As it was left before 2.4.0: the entry alone.
		"a run that records its entry and no commit": run(nil, nil, "scripts/proj/site.yml"),
		"a run whose commit is the empty string":     run(opsRepoID, "", "scripts/proj/site.yml"),
	} {
		code, answer := fetch(id)
		if code != http.StatusConflict || !strings.Contains(answer, "no_checkout_sha") || !strings.Contains(answer, "no pinned commit") {
			t.Errorf("%s = %d %s, want 409 no_checkout_sha and the reason", what, code, answer)
		}
	}

	// The same job's run with its commit is a checkout run, and a run that is
	// of no project (no entry, no repository) is a body run, as before.
	if m := getManifest(t, svc, as, run(opsRepoID, sha, "scripts/proj/site.yml"), tok); m.Checkout == nil || m.Checkout.SHA != sha {
		t.Errorf("a pinned run is not handed out as a checkout run: %+v", m.Checkout)
	}
	if m := getManifest(t, svc, as, run(sql.NullString{}, sql.NullString{}, sql.NullString{}), tok); m.Checkout != nil || m.Body != "- hosts: all\n" {
		t.Errorf("a run that records no pin at all = checkout %+v, body %q; want a body run", m.Checkout, m.Body)
	}
}
