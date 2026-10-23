package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A job built in the app whose script has gone does not run (the owner's
// decision of 2026-10-13), and the inbox says so, under the job's agency,
// until the script is back, the job is on another, or the job is removed.
func TestAnInAppJobWhoseScriptHasGoneIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO scripts (uid, repo_id, name, run_type, script, content_hash, synced_at) VALUES
	   ('s-here', 'global', 'here.sh', 'bash', 'echo here', 'h', 't'),
	   ('s-soon', 'global', 'soon-gone.sh', 'bash', 'echo soon', 'h', 't')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_ref, script_uid, script, script_path, synced_at, deleted_at) VALUES
	   ('fin-gone',   'fin-gone',   'cronomicon', 'bash', 'fin-hosts', 'soon-gone.sh', 's-soon', 'echo a copy', NULL,             't', NULL),
	   ('free-gone',  'free-gone',  'cronomicon', 'bash', NULL,        'soon-gone.sh', 's-soon', 'echo a copy', NULL,             't', NULL),
	   ('file-gone',  'file-gone',  'cronomicon', 'bash', 'fin-hosts', NULL,           NULL,     NULL,          'scripts/x.sh',   't', NULL),
	   ('has-script', 'has-script', 'cronomicon', 'bash', 'fin-hosts', 'here.sh',      's-here', 'echo a copy', NULL,             't', NULL),
	   ('own-body',   'own-body',   'cronomicon', 'bash', 'fin-hosts', NULL,           NULL,     'echo mine',   NULL,             't', NULL),
	   ('git-gone',   'git-gone',   'git',        'bash', 'fin-hosts', 'soon-gone.sh', 's-soon', 'echo a copy', NULL,             't', NULL),
	   ('binned',     'binned',     'cronomicon', 'bash', 'fin-hosts', 'soon-gone.sh', 's-soon', 'echo a copy', NULL,             't', '2026-01-01T00:00:00Z')`)
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindJobScriptGone)
	}
	only := func(got map[string]notices.Notice, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Errorf("open notices = %v, want exactly %v", keys(got), want)
		}
		for _, w := range want {
			if _, ok := got[w]; !ok {
				t.Errorf("no notice for %s; open: %v", w, keys(got))
			}
		}
	}

	// With its script there, only the job that names none and keeps a file's
	// path is one: a file is a script's.
	only(check(), "file-gone")

	// The script is removed (the trigger clears the jobs' script_uid).
	mustExec(t, pool, `DELETE FROM scripts WHERE uid = 's-soon'`)
	got := check()
	only(got, "file-gone", "fin-gone", "free-gone")
	n := got["fin-gone"]
	if n.AgencyID != "ag-fin" {
		t.Errorf("the notice of Finance's job is filed under %q, want Finance", n.AgencyID)
	}
	for _, want := range []string{"The job fin-gone", "the scope fin-hosts", "the script soon-gone.sh", "does not run", "put the script back under that name"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if got["free-gone"].AgencyID != "global" {
		t.Errorf("the notice of the job with no scope is filed under %q, want Global", got["free-gone"].AgencyID)
	}

	// The script is back, and a sync has joined the jobs to it again.
	mustExec(t, pool, `INSERT INTO scripts (uid, repo_id, name, run_type, script, content_hash, synced_at)
	   VALUES ('s-back', 'global', 'soon-gone.sh', 'bash', 'echo back', 'h', 't')`)
	mustExec(t, pool, `UPDATE jobs SET script_uid = 's-back' WHERE uid = 'fin-gone'`)
	only(check(), "file-gone", "free-gone")
}
