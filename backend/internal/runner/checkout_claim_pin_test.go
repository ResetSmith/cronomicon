package runner

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// TestACheckoutRunWaitsForAnAgentThatAllowsCheckout (present defect 39; this
// was the pin TestGR0_ACheckoutRunIsClaimedByAnAgentThatDoesNotAllowCheckout,
// inverted on the owner's decision of 2026-10-12).
//
// A checkout run is run only by an agent started with -allow-checkout, which
// advertises the token "checkout". Nothing asked for that token at the claim:
// the run of a project job recorded no requirement, any agent with the run
// type took it, and one that does not allow checkout refused it and the run
// failed. On a scope served by both kinds of agent the same job passed or
// failed by which agent polled first.
//
// The run records the requirement when it is queued, as a job with a become
// password records "become-file", and waits for an agent that has it.
func TestACheckoutRunWaitsForAnAgentThatAllowsCheckout(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	exec := func(q string) {
		t.Helper()
		if _, err := svc.db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`UPDATE git_repos SET last_sha = 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef' WHERE id = 'global'`)
	exec(`INSERT INTO jobs (uid, name, run_type, script_path, project_root, concurrency_policy, synced_at)
	      VALUES ('uid-proj-job', 'proj-job', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'Allow', '2026-01-01T00:00:00Z')`)
	// A project whose job already asks for something, and one that already asks
	// for checkout: the token is added once.
	exec(`INSERT INTO jobs (uid, name, run_type, script_path, project_root, requires_json, concurrency_policy, synced_at)
	      VALUES ('uid-proj-vault', 'proj-vault', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', '["vault"]', 'Allow', 't'),
	             ('uid-proj-said',  'proj-said',  'ansible', 'scripts/proj/site.yml', 'scripts/proj', '["checkout"]', 'Allow', 't')`)
	// A stored value that is not a list (a row edited by hand: nothing writes
	// one) is read as no requirements, and the token is still added. Without
	// that json_insert adds nothing, and the run asked for nothing at all.
	exec(`INSERT INTO jobs (uid, name, run_type, script_path, project_root, requires_json, concurrency_policy, synced_at)
	      VALUES ('uid-proj-null',   'proj-null',   'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'null',      'Allow', 't'),
	             ('uid-proj-string', 'proj-string', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', '"vault"',   'Allow', 't'),
	             ('uid-proj-junk',   'proj-junk',   'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'not json',  'Allow', 't'),
	             ('uid-proj-empty',  'proj-empty',  'ansible', 'scripts/proj/site.yml', 'scripts/proj', '',          'Allow', 't')`)
	// A body job asks for nothing.
	exec(`INSERT INTO jobs (uid, name, run_type, script, concurrency_policy, synced_at)
	      VALUES ('uid-body-job', 'body-job', 'ansible', '- hosts: all', 'Allow', 't')`)
	enqueue := func(job string) string {
		t.Helper()
		trace, err := scheduler.EnqueueRunWithID(ctx, svc.db, scheduler.EnqueueParams{
			JobName: job, RunType: "ansible", TriggerKind: "manual", TriggeredBy: "a@b.c",
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", job, err)
		}
		return trace
	}
	requires := func(trace string) string {
		t.Helper()
		var r sql.NullString
		if err := svc.db.QueryRowContext(ctx, `SELECT requires_json FROM runs WHERE id = ?`, trace).Scan(&r); err != nil {
			t.Fatal(err)
		}
		return r.String
	}

	for job, want := range map[string]string{
		"proj-vault":  `["vault","checkout"]`,
		"proj-said":   `["checkout"]`,
		"proj-null":   `["checkout"]`,
		"proj-string": `["checkout"]`,
		"proj-junk":   `["checkout"]`,
		"proj-empty":  `["checkout"]`,
		"body-job":    "",
	} {
		if got := requires(enqueue(job)); got != want {
			t.Errorf("the run of %s records the requirements %q, want %q", job, got, want)
		}
	}
	if _, err := svc.db.ExecContext(ctx, `DELETE FROM runs`); err != nil {
		t.Fatal(err)
	}

	trace := enqueue("proj-job")
	if got := requires(trace); got != `["checkout"]` {
		t.Fatalf("the run of a project job records the requirements %q, want the checkout token", got)
	}

	// An agent with Ansible and WITHOUT -allow-checkout does not take it. Nor
	// does the server's own runner, which has no such token: it ran a project
	// as a body, with the pin ignored.
	insertRunner(t, svc, "r-plain", "r-plain", "online", []string{"ansible"})
	if got, err := svc.claimRun(ctx, "r-plain", []string{"ansible"}, true); err != nil || got != nil {
		t.Fatalf("an agent that does not allow checkout claimed the checkout run: %+v (%v)", got, err)
	}
	insertRunner(t, svc, "r-local", "r-local", "online", []string{"ansible", "bash"})
	exec(`UPDATE runners SET kind = 'server' WHERE id = 'r-local'`)
	if got, err := Claim(ctx, svc.db, svc.log, ClaimRequest{RunnerID: "r-local", Caps: []string{"ansible", "bash"}, InjectionOK: true, Local: true, Actor: "local-runner"}); err != nil || got != nil {
		t.Fatalf("the local runner claimed the checkout run: %+v (%v)", got, err)
	}
	// It waits, and says for what.
	reason, err := execspec.UnclaimableReason(ctx, svc.db, trace)
	if err != nil || !strings.Contains(reason, "checkout") || !strings.Contains(reason, "-allow-checkout") {
		t.Errorf("the waiting run's reason = %q (%v), want it to name the token and what gives it", reason, err)
	}
	var status string
	if err := svc.db.QueryRowContext(ctx, `SELECT status FROM runs WHERE id = ?`, trace).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("the checkout run is %q (%v), want it still queued", status, err)
	}

	// An agent started with -allow-checkout takes it.
	insertRunner(t, svc, "r-checkout", "r-checkout", "online", []string{"ansible", "checkout"})
	got, err := svc.claimRun(ctx, "r-checkout", []string{"ansible", "checkout"}, true)
	if err != nil || got == nil || got.TraceID != trace {
		t.Fatalf("an agent that allows checkout did not claim the checkout run: %+v (%v)", got, err)
	}
}
