package runner

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
	"github.com/ResetSmith/cronomicon/internal/runref"
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
//
// Since 2.5.3 the built-in agency has no repository and the environment's
// CRONOMICON_GITLAB_BASE_URL, which was its URL, configures nothing (question
// 207): a run pinned to Global's repository, or recording none, has nowhere
// to be sent, as a run pinned to a repository that is gone has not. Two
// agencies' repositories keep the "the one it was pinned to" half.
func TestManifestNamesTheRepositoryTheRunWasPinnedTo(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.GitLabBaseURL = "https://gitlab.example/infra/global-defs.git" // ignored since 2.5.3
	as := authSvc(t, svc)
	runnerID, tok := "runner-repo", "crn_run_repo"
	insertRunner(t, svc, runnerID, "repo", "online", []string{"ansible"})
	bindRunnerToken(t, svc, tok, runnerID)
	if _, err := svc.db.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't'), ('ag-b', 'B', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES
		('repo-a', 'ag-a', 'https://gitlab.example/agency-a/defs.git', 'main'),
		('repo-b', 'ag-b', 'https://gitlab.example/agency-b/defs.git', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(uid, name, source, run_type, command, script_path, project_root, synced_at)
		VALUES ('uid-proj', 'proj', 'cronomicon', 'terraform', 'plan', 'scripts/proj/site.yml', 'scripts/proj', ?)`, now()); err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	// A run pinned to an agency's repository runs on a scope of that agency's:
	// the manifest asks that of the pin too (runref.PinOutsideRepository).
	for _, q := range []string{
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-a', 'a-hosts', 'cronomicon', 't')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-a', 'ag-a')`,
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-b', 'b-hosts', 'cronomicon', 't')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-b', 'ag-b')`,
	} {
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	run := func(repo any) string {
		t.Helper()
		id := db.NewTraceID()
		var scope any
		agencies := `["Global"]`
		// On the agency's scope for a pin to the agency's repository, and that
		// agency's run (I2: a run's scope is its agency's); a pin to Global's is a
		// Global run, on no scope (since §0a a project of Global's runs on Global's
		// scopes only; until then on any agency's).
		switch repo {
		case "repo-a", "repo-gone":
			scope = "a-hosts"
			agencies = `["A"]`
		case "repo-b":
			scope = "b-hosts"
			agencies = `["B"]`
		}
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
			                 checkout_repo, checkout_sha, checkout_entry, triggered_by, trigger_kind, started_at, created_at, agencies_json)
			VALUES (?, 'proj', 'cronomicon', 'uid-proj', 'terraform', ?, 'running', ?, 'runner', ?, ?, 'scripts/proj/site.yml',
			        'test', 'manual', ?, ?, ?)`, id, scope, runnerID, repo, sha, now(), now(), agencies); err != nil {
			t.Fatalf("insert run: %v", err)
		}
		return id
	}
	for _, c := range []struct {
		repo any
		url  string
	}{
		{"repo-a", "https://gitlab.example/agency-a/defs.git"},
		{"repo-b", "https://gitlab.example/agency-b/defs.git"},
	} {
		m := getManifest(t, svc, as, run(c.repo), tok)
		if m.Checkout == nil || m.Checkout.Repo != c.url || m.Checkout.SHA != sha {
			t.Errorf("a run pinned to the repository %v is sent to %+v, want %s at %s", c.repo, m.Checkout, c.url, sha)
		}
	}

	// A run pinned to a repository that is no longer connected has nowhere to
	// be sent, and is not sent to another. Nor, since 2.5.3, has a run pinned
	// to Global's repository or recording none: the built-in agency has no
	// repository, and the environment's URL is not read.
	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	for _, repo := range []any{"repo-gone", "global", nil} {
		traceID := run(repo)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
		req.SetPathValue("traceId", traceID)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_checkout_repo") {
			t.Errorf("a run pinned to the repository %v was answered %d: %s, want 409 no_checkout_repo", repo, rec.Code, rec.Body.String())
		}
	}

	// Not even when the built-in agency kept a connection from before 2.5.3
	// (a leftover, CA-6): it is removed, never synced or checked out from.
	if _, err := svc.db.Exec(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('global', 'global', '', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`UPDATE git_repos SET url = 'https://gitlab.example/old/global.git' WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []any{"global", nil} {
		traceID := run(repo)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
		req.SetPathValue("traceId", traceID)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "no_checkout_repo") {
			t.Errorf("with a leftover connection, a run pinned to %v was answered %d: %s, want 409 no_checkout_repo", repo, rec.Code, rec.Body.String())
		}
	}
}

// GR-22, the dispatch half, through the handler and the server's own rule for
// where a repository is cloned (gitlab.CloneDirFor): two repositories hold a
// script at the same path, and the manifest hands each run the body in its
// script's repository. It read Global's clone for every run.
func TestManifestBodyIsFromTheScriptsRepositorysClone(t *testing.T) {
	// Global's clone is the directory the variable names; any other
	// repository's is a directory named by its id beside it.
	root := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(root, "job-definitions"))
	synced := map[string]string{}
	for repo, dir := range map[string]string{"global": "job-definitions", "repo-a": "repo-a"} {
		synced[repo] = gittest.Commit(t, filepath.Join(root, dir), map[string]string{"scripts/plan.args": "plan\n-var=repository=" + repo})
	}

	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-clones", "crn_run_clones"
	insertRunner(t, svc, runnerID, "clones", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't')`,
		`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`,
		`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES
		   ('s-g', 'global', 'plan', 'terraform', 'scripts/plan.args', 'h', 't'),
		   ('s-a', 'repo-a', 'plan', 'terraform', 'scripts/plan.args', 'h', 't')`,
		// The jobs are an agency's repository's, so they run on a scope of that
		// agency's (GR-15, which the manifest asks).
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-a', 'a-hosts', 'cronomicon', 't')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-a', 'ag-a')`,
		`INSERT INTO jobs (uid, name, source, repo_id, scope, run_type, script_uid, script_path, synced_at) VALUES
		   ('j-own',     'own',     'git', 'repo-a', 'a-hosts', 'terraform', 's-a', 'scripts/plan.args', 't'),
		   ('j-borrows', 'borrows', 'git', 'repo-a', 'a-hosts', 'terraform', 's-g', 'scripts/plan.args', 't')`,
	} {
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	for repo, sha := range synced {
		gittest.Synced(t, svc.db, repo, sha)
	}
	for uid, c := range map[string]struct{ name, repo string }{"j-own": {"own", "repo-a"}, "j-borrows": {"borrows", "global"}} {
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
			                 triggered_by, trigger_kind, started_at, created_at)
			VALUES (?, ?, 'git', ?, 'terraform', 'a-hosts', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
			id, c.name, uid, runnerID, now(), now()); err != nil {
			t.Fatalf("insert run of %s: %v", uid, err)
		}
		m := getManifest(t, svc, as, id, tok)
		if want := "plan\n-var=repository=" + c.repo; m.Body != want {
			t.Errorf("the run of %s is handed %q, want the body in the clone of %s", uid, m.Body, c.repo)
		}
	}
}

// GR-15 at the moment of dispatch. Every producer of a run refuses a job of an
// agency's repository on a scope that is not that agency's. A run made BEFORE
// its scope was given to another agency passed then: the manifest asks again,
// and does not hand it out.
func TestManifestRefusesARunWhoseScopeWasGivenAway(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-conf", "crn_run_conf"
	insertRunner(t, svc, runnerID, "conf", "online", []string{"terraform"})
	bindRunnerToken(t, svc, tok, runnerID)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	exec(`INSERT INTO jobs (uid, name, source, repo_id, scope, run_type, command, synced_at) VALUES
	      ('j-fin',    'plan',        'git', 'repo-fin', 'fin-hosts', 'terraform', 'plan -var=secret-of-finance', 't'),
	      ('j-global', 'global-plan', 'git', 'global',   'fin-hosts', 'terraform', 'plan -var=global',            't')`)
	run := func(jobUID, jobName string) string {
		t.Helper()
		id := db.NewTraceID()
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
		                       triggered_by, trigger_kind, started_at, created_at)
		      VALUES (?, ?, 'git', ?, 'terraform', 'fin-hosts', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
			id, jobName, jobUID, runnerID, now(), now())
		return id
	}
	fetch := func(traceID string) *httptest.ResponseRecorder {
		h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
		req.SetPathValue("traceId", traceID)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// On its agency's scope the run is handed out.
	if rec := fetch(run("j-fin", "plan")); rec.Code != http.StatusOK {
		t.Fatalf("the run on its agency's scope was answered %d: %s", rec.Code, rec.Body.String())
	}

	// The scope is given to another agency. A run made before that is refused,
	// in the words every producer uses, and nothing of the job is in the answer.
	queuedBefore := run("j-fin", "plan")
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	rec := fetch(queuedBefore)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), runref.CodeRepoScopeMismatch) ||
		!strings.Contains(rec.Body.String(), runref.ReasonRepoScopeMismatch) {
		t.Errorf("the run whose scope was given away was answered %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-of-finance") {
		t.Errorf("the refusal carries the job's command: %s", rec.Body.String())
	}

	// Global's repository's job on the same scope is refused too (§0a, IN-3;
	// until then it was nobody's to refuse).
	if rec := fetch(run("j-global", "global-plan")); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), runref.CodeRepoScopeMismatch) {
		t.Errorf("Global's repository's job on that scope was answered %d: %s", rec.Code, rec.Body.String())
	}
}

// Present defect 27, at the manifest: a script that cannot be read from its
// repository's objects is the run's failure, and the answer says why. It was a
// 500 that said "command resolution failed"; and the working tree is not read
// in its place.
func TestManifestRefusesAScriptThatIsNotInTheSyncedCommit(t *testing.T) {
	root := t.TempDir()
	clone := filepath.Join(root, "job-definitions")
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", clone)

	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-unread", "crn_run_unread"
	insertRunner(t, svc, runnerID, "unread", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	gittest.CommitSynced(t, svc.db, "global", clone, map[string]string{"scripts/there.sh": "echo there\n"})
	// In the working tree, and in no commit.
	if err := os.WriteFile(filepath.Join(clone, "scripts", "gone.sh"), []byte("echo only in the working tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO jobs (uid, name, source, repo_id, run_type, script_path, synced_at) VALUES
		   ('j-there', 'there', 'git', 'global', 'bash', 'scripts/there.sh', 't'),
		   ('j-gone',  'gone',  'git', 'global', 'bash', 'scripts/gone.sh',  't')`,
	} {
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	run := func(uid, name string) string {
		t.Helper()
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, runner_id, executor,
			                 triggered_by, trigger_kind, started_at, created_at)
			VALUES (?, ?, 'git', ?, 'bash', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
			id, name, uid, runnerID, now(), now()); err != nil {
			t.Fatalf("insert run of %s: %v", uid, err)
		}
		return id
	}

	if m := getManifest(t, svc, as, run("j-there", "there"), tok); m.Body != "echo there\n" {
		t.Errorf("the run of a committed script is handed %q", m.Body)
	}

	h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	id := run("j-gone", "gone")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id+"/manifest", nil)
	req.SetPathValue("traceId", id)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	answer := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(answer, "script_unreadable") {
		t.Fatalf("a script that is not in the synced commit = %d %s, want 409 script_unreadable", rec.Code, answer)
	}
	for _, want := range []string{"scripts/gone.sh", "not in commit"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the refusal does not say %q: %s", want, answer)
		}
	}
	if strings.Contains(answer, root) {
		t.Errorf("the refusal names a path on the server: %s", answer)
	}
}
