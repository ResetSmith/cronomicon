package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// Present defect 24's notice (the owner's decisions of 2026-10-09 and
// 2026-10-12): a job built in the app on a project is a checkout job from
// 2.4.0, and where no registered agent that serves it is started with
// -allow-checkout its runs will fail. It is read off the SCRIPT, so it is
// there at the first start, before any job carries the marker; it is filed
// under the job's agency; and it resolves when such an agent is there.
func TestAnInAppJobOnAProjectWithNoCheckoutAgentIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	for _, s := range [][3]string{{"sc-fin", "fin-hosts", "ag-fin"}, {"sc-tax", "tax-hosts", "ag-tax"}, {"sc-bound", "fin-bound", "ag-fin"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, s[0], s[1])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	mustExec(t, pool, `INSERT INTO scripts (uid, repo_id, name, run_type, script_path, project_root, content_hash, synced_at) VALUES
	   ('s-proj',  'global', 'deploy-app', 'ansible', 'scripts/deploy-app/site.yml', 'scripts/deploy-app', 'h', 't'),
	   ('s-plain', 'global', 'plain.yml',  'ansible', 'scripts/plain.yml',           NULL,                 'h', 't')`)
	// As the first start of 2.4.0 finds them: no job built in the app carries
	// the marker (project_root) on its own row.
	job := func(uid, source string, scope, script, deleted any) {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_uid, script_path, synced_at, deleted_at)
		   VALUES (?, ?, ?, 'ansible', ?, ?, 'x', 't', ?)`, uid, uid, source, scope, script, deleted)
	}
	job("fin-proj", "cronomicon", "fin-hosts", "s-proj", nil)   // the condition, in Finance
	job("tax-proj", "cronomicon", "tax-hosts", "s-proj", nil)   // the condition, in Tax
	job("free-proj", "cronomicon", nil, "s-proj", nil)          // the condition, with no scope: Global's
	job("bound-proj", "cronomicon", "fin-bound", "s-proj", nil) // the condition, on a scope that names its runners
	job("fin-plain", "cronomicon", "fin-hosts", "s-plain", nil) // not on a project
	job("git-proj", "git", "fin-hosts", "s-proj", nil)          // a Git job has always been a checkout job
	job("binned", "cronomicon", "fin-hosts", "s-proj", "2026-01-01T00:00:00Z")
	job("scriptless", "cronomicon", "fin-hosts", nil, nil)

	runner := func(id, kind, caps string, agencies ...string) {
		mustExec(t, pool, `INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
		   VALUES (?, ?, ?, 'offline', 'Linux', ?, 0, 5, '1.0', 't', 't')`, id, id, kind, caps)
		for _, a := range agencies {
			mustExec(t, pool, `INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, id, a)
		}
	}
	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindInAppProjectJobNoCheckoutAgent)
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

	// No agent at all.
	got := check()
	only(got, "fin-proj", "tax-proj", "free-proj", "bound-proj")
	n := got["fin-proj"]
	if n.AgencyID != "ag-fin" {
		t.Errorf("the notice of Finance's job is filed under %q, want Finance", n.AgencyID)
	}
	for _, want := range []string{"The job fin-proj", "the scope fin-hosts", "the project deploy-app", "-allow-checkout", "will fail"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if strings.Contains(n.Detail, "wait") {
		t.Errorf("the detail says the runs will wait, which they do not: %s", n.Detail)
	}
	if got["free-proj"].AgencyID != "global" || !strings.Contains(got["free-proj"].Detail, "no scope") {
		t.Errorf("the notice of the job with no scope: %+v, want it under Global", got["free-proj"])
	}

	// An agent with Ansible and no checkout serves Finance: nothing changes.
	// Nor does the server's own runner count, whatever it advertises.
	runner("fin-plain-agent", "agent", `["ansible","bash"]`, "ag-fin")
	runner("the-server", "server", `["ansible","checkout"]`, "ag-fin", "ag-tax", "global")
	only(check(), "fin-proj", "tax-proj", "free-proj", "bound-proj")

	// An agent started with -allow-checkout serves Finance. Finance's job on an
	// unbound scope is served; the scope that names its runners does not name
	// this one; Tax's and Global's are another agency's.
	runner("fin-checkout-agent", "agent", `["ansible","checkout"]`, "ag-fin")
	mustExec(t, pool, `INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-bound', 'fin-plain-agent', 'fin-plain-agent', 't')`)
	only(check(), "tax-proj", "free-proj", "bound-proj")

	// The scope names the capable agent too.
	mustExec(t, pool, `INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-bound', 'fin-checkout-agent', 'fin-checkout-agent', 't')`)
	only(check(), "tax-proj", "free-proj")

	// Global's and Tax's get theirs.
	runner("global-checkout-agent", "agent", `["checkout","ansible"]`, "global")
	runner("tax-checkout-agent", "agent", `["checkout","ansible"]`, "ag-tax")
	only(check())

	// And it comes back when the agent goes.
	mustExec(t, pool, `DELETE FROM runners WHERE id = 'tax-checkout-agent'`)
	only(check(), "tax-proj")
}

func keys(m map[string]notices.Notice) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
