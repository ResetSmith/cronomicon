package runref

import (
	"context"
	"testing"
)

// Present defect 44: a project job's run is refused where it is produced when
// no REGISTERED agent that serves the job allows checkout, and waits when one
// does. What matters as much as the refusal is what is NOT refused: a job that
// is no project, and a project with an agent to wait for, online or not.
func TestCheckoutNeedsAgent(t *testing.T) {
	f := newOwnerFixture(t) // agencies ag-a and ag-b
	ctx := context.Background()
	scope := func(id, name, agency string) {
		t.Helper()
		f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, id, name)
		f.exec(`DELETE FROM scope_agencies WHERE scope_id = ?`, id)
		f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, id, agency)
	}
	scope("s-a", "a-hosts", "ag-a")
	scope("s-b", "b-hosts", "ag-b")
	scope("s-bound", "a-bound", "ag-a")
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, synced_at, script_path, project_root) VALUES
	        ('j-proj',  'proj',  'git',        'ansible', 't', 'scripts/proj/site.yml', 'scripts/proj'),
	        ('j-app',   'built', 'cronomicon', 'ansible', 't', 'scripts/proj/site.yml', 'scripts/proj'),
	        ('j-body',  'body',  'git',        'ansible', 't', 'scripts/x.yml',         NULL),
	        ('j-twin1', 'twin',  'cronomicon', 'ansible', 't', 'scripts/proj/site.yml', 'scripts/proj'),
	        ('j-twin2', 'twin',  'cronomicon', 'ansible', 't', 'scripts/proj/site.yml', 'scripts/proj')`)
	runner := func(id, kind, caps string, agencies ...string) {
		t.Helper()
		f.exec(`INSERT INTO runners (id, name, kind, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
		        VALUES (?, ?, ?, 'offline', 'Linux', ?, 0, 5, '1.0', 't', 't')`, id, id, kind, caps)
		for _, a := range agencies {
			f.exec(`INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, id, a)
		}
		// Its owner is the agency it is placed in, as the write path makes it.
		if len(agencies) > 0 {
			f.exec(`UPDATE runners SET owner_agency = ? WHERE id = ?`, agencies[0], id)
		}
	}
	ask := func(what, uid, source, name, scope string, want bool) {
		t.Helper()
		got, err := CheckoutNeedsAgent(ctx, f.pool, uid, source, name, scope)
		if err != nil || got != want {
			t.Errorf("%s: refused = %v (%v), want %v", what, got, err, want)
		}
	}

	// No agent at all.
	ask("a project, no agent", "j-proj", "git", "proj", "a-hosts", true)
	ask("a project built in the app, no agent", "j-app", "cronomicon", "built", "a-hosts", true)
	ask("a project with no scope, no agent", "j-proj", "git", "proj", "", true)
	ask("a project found by name", "", "git", "proj", "a-hosts", true)
	ask("a project found by the default source", "", "", "proj", "a-hosts", true)
	ask("a job that is no project", "j-body", "git", "body", "a-hosts", false)
	ask("a job that is not there", "no-such", "git", "nobody", "a-hosts", false)
	ask("a name two jobs hold, with no uid", "", "cronomicon", "twin", "a-hosts", false)

	// Agents that do not count: one without checkout, the server's own runner
	// whatever its row says, and another agency's.
	runner("a-plain", "agent", `["ansible","bash"]`, "ag-a")
	runner("the-server", "server", `["bash","checkout"]`, "ag-a", "global")
	runner("b-checkout", "agent", `["ansible","checkout"]`, "ag-b")
	// A legacy placement (MA-9): listed for ag-a, owned by Global. It takes none
	// of ag-a's work (§0a, question 29), so it is no agent to wait for (the
	// review of I1: this check counted it).
	runner("legacy-checkout", "agent", `["ansible","checkout"]`)
	f.exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('legacy-checkout', 'ag-a')`)
	ask("an agent that does not allow checkout", "j-proj", "git", "proj", "a-hosts", true)
	ask("no scope, with only the server's runner for Global", "j-proj", "git", "proj", "", true)
	ask("another agency's agent allows it, its own scope", "j-proj", "git", "proj", "b-hosts", false)

	// One that declares it and has it masked by an administrator: the claim
	// hands it no project's run, so it is no agent to wait for.
	runner("a-masked", "agent", `["ansible","checkout"]`, "ag-a")
	f.exec(`UPDATE runners SET managed_settings = '{"capabilityMask":["checkout"]}' WHERE id = 'a-masked'`)
	ask("an agent whose checkout is masked", "j-proj", "git", "proj", "a-hosts", true)

	// One of the scope's agency's that allows it: registered, offline, and
	// enough. The run waits for it. A mask of another token takes nothing from
	// this one.
	runner("a-checkout", "agent", `["ansible","checkout"]`, "ag-a")
	f.exec(`UPDATE runners SET managed_settings = '{"capabilityMask":["ansible"]}' WHERE id = 'a-checkout'`)
	ask("an agent of the scope's agency allows it", "j-proj", "git", "proj", "a-hosts", false)
	ask("the same, for the job built in the app", "j-app", "cronomicon", "built", "a-hosts", false)
	ask("the same, for no scope: that agent is not Global's", "j-proj", "git", "proj", "", true)
	ask("a scope that is not there is Global's", "j-proj", "git", "proj", "no-such-scope", true)

	// A scope that names its runners, and not that one.
	f.exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('s-bound', 'a-plain', 'a-plain', 't')`)
	ask("a scope bound to an agent that does not allow it", "j-proj", "git", "proj", "a-bound", true)
	f.exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('s-bound', 'a-checkout', 'a-checkout', 't')`)
	ask("the scope names the agent that allows it", "j-proj", "git", "proj", "a-bound", false)

	// Global's.
	runner("g-checkout", "agent", `["checkout"]`, "global")
	ask("no scope, with a Global agent that allows it", "j-proj", "git", "proj", "", false)
}
