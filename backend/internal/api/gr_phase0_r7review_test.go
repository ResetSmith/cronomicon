package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Three defects found by the review of R6's second part and R7 (2026-10-14),
// each older than the range reviewed (present defects 49, 50, 51 in the plan).
// Pinned as they were, then fixed on the owner's yes (questions 24 to 26): each
// test now holds the fix.

const r7reviewScript = `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`

func r7reviewJob(name, scope, extra string) string {
	return `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `"` + extra + `}`
}

// Present defect 49, FIXED (the owner's yes to question 24, 2026-10-14): the
// answer to a save of a job was fetched by NAME (fetchJobByName; the workflow's
// save the same), so the second agency to save a job of a name was answered
// with the FIRST agency's job: its scope, its environment, its schedule entries
// and theirs. A save is answered with the definition it wrote, by its uid.
func TestGR0_ASavesAnswerIsAnotherAgencysDefinitionOfTheName(t *testing.T) {
	h, pool := gateServer(t)
	mustExec(t, pool)(r7reviewScript)
	agenciesGetTheirOwnRepository(t, pool)
	mustExec(t, pool)(`INSERT INTO jobs (uid, name, source, run_type, scope, enabled, created_at) VALUES
	      ('j-fin-step', 'fin-step', 'cronomicon', 'bash', 'fin-hosts', 1, 't'), ('j-tax-step', 'tax-step', 'cronomicon', 'bash', 'tax-hosts', 1, 't')`)
	created := func(who, path, body string) string {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, path, who, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s POST %s: %d %s", who, path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	created(gFinAdmin, "/api/v1/jobs", r7reviewJob("load", "fin-hosts",
		`,"env":{"FIN_ONLY":"fin-value"},"schedules":[{"name":"fin-nightly","cron":"0 1 * * *","env":{"FIN_SCHED":"fin-sched-value"}}]`))
	body := created(gTaxAdmin, "/api/v1/jobs", r7reviewJob("load", "tax-hosts", ""))
	if strings.Contains(body, "fin-value") || strings.Contains(body, "fin-hosts") || strings.Contains(body, "fin-sched-value") {
		t.Errorf("TAX's save of load is answered with FIN's load: %s", body)
	}
	if !strings.Contains(body, `"scope":"tax-hosts"`) {
		t.Errorf("TAX's save of load is not answered with TAX's load: %s", body)
	}
	// An edit, the same.
	taxLoad := rowID(t, pool, `SELECT rowid FROM jobs WHERE name = 'load' AND scope = 'tax-hosts'`)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/jobs/"+taxLoad, gTaxAdmin, r7reviewJob("load", "tax-hosts", "")); rec.Code != http.StatusOK ||
		strings.Contains(rec.Body.String(), "fin-value") || !strings.Contains(rec.Body.String(), `"scope":"tax-hosts"`) {
		t.Errorf("TAX's edit of load is not answered with TAX's load: %d %s", rec.Code, rec.Body)
	}
	// And a workflow's.
	created(gFinAdmin, "/api/v1/workflows", `{"name":"nightly","steps":[{"type":"job","name":"fin-step"}]}`)
	body = created(gTaxAdmin, "/api/v1/workflows", `{"name":"nightly","steps":[{"type":"job","name":"tax-step"}]}`)
	if strings.Contains(body, "fin-step") || !strings.Contains(body, "tax-step") {
		t.Errorf("TAX's save of nightly is not answered with TAX's nightly: %s", body)
	}
}

// Present defect 50, FIXED (the owner's yes to question 25, 2026-10-14): a
// workflow's sub-workflow step resolved by name among the workflows that were
// not in the recycle bin (workflow.loadChildWorkflow), so when one agency's
// child was binned, its parent ran ANOTHER agency's workflow of the same name,
// on that agency's hosts. Binning is not a save, so 0b6827a's refusal was not
// asked.
//
// Now each sub-workflow step of a workflow built in the app is pinned, at its
// save, to the workflow it runs (workflowUid, chosen among those the saver may
// compose), and a pinned child that is binned fails the step. A step that pins
// nothing, saved before, runs a child built in the app only when one holds the
// name.
func TestGR0_BinningAChildMakesItsParentRunAnotherAgencysChild(t *testing.T) {
	h, pool := gateServer(t)
	mustExec(t, pool)(r7reviewScript)
	agenciesGetTheirOwnRepository(t, pool)
	for _, c := range []struct{ who, path, body string }{
		{gFinAdmin, "/api/v1/jobs", r7reviewJob("fin-a", "fin-hosts", "")},
		{gFinAdmin, "/api/v1/jobs", r7reviewJob("fin-d", "fin-hosts", "")},
		{gTaxAdmin, "/api/v1/jobs", r7reviewJob("tax-a", "tax-hosts", "")},
		{gFinAdmin, "/api/v1/workflows", `{"name":"nightly","steps":[{"type":"job","name":"fin-d"}]}`},
		{gTaxAdmin, "/api/v1/workflows", `{"name":"nightly","steps":[{"type":"job","name":"tax-a"}]}`},
		{gFinAdmin, "/api/v1/workflows", `{"name":"parent","steps":[{"type":"job","name":"fin-a"},{"type":"workflow","name":"go","workflow":"nightly"}]}`},
		{gTaxAdmin, "/api/v1/workflows", `{"name":"tax-parent","steps":[{"type":"workflow","name":"go","workflow":"nightly"}]}`},
	} {
		if rec := gateReq(t, h, http.MethodPost, c.path, c.who, c.body); rec.Code/100 != 2 {
			t.Fatalf("seed %s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	uidOf := func(q string) string {
		t.Helper()
		var u string
		if err := pool.QueryRow(q).Scan(&u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	finNightly := uidOf(`SELECT uid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:FIN'`)
	taxNightly := uidOf(`SELECT uid FROM workflows WHERE name = 'nightly' AND owner_agency = 'ag:TAX'`)
	stepsOf := func(name string) string { return uidOf(`SELECT steps FROM workflows WHERE name = '` + name + `'`) }
	// Each save pinned its saver's own child.
	if !strings.Contains(stepsOf("parent"), `"workflowUid":"`+finNightly+`"`) {
		t.Errorf("FIN's parent is not pinned to FIN's nightly: %s", stepsOf("parent"))
	}
	if !strings.Contains(stepsOf("tax-parent"), `"workflowUid":"`+taxNightly+`"`) {
		t.Errorf("TAX's parent is not pinned to TAX's nightly: %s", stepsOf("tax-parent"))
	}
	childScopes := func(raw string) []string {
		t.Helper()
		steps, err := workflow.ParseSteps(raw)
		if err != nil {
			t.Fatal(err)
		}
		scopes, err := workflow.New(pool, slog.New(slog.NewTextHandler(io.Discard, nil))).SubWorkflowJobScopes(context.Background(), steps, "cronomicon")
		if err != nil {
			t.Fatal(err)
		}
		return scopes
	}
	if got := childScopes(stepsOf("parent")); !slices.Equal(got, []string{"fin-hosts"}) {
		t.Fatalf("before: FIN's parent's child runs %v, want FIN's", got)
	}
	// A step that pins nothing (saved before 2.4.0) runs neither of two, and
	// not a Git workflow of the name in their place either.
	mustExec(t, pool)(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at, repo_id)
	        VALUES ('g-nightly', 'nightly', 'git', '[{"type":"job","name":"fin-a","jobSource":"cronomicon"}]', 1, 't', 'global')`)
	if got := childScopes(`[{"type":"workflow","name":"go","workflow":"nightly"}]`); len(got) != 0 {
		t.Errorf("an unpinned step with two workflows of the name runs %v, want neither", got)
	}
	rowOf := rowID(t, pool, `SELECT rowid FROM workflows WHERE uid = '`+finNightly+`'`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/workflows/"+rowOf, gFinAdmin, ""); rec.Code/100 != 2 {
		t.Fatalf("FIN bins its own nightly: %d %s", rec.Code, rec.Body)
	}
	// FIN's parent's child is binned: the step fails; TAX's does not run.
	if got := childScopes(stepsOf("parent")); len(got) != 0 {
		t.Errorf("after FIN bins its nightly, FIN's parent's child runs %v; want none (the step fails)", got)
	}
	// A global administrator, who may compose both, is asked which.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot, `{"name":"root-parent","steps":[{"type":"workflow","name":"go","workflow":"nightly"}]}`)
	if rec.Code/100 == 2 {
		// (FIN's is in the bin now: only TAX's is a candidate.)
		if !strings.Contains(stepsOf("root-parent"), `"workflowUid":"`+taxNightly+`"`) {
			t.Errorf("the global administrator's parent is not pinned to the one candidate: %s", stepsOf("root-parent"))
		}
	} else {
		t.Errorf("a global administrator's parent with one candidate = %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/workflows", gFinAdmin, `{"name":"fin-nightly2","steps":[{"type":"job","name":"fin-d"}]}`); rec.Code/100 != 2 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	mustExec(t, pool)(`UPDATE workflows SET name = 'nightly' WHERE name = 'fin-nightly2'`)
	rec = gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot, `{"name":"root-parent2","steps":[{"type":"workflow","name":"go","workflow":"nightly"}]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "workflowUid") {
		t.Errorf("a global administrator's parent naming two workflows it may compose = %d %s, want 422 asking for workflowUid", rec.Code, rec.Body)
	}
	// Given, it is kept.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot,
		`{"name":"root-parent3","steps":[{"type":"workflow","name":"go","workflow":"nightly","workflowUid":"`+taxNightly+`"}]}`)
	if rec.Code/100 != 2 || !strings.Contains(stepsOf("root-parent3"), `"workflowUid":"`+taxNightly+`"`) {
		t.Errorf("a parent that gives its child's identity = %d %s", rec.Code, rec.Body)
	}
	// One that names nothing, another name's workflow, or one in the bin, is
	// refused.
	for uid, says := range map[string]string{
		"no-such-uid": "names no workflow",
		uidOf(`SELECT uid FROM workflows WHERE name = 'tax-parent'`): "is the workflow",
		finNightly: "recycle bin",
	} {
		rec = gateReq(t, h, http.MethodPost, "/api/v1/workflows", gRoot,
			`{"name":"root-parent4","steps":[{"type":"workflow","name":"go","workflow":"nightly","workflowUid":"`+uid+`"}]}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), says) {
			t.Errorf("a parent whose workflowUid %s is not its step's workflow = %d %s, want 422 saying %q", uid, rec.Code, rec.Body, says)
		}
	}
}

// Present defect 51, FIXED (the owner's yes to question 26, 2026-10-14): a
// scope's jobs passed to another agency with no write to scope_agencies. A job
// carries its scope by NAME; a rename left its jobs on the old name (and its
// brokenReferences did not look at jobs), and a scope another agency's
// administrator then created under the old name took them: listed to that
// agency, runnable on its hosts. A way round the rule that a scope is moved by
// a global administrator only (bee2b35).
//
// Now a rename takes the jobs built in the app with it and names the jobs from
// Git that stay on the old name; and a scope may not take a name that jobs of
// another agency name (create, clone or rename: 409 scope_name_in_use), unless
// a global administrator takes it.
func TestGR0_AScopesJobsPassToTheAgencyThatTakesItsOldName(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(r7reviewScript)
	agenciesGetTheirOwnRepository(t, pool)
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs", gFinAdmin, r7reviewJob("fa", "fin-hosts", "")); rec.Code != http.StatusCreated {
		t.Fatalf("FIN's job: %d %s", rec.Code, rec.Body)
	}
	// A job from Global's repository that names FIN's scope (I-7), and one from
	// FIN's own repository.
	exec(`UPDATE git_repos SET id = 'repo-fin', url = 'http://127.0.0.1:1/fin.git' WHERE agency_id = 'ag:FIN'`)
	exec(`UPDATE scripts SET repo_id = 'repo-fin' WHERE repo_id = 'repo-ag:FIN'`)
	exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, scope, enabled, synced_at) VALUES
	        ('j-glob', 'glob-job', 'git', 'global',   'bash', 'fin-hosts', 1, 't'),
	        ('j-own',  'own-job',  'git', 'repo-fin', 'bash', 'fin-own',   1, 't'),
	        ('j-lost', 'lost-job', 'cronomicon', NULL, 'bash', 'ghost',     1, 't')`)
	scopeOf := func(job string) string {
		var sc string
		if err := pool.QueryRow(`SELECT COALESCE(scope,'') FROM jobs WHERE name = ?`, job).Scan(&sc); err != nil {
			t.Fatal(err)
		}
		return sc
	}

	rec := gateReq(t, h, http.MethodPatch, "/api/v1/scopes/sc:fin", gFinAdmin, `{"scope":"fin-renamed","hosts":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("FIN renames its scope: %d %s", rec.Code, rec.Body)
	}
	if got := scopeOf("fa"); got != "fin-renamed" {
		t.Errorf("FIN's job built in the app stayed on %q, want it on the scope's new name", got)
	}
	if got := scopeOf("glob-job"); got != "fin-hosts" {
		t.Errorf("the job from Git was rewritten to %q: its file says fin-hosts", got)
	}
	if !strings.Contains(rec.Body.String(), `{"entity":"jobs","name":"glob-job"}`) {
		t.Errorf("the rename does not name the job from Git left on the old name: %s", rec.Body)
	}

	// The old name, which Global's job still names, is not TAX's to take: not
	// by a create, a clone or a rename.
	refused := func(what, method, path, who, body string) {
		t.Helper()
		rec := gateReq(t, h, method, path, who, body)
		if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "scope_name_in_use" {
			t.Errorf("%s = %d %s, want 409 scope_name_in_use", what, rec.Code, rec.Body)
		}
	}
	refused("TAX creating a scope of the old name", http.MethodPost, "/api/v1/scopes", gTaxAdmin, `{"scope":"fin-hosts","hosts":["tax-box"],"agencyIds":["ag:TAX"]}`)
	refused("TAX renaming its scope to the old name", http.MethodPatch, "/api/v1/scopes/sc:tax", gTaxAdmin, `{"scope":"fin-hosts","hosts":[]}`)
	refused("TAX cloning a scope to the old name", http.MethodPost, "/api/v1/scopes/sc:tax/clone", gTaxAdmin, `{"agencyId":"ag:TAX","name":"fin-hosts"}`)
	// A job built in the app whose scope is gone is nobody's that can be told:
	// FIN may not take its name either.
	refused("FIN creating a scope of a name a lost job names", http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"ghost","hosts":[],"agencyIds":["ag:FIN"]}`)
	if n := count(t, pool, `SELECT COUNT(*) FROM scopes WHERE name IN ('fin-hosts', 'ghost')`); n != 0 {
		t.Errorf("a refused scope was written: %d", n)
	}
	if got := scopeOf("glob-job"); got != "fin-hosts" {
		t.Errorf("Global's job now names %q", got)
	}

	// A name that only the agency's own jobs name is the agency's to take.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scopes", gFinAdmin, `{"scope":"fin-own","hosts":[],"agencyIds":["ag:FIN"]}`); rec.Code != http.StatusCreated {
		t.Errorf("FIN creating a scope that FIN's own repository's job names = %d %s, want 201", rec.Code, rec.Body)
	}
	// And a global administrator gives a name, and its jobs, to an agency.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/scopes", gRoot, `{"scope":"fin-hosts","hosts":[],"agencyIds":["ag:TAX"]}`); rec.Code != http.StatusCreated {
		t.Errorf("a global administrator creating the scope = %d %s, want 201", rec.Code, rec.Body)
	}
}
