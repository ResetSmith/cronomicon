package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// The notice for a project job that no agent can run (present defects 24, 39
// and 44; the owner's decisions of 2026-10-09, 2026-10-12 and 2026-10-13).
// Where no registered agent that serves a project job is started with
// -allow-checkout, its runs are refused where they are produced, and this
// names the job: one from Git by its own marker, and one built in the app by
// its SCRIPT's as well, so that it is there at the first start of 2.4.0,
// before any such job carries the marker. It is filed under the job's agency
// and resolves when such an agent is there.
func TestAProjectJobWithNoCheckoutAgentIsReported(t *testing.T) {
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
	// A shell-type project and a job built in the app on it, in each agency.
	mustExec(t, pool, `INSERT INTO scripts (uid, repo_id, name, run_type, script_path, project_root, content_hash, synced_at) VALUES
	   ('s-shell', 'global', 'shell-proj', 'bash', 'scripts/shell-proj/run.sh', 'scripts/shell-proj', 'h', 't')`)
	for _, j := range [][2]string{{"fin-shell", "fin-hosts"}, {"tax-shell", "tax-hosts"}} {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_uid, script_path, synced_at)
		   VALUES (?, ?, 'cronomicon', 'bash', ?, 's-shell', 'x', 't')`, j[0], j[0], j[1])
	}
	job("fin-proj", "cronomicon", "fin-hosts", "s-proj", nil) // the condition, in Finance
	// This one has been synced since 2.4.0 started: its own row carries the marker.
	mustExec(t, pool, `UPDATE jobs SET project_root = 'scripts/deploy-app' WHERE uid = 'fin-proj'`)
	job("tax-proj", "cronomicon", "tax-hosts", "s-proj", nil)   // the condition, in Tax
	job("free-proj", "cronomicon", nil, "s-proj", nil)          // the condition, with no scope: Global's
	job("bound-proj", "cronomicon", "fin-bound", "s-proj", nil) // the condition, on a scope that names its runners
	job("fin-plain", "cronomicon", "fin-hosts", "s-plain", nil) // not on a project
	job("git-proj", "git", "fin-hosts", "s-proj", nil)          // a Git job with no marker of its own is no project
	// A Git job that IS a project, by the marker sync wrote on its own row.
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_uid, script_path, project_root, synced_at)
	   VALUES ('git-own-proj', 'git-own-proj', 'git', 'ansible', 'fin-hosts', 's-proj', 'scripts/deploy-app/site.yml', 'scripts/deploy-app', 't')`)
	// And one on a scope the catalogue does not hold (a scope of an agent's own
	// inventory): its run is Global's agents', as a run with no scope is, and
	// so is this notice's question (the review of 2.4.0 Phase R5's last part:
	// the notice said "its runs are refused" of a job whose runs were not).
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, script_uid, script_path, project_root, synced_at)
	   VALUES ('git-elsewhere-proj', 'git-elsewhere-proj', 'git', 'ansible', 'not-in-the-catalogue', 's-proj', 'scripts/deploy-app/site.yml', 'scripts/deploy-app', 't')`)
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
		return openOf(t, pool, notices.KindProjectJobNoCheckoutAgent)
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
	only(got, "fin-proj", "git-own-proj", "git-elsewhere-proj", "tax-proj", "free-proj", "bound-proj", "fin-shell", "tax-shell")
	n := got["fin-proj"]
	if n.AgencyID != "ag-fin" {
		t.Errorf("the notice of Finance's job is filed under %q, want Finance", n.AgencyID)
	}
	for _, want := range []string{"The job fin-proj", "the scope fin-hosts", "was built in the app on the project deploy-app", "-allow-checkout",
		"its runs are refused", "build the job on a script that is not a project"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	if strings.Contains(n.Detail, "will wait") {
		t.Errorf("the detail says the runs will wait; with no agent registered they are refused: %s", n.Detail)
	}
	// The refusal reads the marker on the job's own row. fin-proj carries one;
	// bound-proj is as the first start of 2.4.0 finds it, with the marker on
	// its script only, and is told what is true of it until a sync or a save.
	if strings.Contains(n.Detail, "will be refused") {
		t.Errorf("a job whose own row carries the marker is told its runs WILL be refused; they are: %s", n.Detail)
	}
	if b := got["bound-proj"].Detail; !strings.Contains(b, "its runs will be refused from the next sync") ||
		!strings.Contains(b, "Until then they run as they did before 2.4.0") || strings.Contains(b, "so its runs are refused") {
		t.Errorf("a job whose own row carries no marker yet is told: %s", b)
	}
	// The Git job is told the same, in its own words.
	g := got["git-own-proj"]
	for _, want := range []string{"The job git-own-proj", "is a project", "its runs are refused", "change the job in Git"} {
		if !strings.Contains(g.Detail, want) {
			t.Errorf("the Git job's detail does not say %q: %s", want, g.Detail)
		}
	}
	if g.AgencyID != "ag-fin" || strings.Contains(g.Detail, "built in the app") {
		t.Errorf("the Git job's notice: agency %q, detail %s", g.AgencyID, g.Detail)
	}
	if e := got["git-elsewhere-proj"]; e.AgencyID != "global" || !strings.Contains(e.Detail, "the scope not-in-the-catalogue") {
		t.Errorf("the notice of the job on a scope that is not in the catalogue: %+v, want it under Global", e)
	}
	if got["free-proj"].AgencyID != "global" || !strings.Contains(got["free-proj"].Detail, "no scope") {
		t.Errorf("the notice of the job with no scope: %+v, want it under Global", got["free-proj"])
	}

	// An agent with Ansible and no checkout serves Finance: nothing changes.
	runner("fin-plain-agent", "agent", `["ansible","bash"]`, "ag-fin")
	only(check(), "fin-proj", "git-own-proj", "git-elsewhere-proj", "tax-proj", "free-proj", "bound-proj", "fin-shell", "tax-shell")

	// The server's own runner serves Finance. It is no agent that allows
	// checkout, whatever its row says, and it takes no project job's run, of a
	// shell type or not: every notice stays.
	runner("the-server", "server", `["bash","perl","powershell","python","checkout"]`, "ag-fin")
	only(check(), "fin-proj", "git-own-proj", "git-elsewhere-proj", "tax-proj", "free-proj", "bound-proj", "fin-shell", "tax-shell")

	// An agent that declares checkout serves Finance, and an administrator has
	// masked it: the claim hands it no project's run, and every notice stays.
	runner("fin-masked-agent", "agent", `["ansible","checkout"]`, "ag-fin")
	mustExec(t, pool, `UPDATE runners SET managed_settings = '{"capabilityMask":["checkout"]}' WHERE id = 'fin-masked-agent'`)
	only(check(), "fin-proj", "git-own-proj", "git-elsewhere-proj", "tax-proj", "free-proj", "bound-proj", "fin-shell", "tax-shell")

	// An agent started with -allow-checkout serves Finance. Finance's job on an
	// unbound scope is served; the scope that names its runners does not name
	// this one; Tax's and Global's are another agency's.
	runner("fin-checkout-agent", "agent", `["ansible","checkout"]`, "ag-fin")
	mustExec(t, pool, `UPDATE runners SET managed_settings = '{"capabilityMask":["ansible"]}' WHERE id = 'fin-checkout-agent'`)
	mustExec(t, pool, `INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-bound', 'fin-plain-agent', 'fin-plain-agent', 't')`)
	only(check(), "tax-proj", "free-proj", "git-elsewhere-proj", "bound-proj", "tax-shell")

	// The scope names the capable agent too.
	mustExec(t, pool, `INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-bound', 'fin-checkout-agent', 'fin-checkout-agent', 't')`)
	only(check(), "tax-proj", "free-proj", "git-elsewhere-proj", "tax-shell")

	// Global's and Tax's get theirs, and so does the job on a scope that is
	// not in the catalogue: Global's agent is the one that takes its run.
	runner("global-checkout-agent", "agent", `["checkout","ansible"]`, "global")
	runner("tax-checkout-agent", "agent", `["checkout","ansible"]`, "ag-tax")
	only(check())

	// And it comes back when the agent goes.
	mustExec(t, pool, `DELETE FROM runners WHERE id = 'tax-checkout-agent'`)
	only(check(), "tax-proj", "tax-shell")
}

func keys(m map[string]notices.Notice) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
