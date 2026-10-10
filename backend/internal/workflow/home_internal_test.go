package workflow

import (
	"context"
	"strings"
	"testing"
	"time"
)

// GR-16 (2.4.0) in the engine: a Git workflow's name-only steps, and its
// sub-workflows, resolve in the workflow's own repository, then in Global's,
// and never in another agency's.
//
// A Git workflow cannot pin a step to a job's identity (sync strips `jobUid`).
// Until Phase R4 a name that two repositories' jobs held was refused as
// ambiguous for every Git workflow, and a name that only ANOTHER agency's
// repository held resolved to that agency's job.

type homeFixture struct {
	t *testing.T
	e *Engine
}

func newHomeFixture(t *testing.T) *homeFixture {
	t.Helper()
	f := &homeFixture{t: t, e: identityPool(t)}
	f.exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	f.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main'), ('repo-c', 'ag-c', 'u', 'main')`)
	// A scope of each agency, and one that is Global's.
	for _, sc := range [][3]string{{"sc-b", "b-hosts", "ag-b"}, {"sc-c", "c-hosts", "ag-c"}, {"sc-g", "g-hosts", "global"}} {
		f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, sc[0], sc[1])
		f.exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, sc[0])
		f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, sc[0], sc[2])
	}
	return f
}

// lastRun is the newest run of a job name: the job it is of, its scope, status,
// stored reason and environment. ok is false when there is none within the wait.
func (f *homeFixture) lastRun(job string) (uid, scope, status, reason, env string, ok bool) {
	f.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		err := f.e.db.QueryRow(`SELECT COALESCE(job_uid,''), COALESCE(scope,''), status, COALESCE(queued_reason,''), COALESCE(env_json,'')
		                          FROM runs WHERE job_name = ? ORDER BY created_at DESC, rowid DESC LIMIT 1`, job).Scan(&uid, &scope, &status, &reason, &env)
		if err == nil {
			return uid, scope, status, reason, env, true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", "", "", "", "", false
}

func (f *homeFixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.e.db.Exec(q, args...); err != nil {
		f.t.Fatalf("seed: %v\n%s", err, q)
	}
}

// job seeds a job. repo is "" for one built in the app.
func (f *homeFixture) job(uid, name, repo, scope string) {
	f.t.Helper()
	source, repoID := "git", any(repo)
	if repo == "" {
		source, repoID = "cronomicon", nil
	}
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, scope, synced_at, repo_id)
	        VALUES (?, ?, ?, 'bash', 'echo hi', 'Allow', 1, ?, 't', ?)`, uid, name, source, scope, repoID)
}

// workflowRow seeds a Git workflow and returns its rowid.
func (f *homeFixture) workflowRow(uid, name, repo, steps string) int64 {
	f.t.Helper()
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id) VALUES (?, ?, 'git', ?, 1, 't', ?)`, uid, name, steps, repo)
	var rowid int64
	if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(&rowid); err != nil {
		f.t.Fatal(err)
	}
	return rowid
}

func TestAGitWorkflowsStepResolvesInItsRepositoryThenGlobals(t *testing.T) {
	f := newHomeFixture(t)
	ctx := context.Background()
	f.job("d-g", "deploy", "global", "g-hosts")
	f.job("d-b", "deploy", "repo-b", "b-hosts")
	f.job("d-c", "deploy", "repo-c", "c-hosts")
	f.job("og", "only-global", "global", "g-hosts")
	f.job("oc", "only-c", "repo-c", "c-hosts")
	f.job("ob", "only-b", "repo-b", "b-hosts")
	// Built in the app: one of the workflow's own agency, one of another's.
	f.job("app-b", "b-app-job", "", "b-hosts")
	f.job("app-c", "c-app-job", "", "c-hosts")
	f.job("app-g", "g-app-job", "", "g-hosts")

	resolve := func(name, stepSource, home string) (uid, unavailable string, found bool) {
		t.Helper()
		_, jd, ok := f.e.resolveJobDef(ctx, StepRef{Name: name, Source: stepSource}, "git", home)
		return jd.uid, jd.unavailable, ok
	}
	for _, c := range []struct {
		what, name, stepSource, home string
		wantUID                      string
		wantFound                    bool
	}{
		{"a name every repository has, from repo-b", "deploy", "", "repo-b", "d-b", true},
		{"the same, from repo-c", "deploy", "", "repo-c", "d-c", true},
		{"the same, from Global's", "deploy", "", "global", "d-g", true},
		{"the same, the step saying git", "deploy", "git", "repo-b", "d-b", true},
		{"a name only Global's has", "only-global", "", "repo-b", "og", true},
		{"a name only another agency's has", "only-c", "", "repo-b", "", false},
		{"the same, the step saying git", "only-c", "git", "repo-b", "", false},
		{"a name only another agency's has, from Global's", "only-b", "", "global", "", false},
		// Among the jobs built in the app, a workflow of an AGENCY's repository
		// finds its own agency's and no other's; Global's repository's finds
		// any, as it always has.
		{"the app's job of the workflow's own agency, second", "b-app-job", "", "repo-b", "app-b", true},
		{"the same, the step saying so", "b-app-job", "cronomicon", "repo-b", "app-b", true},
		{"the app's job of ANOTHER agency", "c-app-job", "", "repo-b", "", false},
		{"the same, the step saying so", "c-app-job", "cronomicon", "repo-b", "", false},
		{"the app's job that is Global's", "g-app-job", "", "repo-b", "", false},
		{"the app's job of any agency, from Global's repository", "c-app-job", "", "global", "app-c", true},
	} {
		uid, unavailable, found := resolve(c.name, c.stepSource, c.home)
		if found != c.wantFound || uid != c.wantUID || unavailable != "" {
			t.Errorf("%s: resolved to %q (found %v, %q); want %q (found %v)", c.what, uid, found, unavailable, c.wantUID, c.wantFound)
		}
	}

	// With no home the older rule stands: a name several Git jobs hold is refused.
	if uid, unavailable, found := resolve("deploy", "", ""); !found || uid != "" || !strings.Contains(unavailable, "more than one job") {
		t.Errorf("with no home, a name three repositories hold: %q, %q, found %v; want the ambiguity refusal", uid, unavailable, found)
	}
	if uid, _, found := resolve("only-c", "", ""); !found || uid != "oc" {
		t.Errorf("with no home, a name one Git job holds: %q, found %v; want that job, as before", uid, found)
	}

	// The home repository's own job stops the search even when it may not run:
	// its binned job is not quietly replaced by Global's of the same name.
	f.exec(`UPDATE jobs SET deleted_at = 't' WHERE uid = 'd-b'`)
	if uid, unavailable, found := resolve("deploy", "", "repo-b"); !found || uid != "d-b" || !strings.Contains(unavailable, "recycle bin") {
		t.Errorf("the home repository's binned job: %q, %q, found %v; want it refused, not Global's in its place", uid, unavailable, found)
	}
	f.exec(`UPDATE jobs SET deleted_at = NULL WHERE uid = 'd-b'`)

	// What is authorized is what would run: the scopes of the workflow's own
	// repository's jobs.
	steps := []Step{{Type: "job", Name: "deploy"}, {Type: "job", Name: "only-global"}}
	for home, want := range map[string]string{"repo-b": "b-hosts,g-hosts", "repo-c": "c-hosts,g-hosts", "global": "g-hosts"} {
		scopes, err := f.e.JobScopesAt(ctx, steps, "git", home)
		if err != nil {
			t.Fatal(err)
		}
		if got := joinSorted(scopes); got != want {
			t.Errorf("the scopes a workflow of %s is authorized on = %q, want %q", home, got, want)
		}
	}
}

func joinSorted(in []string) string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return strings.Join(out, ",")
}

func TestAGitWorkflowsSubWorkflowIsItsRepositorysThenGlobals(t *testing.T) {
	f := newHomeFixture(t)
	ctx := context.Background()
	f.job("d-g", "deploy", "global", "g-hosts")
	f.job("d-b", "deploy", "repo-b", "b-hosts")
	f.job("d-c", "deploy", "repo-c", "c-hosts")
	const child = `[{"type":"job","name":"deploy"}]`
	// Another agency's is the oldest row, so the older rule would pick it.
	rowC := f.workflowRow("f-c", "flow", "repo-c", child)
	rowG := f.workflowRow("f-g", "flow", "global", child)
	rowB := f.workflowRow("f-b", "flow", "repo-b", child)
	f.workflowRow("only-c", "theirs", "repo-c", child)
	rowGonly := f.workflowRow("only-g", "shared", "global", child)
	// Built in the app: another agency's (the older row), and the workflow's own.
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, owner_agency) VALUES
	        ('app-c', 'month-end', 'cronomicon', '[]', 1, 't', 'ag-c'),
	        ('app-b', 'month-end', 'cronomicon', '[]', 1, 't', 'ag-b'),
	        ('app-c2', 'c-only', 'cronomicon', '[]', 1, 't', 'ag-c')`)
	appRow := func(uid string) int64 {
		var r int64
		if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	// A workflow of the home repository that is switched off, with Global's of the name on.
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id) VALUES
	        ('off-b', 'paused', 'git', '[]', 0, 't', 'repo-b'), ('on-g', 'paused', 'git', '[]', 1, 't', 'global')`)

	for _, c := range []struct {
		what, name, home string
		wantRow          int64
		wantHome         string
	}{
		{"from repo-b", "flow", "repo-b", rowB, "repo-b"},
		{"from repo-c", "flow", "repo-c", rowC, "repo-c"},
		{"from Global's", "flow", "global", rowG, "global"},
		{"a name only Global's has, from repo-b", "shared", "repo-b", rowGonly, "global"},
		{"a name only another agency's has, from repo-b", "theirs", "repo-b", 0, ""},
		{"with no home: the oldest, as before", "flow", "", rowC, "repo-c"},
		// Built in the app: the workflow's own agency's, and no other's.
		{"the app's workflow of its own agency, from repo-b", "month-end", "repo-b", appRow("app-b"), ""},
		{"the app's workflow of another agency, from repo-b", "c-only", "repo-b", 0, ""},
		{"the app's workflow of any agency, from Global's: the oldest, as before", "month-end", "global", appRow("app-c"), ""},
		// Its own repository's workflow is switched off: the step fails; Global's
		// of the name does not run in its place.
		{"its own, switched off, with Global's on", "paused", "repo-b", 0, ""},
		{"from Global's, Global's own", "paused", "global", appRow("on-g"), "global"},
	} {
		_, _, row, home, ok := f.e.loadChildWorkflow(ctx, c.name, "git", c.home)
		if row != c.wantRow || home != c.wantHome || ok != (c.wantRow != 0) {
			t.Errorf("%s: the sub-workflow %s is row %d of %q (found %v); want row %d of %q", c.what, c.name, row, home, ok, c.wantRow, c.wantHome)
		}
	}

	// The child's steps are then its OWN repository's: a parent in repo-b whose
	// sub-workflow is Global's reaches Global's deploy, not repo-b's.
	parent := []Step{{Type: "workflow", Name: "go", Workflow: "shared"}, {Type: "workflow", Name: "go2", Workflow: "flow"}}
	scopes, err := f.e.SubWorkflowJobScopesAt(ctx, parent, "git", "repo-b")
	if err != nil {
		t.Fatal(err)
	}
	if got := joinSorted(scopes); got != "b-hosts,g-hosts" {
		t.Errorf("the scopes reached through a repo-b workflow's sub-workflows = %q, want b-hosts (its own flow) and g-hosts (Global's shared)", got)
	}
	if rowB == 0 || rowG == 0 {
		t.Fatal("fixture")
	}
}

// End to end: a workflow of an agency's repository, triggered, runs its own
// repository's job of a name that Global's and another agency's also hold.
func TestATriggeredGitWorkflowRunsItsOwnRepositorysJob(t *testing.T) {
	f := newHomeFixture(t)
	f.job("d-c", "deploy", "repo-c", "c-hosts")
	f.job("d-g", "deploy", "global", "")
	f.job("d-b", "deploy", "repo-b", "b-hosts")
	row := f.workflowRow("wf-b", "nightly", "repo-b", `[{"type":"job","name":"deploy"}]`)
	if got := f.e.Home(context.Background(), row); got != "repo-b" {
		t.Fatalf("the workflow's home is %q, want repo-b", got)
	}

	if _, err := f.e.Trigger(context.Background(), TriggerParams{
		WorkflowName: "nightly", WorkflowSource: "git", WorkflowID: row, TriggeredBy: "t@example.com",
		Steps: []Step{{Type: "job", Name: "deploy"}},
	}); err != nil {
		t.Fatalf("Trigger: %v", err)
	}
	var uid, reason string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := f.e.db.QueryRow(`SELECT COALESCE(job_uid,''), COALESCE(queued_reason,'') FROM runs WHERE job_name = 'deploy' ORDER BY created_at DESC LIMIT 1`).Scan(&uid, &reason); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if uid != "d-b" {
		t.Errorf("the step's run is of the job %q (reason %q); want the workflow's own repository's, d-b", uid, reason)
	}
	if strings.Contains(reason, "more than one job") {
		t.Errorf("the step was refused as ambiguous: %s", reason)
	}
}

// A workflow built in the app has no home, and neither does a row that is not there.
func TestHomeIsAGitWorkflowsRepository(t *testing.T) {
	f := newHomeFixture(t)
	git := f.workflowRow("wf-b", "nightly", "repo-b", `[]`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at) VALUES ('wf-old', 'old', 'git', '[]', 1, 't')`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES ('wf-app', 'built', 'cronomicon', '[]', 1, 't')`)
	rowOf := func(uid string) int64 {
		var r int64
		if err := f.e.db.QueryRow(`SELECT rowid FROM workflows WHERE uid = ?`, uid).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	ctx := context.Background()
	for what, c := range map[string]struct {
		row  int64
		want string
	}{
		"an agency's repository's workflow":         {git, "repo-b"},
		"a Git workflow that records no repository": {rowOf("wf-old"), "global"},
		"a workflow built in the app":               {rowOf("wf-app"), ""},
		"no such workflow":                          {99999, ""},
	} {
		if got := f.e.Home(ctx, c.row); got != c.want {
			t.Errorf("%s: home %q, want %q", what, got, c.want)
		}
	}
}
