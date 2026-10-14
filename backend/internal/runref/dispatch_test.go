package runref

import (
	"context"
	"testing"
)

// The dispatch decision is one list, which the manifest and the local runner
// both ask (2.4.0 Phase R5). Each refusal is reached here by the change that
// can turn it while a run waits, with its code and its sentence, and in the
// order the list gives: a run that two rules refuse is given the first one's
// reason. What matters as much is what is NOT refused.
func TestRefuseDispatch(t *testing.T) {
	f := newOwnerFixture(t) // agencies ag-a and ag-b
	ctx := context.Background()
	f.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`)
	f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES
	        ('s-a', 'a-hosts', 'cronomicon', 't'), ('s-b', 'b-hosts', 'cronomicon', 't'), ('s-g', 'g-hosts', 'cronomicon', 't')`)
	f.exec(`DELETE FROM scope_agencies WHERE scope_id IN ('s-a', 's-b', 's-g')`)
	f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-a', 'ag-a'), ('s-b', 'ag-b'), ('s-g', 'global')`)
	f.exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES
	        ('sc-a', 'repo-a', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't'),
	        ('sc-g', 'global', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't')`)
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, repo_id, script_ref, script_uid, command) VALUES
	        ('j-a',     'of-a',       'git',        'bash', 't', 'repo-a', NULL,     NULL,   'true'),
	        ('j-g',     'of-global',  'git',        'bash', 't', 'global', NULL,     NULL,   'true'),
	        ('j-app-a', 'built-on-a', 'cronomicon', 'bash', 't', NULL,     'run.sh', 'sc-a', NULL),
	        ('j-app-g', 'built-on-g', 'cronomicon', 'bash', 't', NULL,     'run.sh', 'sc-g', NULL),
	        ('j-gone',  'built-gone', 'cronomicon', 'bash', 't', NULL,     'was.sh', NULL,   NULL)`)
	f.exec(`INSERT INTO workflows (uid, name, source, steps, synced_at, repo_id) VALUES
	        ('w-a',   'flow-of-a', 'git',        '[]', 't', 'repo-a'),
	        ('w-app', 'flow-app',  'cronomicon', '[]', 't', NULL)`)
	f.exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_uid, status, triggered_by, trigger_kind, created_at) VALUES
	        ('wr-a',   'flow-of-a', 'w-a',   'running', 'u', 'manual', 't'),
	        ('wr-app', 'flow-app',  'w-app', 'running', 'u', 'manual', 't')`)

	run := func(id, name, source, uid, scope string, pin [3]any, workflowRun any) {
		t.Helper()
		f.exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, scope, status, triggered_by, trigger_kind,
		                          created_at, checkout_repo, checkout_sha, checkout_entry, workflow_run_id)
		        VALUES (?, ?, ?, ?, 'bash', NULLIF(?, ''), 'running', 'u', 'manual', 't', ?, ?, ?, ?)`,
			id, name, source, uid, scope, pin[0], pin[1], pin[2], workflowRun)
	}
	none := [3]any{nil, nil, nil}
	run("r-ok", "of-a", "git", "j-a", "a-hosts", none, nil)
	run("r-stranded", "of-a", "git", "j-a", "b-hosts", none, nil)
	run("r-app-stranded", "built-on-a", "cronomicon", "j-app-a", "b-hosts", none, nil)
	run("r-global", "of-global", "git", "j-g", "b-hosts", none, nil)
	run("r-global-own", "of-global", "git", "j-g", "g-hosts", none, nil)
	run("r-global-none", "of-global", "git", "j-g", "", none, nil)
	run("r-app-global", "built-on-g", "cronomicon", "j-app-g", "", none, nil)
	run("r-step-outside", "of-global", "git", "j-g", "g-hosts", none, "wr-a")
	run("r-step-inside", "of-a", "git", "j-a", "a-hosts", none, "wr-a")
	run("r-step-app", "of-global", "git", "j-g", "g-hosts", none, "wr-app")
	run("r-step-app-outside", "of-a", "git", "j-a", "a-hosts", none, "wr-app")
	run("r-pin-outside", "built-on-g", "cronomicon", "j-app-g", "b-hosts", [3]any{"repo-a", "abc123", "scripts/p/site.yml"}, nil)
	run("r-pin-inside", "built-on-g", "cronomicon", "j-app-g", "a-hosts", [3]any{"repo-a", "abc123", "scripts/p/site.yml"}, nil)
	run("r-pin-global", "built-on-g", "cronomicon", "j-app-g", "b-hosts", [3]any{"global", "abc123", "scripts/p/site.yml"}, nil)
	run("r-gone", "built-gone", "cronomicon", "j-gone", "a-hosts", none, nil)
	run("r-gone-unpinned", "built-gone", "cronomicon", "j-gone", "a-hosts", [3]any{nil, nil, "scripts/p/site.yml"}, nil)
	run("r-unpinned-entry", "of-global", "git", "j-g", "g-hosts", [3]any{nil, nil, "scripts/p/site.yml"}, nil)
	run("r-unpinned-repo", "of-global", "git", "j-g", "g-hosts", [3]any{"global", nil, nil}, nil)
	run("r-unpinned-empty", "of-global", "git", "j-g", "g-hosts", [3]any{"global", "", "scripts/p/site.yml"}, nil)
	// Refused by two rules: the first of the list is the one that is said.
	run("r-stranded-step", "of-a", "git", "j-a", "b-hosts", none, "wr-a")
	run("r-stranded-unpinned", "of-a", "git", "j-a", "b-hosts", [3]any{"repo-a", nil, "scripts/p/site.yml"}, nil)
	run("r-pin-outside-unpinned", "built-on-g", "cronomicon", "j-app-g", "b-hosts", [3]any{"repo-a", nil, "scripts/p/site.yml"}, nil)

	for _, c := range []struct {
		what, run  string
		code, text string // "" for a run that goes ahead
	}{
		{"a job of an agency's repository on its agency's scope", "r-ok", "", ""},
		{"the same on another agency's scope", "r-stranded", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"built in the app on that repository's script, on another agency's scope", "r-app-stranded", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		// §0a (IN-3): Global's repository is confined too; until then any scope went ahead.
		{"a job of Global's repository on an agency's scope", "r-global", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"a job of Global's repository on Global's scope", "r-global-own", "", ""},
		{"a job of Global's repository on no scope", "r-global-none", "", ""},
		{"built in the app on Global's script, on no scope", "r-app-global", "", ""},
		{"a step of an agency's repository's workflow, outside that agency", "r-step-outside", CodeRepoScopeMismatch, ReasonWorkflowScopeMismatch},
		{"the same workflow's step on its agency's scope", "r-step-inside", "", ""},
		// §0a (IN-8): a workflow built in the app is its owner's (Global's here);
		// until then its steps went ahead anywhere.
		{"a step of a workflow built in the app, on its agency's scope", "r-step-app", "", ""},
		{"a step of a workflow built in the app, on another agency's", "r-step-app-outside", CodeRepoScopeMismatch, ReasonWorkflowScopeMismatch},
		{"pinned to an agency's repository, on another agency's scope", "r-pin-outside", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"pinned to an agency's repository, on its scope", "r-pin-inside", "", ""},
		// Question 31: an in-app job on Global's script is left as it was.
		{"pinned to Global's repository, on any scope", "r-pin-global", "", ""},
		{"built in the app on a script that has gone", "r-gone", CodeScriptGone, ReasonScriptGone},
		{"the same with an entry and no commit: the script is the reason", "r-gone-unpinned", CodeScriptGone, ReasonScriptGone},
		{"a project's run with an entry and no commit", "r-unpinned-entry", CodeNoCheckoutSHA, ReasonNoCheckoutSHA},
		{"a project's run with a repository and no commit", "r-unpinned-repo", CodeNoCheckoutSHA, ReasonNoCheckoutSHA},
		{"a project's run with an empty commit", "r-unpinned-empty", CodeNoCheckoutSHA, ReasonNoCheckoutSHA},
		{"stranded, and a step outside its workflow's repository", "r-stranded-step", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"stranded, and unpinned", "r-stranded-unpinned", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"pinned outside, and with no commit", "r-pin-outside-unpinned", CodeRepoScopeMismatch, ReasonRepoScopeMismatch},
		{"a run that is not there", "no-such-run", "", ""},
	} {
		got, err := RefuseDispatch(ctx, f.pool, c.run)
		if err != nil {
			t.Errorf("%s: %v", c.what, err)
			continue
		}
		switch {
		case c.code == "" && got != nil:
			t.Errorf("%s: refused with %s (%s), want it to go ahead", c.what, got.Code, got.Reason)
		case c.code != "" && got == nil:
			t.Errorf("%s: goes ahead, want %s", c.what, c.code)
		case c.code != "" && (got.Code != c.code || got.Reason != c.text):
			t.Errorf("%s: refused with %s (%s), want %s (%s)", c.what, got.Code, got.Reason, c.code, c.text)
		}
	}

	// The three sentences are three: a refusal read in a run's log has to say
	// which rule it was.
	if ReasonRepoScopeMismatch == ReasonWorkflowScopeMismatch || ReasonScriptGone == ReasonNoCheckoutSHA {
		t.Fatal("two rules share a sentence")
	}

	// What changes while a run waits turns the answer: the scope is given to
	// another agency, and the script is removed.
	f.exec(`UPDATE scope_agencies SET agency_id = 'ag-b' WHERE scope_id = 's-a'`)
	if got, err := RefuseDispatch(ctx, f.pool, "r-ok"); err != nil || got == nil || got.Code != CodeRepoScopeMismatch {
		t.Errorf("after its scope was given to another agency: %+v (%v), want %s", got, err, CodeRepoScopeMismatch)
	}
	f.exec(`UPDATE jobs SET script_uid = NULL WHERE uid = 'j-app-g'`)
	if got, err := RefuseDispatch(ctx, f.pool, "r-app-global"); err != nil || got == nil || got.Code != CodeScriptGone {
		t.Errorf("after its script was removed: %+v (%v), want %s", got, err, CodeScriptGone)
	}

	// A check that cannot be made is an error, and no answer: both callers
	// refuse on it.
	f.exec(`DROP TABLE scope_agencies`)
	if got, err := RefuseDispatch(ctx, f.pool, "r-stranded"); err == nil {
		t.Errorf("with a check that cannot be made: %+v and no error", got)
	}
}
