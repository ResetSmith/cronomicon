package db

import "testing"

// TestMigrate1360WorkflowOwner: every workflow gets an owner. A Git workflow's
// is its repository's agency; an in-app workflow's is the one agency its job
// steps belong to, sub-workflows followed, and Global when there is none to
// say or more than one. Then the way back.
func TestMigrate1360WorkflowOwner(t *testing.T) {
	h := openAt(t, 1350)
	h.exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	h.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	scope := func(id, name string, agencies ...string) {
		t.Helper()
		h.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, id, name)
		h.exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, id)
		for _, a := range agencies {
			h.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, id, a)
		}
	}
	scope("sc-fin", "fin-hosts", "ag-fin")
	scope("sc-tax", "tax-hosts", "ag-tax")
	scope("sc-glob", "glob-hosts", "global")
	scope("sc-both", "both-hosts", "ag-fin", "ag-tax")
	job := func(uid, name, source string, scope any, deleted any) {
		t.Helper()
		h.exec(`INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, deleted_at) VALUES (?, ?, ?, 'bash', ?, 't', ?)`,
			uid, name, source, scope, deleted)
	}
	job("j-fin", "fin-job", "cronomicon", "fin-hosts", nil)
	job("j-fin2", "fin-job-2", "cronomicon", "fin-hosts", nil)
	job("j-tax", "tax-job", "cronomicon", "tax-hosts", nil)
	job("j-glob", "glob-job", "cronomicon", "glob-hosts", nil)
	job("j-none", "unscoped-job", "cronomicon", nil, nil)
	job("j-both", "both-job", "cronomicon", "both-hosts", nil)
	job("j-odd", "odd-scope-job", "cronomicon", "no-such-scope", nil)
	job("j-gone", "binned-job", "cronomicon", "tax-hosts", "2026-01-01T00:00:00Z")
	// One name in both sources, and one name twice in the app (two agencies').
	job("j-twin-app", "twin", "cronomicon", "fin-hosts", nil)
	job("j-twin-git", "twin", "git", "tax-hosts", nil)
	job("j-pin-fin", "pinned", "cronomicon", "fin-hosts", nil)
	job("j-pin-tax", "pinned", "cronomicon", "tax-hosts", nil)

	wf := func(uid, name, source, steps string, repo any) {
		t.Helper()
		h.exec(`INSERT INTO workflows (uid, name, source, steps, synced_at, repo_id) VALUES (?, ?, ?, ?, 't', ?)`, uid, name, source, steps, repo)
	}
	want := map[string]string{}
	app := func(uid, steps, owner string) {
		t.Helper()
		wf(uid, "wf-"+uid, "cronomicon", steps, nil)
		want[uid] = owner
	}
	app("one-agency", `[{"type":"job","name":"fin-job"},{"type":"job","name":"fin-job-2"}]`, "ag-fin")
	app("no-type", `[{"name":"fin-job"}]`, "ag-fin") // a step with no type is a job
	app("spans", `[{"type":"job","name":"fin-job"},{"type":"job","name":"tax-job"}]`, "global")
	app("with-global", `[{"type":"job","name":"fin-job"},{"type":"job","name":"glob-job"}]`, "global")
	app("unscoped", `[{"type":"job","name":"unscoped-job"}]`, "global")
	app("shared-scope", `[{"type":"job","name":"both-job"}]`, "global")
	app("odd-scope", `[{"type":"job","name":"odd-scope-job"}]`, "global")
	app("empty", `[]`, "global")
	app("not-json", `this is not a step graph`, "global")
	app("dangling", `[{"type":"job","name":"fin-job"},{"type":"job","name":"nobody"}]`, "ag-fin") // a job that is not there says nothing
	app("binned-step", `[{"type":"job","name":"fin-job"},{"type":"job","name":"binned-job"}]`, "ag-fin")
	app("nested", `[{"type":"parallel","jobs":[{"type":"job","name":"fin-job"},{"type":"sequence","steps":[{"type":"job","name":"fin-job-2"}]}]},
	                {"type":"branch","condition":{"type":"job_status","jobRef":"fin-job"},"pass":{"steps":[{"type":"job","name":"fin-job"}]},"fail":{"steps":[{"type":"job","name":"tax-job"}]}}]`, "global")
	app("own-source-first", `[{"type":"job","name":"twin"}]`, "ag-fin")                // the app's twin before Git's
	app("stated-source", `[{"type":"job","name":"twin","jobSource":"git"}]`, "ag-tax") // the step says which
	app("pinned", `[{"type":"job","name":"pinned","jobUid":"j-pin-tax"}]`, "ag-tax")   // the step pins one
	app("child-tax", `[{"type":"job","name":"tax-job"}]`, "ag-tax")
	app("parent-of-tax", `[{"type":"workflow","name":"go","workflow":"wf-child-tax"}]`, "ag-tax")
	app("parent-spans", `[{"type":"job","name":"fin-job"},{"type":"workflow","name":"go","workflow":"wf-child-tax"}]`, "global")
	app("grandparent", `[{"type":"workflow","name":"go","workflow":"wf-parent-of-tax"}]`, "ag-tax")
	app("child-missing", `[{"type":"job","name":"fin-job"},{"type":"workflow","name":"go","workflow":"no-such-workflow"}]`, "ag-fin")
	// Two workflows that name each other: followed to the depth, and no further.
	app("loop-a", `[{"type":"job","name":"fin-job"},{"type":"workflow","name":"go","workflow":"wf-loop-b"}]`, "ag-fin")
	app("loop-b", `[{"type":"workflow","name":"go","workflow":"wf-loop-a"}]`, "ag-fin")

	wf("git-fin", "git-fin", "git", `[{"type":"job","name":"tax-job"}]`, "repo-fin") // its repository's, whatever it runs
	wf("git-global", "git-global", "git", `[{"type":"job","name":"fin-job"}]`, "global")
	wf("git-none", "git-none", "git", `[]`, nil)
	wf("git-orphan", "git-orphan", "git", `[]`, "no-such-repository")
	want["git-fin"], want["git-global"], want["git-none"], want["git-orphan"] = "ag-fin", "global", "global", "global"

	h.to(1360)

	for uid, owner := range want {
		if got := h.str(`SELECT owner_agency FROM workflows WHERE uid = ?`, uid); got != owner {
			t.Errorf("the workflow %s is %q's, want %q's", uid, got, owner)
		}
	}
	if n := h.count(`SELECT COUNT(*) FROM workflows WHERE COALESCE(owner_agency, '') = ''`); n != 0 {
		t.Errorf("%d workflows have no owner", n)
	}
	// A new row has one from the start.
	h.exec(`INSERT INTO workflows (uid, name, source, steps, synced_at) VALUES ('later', 'later', 'cronomicon', '[]', 't')`)
	if got := h.str(`SELECT owner_agency FROM workflows WHERE uid = 'later'`); got != "global" {
		t.Errorf("a new workflow's owner is %q, want global until its composer says", got)
	}

	h.to(1350)
	if n := h.count(`SELECT COUNT(*) FROM pragma_table_info('workflows') WHERE name = 'owner_agency'`); n != 0 {
		t.Errorf("workflows.owner_agency is still there after the way back")
	}
	if n := h.count(`SELECT COUNT(*) FROM workflows`); n != len(want)+1 {
		t.Errorf("the way back lost workflows: %d left of %d", n, len(want)+1)
	}
	h.to(1360)
}
