package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// Phase A of the local runner gives it a row among the runners and leaves it
// claiming through the SSH executor's own query: nothing claims an
// `executor='runner'` run for it. So nothing that answers "can a runner take
// this run" or "may this scope be bound to that runner" may count it, online or
// not — or a scope bound to it would wait for ever while the stuck-run hint
// said something could take the run. Phase B, which makes it claim by the
// runner rule, deletes execspec.ClaimsByPollSQL and inverts this test.
func TestTheLocalRunnerIsNotARunnerExecutorClaimantYet(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	insertAgencyRow(t, svc, "a1", "alpha")
	// The local runner, on: online, the four shell types, secret injection on,
	// serving Global (its birth row) and alpha.
	insertRunner(t, svc, "local", "Local runner", "online", []string{"bash", "perl", "powershell", "python"})
	exec(`UPDATE runners SET kind = 'server', allow_secret_injection = 1 WHERE id = 'local'`)
	addRunnerAgency(t, svc, "local", "a1")
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-a', 'a-scope', 'cronomicon', ?)`, now())
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-a'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-a', 'a1')`)
	exec(`INSERT INTO runs(id, job_name, run_type, scope, agencies_json, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run', 'j', 'bash', 'a-scope', '["alpha"]', 'queued', 'test', 'manual', 'runner', ?)`, now())
	exec(`INSERT OR IGNORE INTO run_agencies(run_id, agency) VALUES('run', 'alpha')`)

	if ok, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, "run"); err != nil || ok {
		t.Errorf("EligibleOnlineRunnerForRun = %v, %v; nothing claims a runner-executor run for the local runner", ok, err)
	}
	if reason, err := execspec.UnclaimableReason(ctx, svc.db, "run"); err != nil || reason != "no runner is online" {
		t.Errorf("UnclaimableReason = %q, %v; want \"no runner is online\"", reason, err)
	}
	if ok, err := execspec.AgenciesHaveOnlineRunner(ctx, svc.db, []string{"alpha"}); err != nil || ok {
		t.Errorf("AgenciesHaveOnlineRunner = %v, %v; want false", ok, err)
	}
	if ok, err := execspec.RunnerEligibleForScope(ctx, svc.db, "sc-a", "local"); err != nil || ok {
		t.Errorf("RunnerEligibleForScope(local) = %v, %v; a scope must not be bound to it yet", ok, err)
	}

	// An agent in the same place is counted by all four, as before.
	insertRunner(t, svc, "agent", "agent", "online", []string{"bash"})
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'agent'`)
	addRunnerAgency(t, svc, "agent", "a1")
	if ok, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, "run"); err != nil || !ok {
		t.Errorf("with an agent online, EligibleOnlineRunnerForRun = %v, %v", ok, err)
	}
	if ok, err := execspec.AgenciesHaveOnlineRunner(ctx, svc.db, []string{"alpha"}); err != nil || !ok {
		t.Errorf("with an agent online, AgenciesHaveOnlineRunner = %v, %v", ok, err)
	}
	if ok, err := execspec.RunnerEligibleForScope(ctx, svc.db, "sc-a", "agent"); err != nil || !ok {
		t.Errorf("RunnerEligibleForScope(agent) = %v, %v", ok, err)
	}
}

// A re-enrolled agent is offered its old placement by name. The local runner
// never enrols, so the snapshot of an agent that was called what it is called
// is not an offer to it.
func TestTheLocalRunnerIsNeverOfferedAPlacement(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// An agent called "Local runner", bound to a Global scope, deregistered.
	insertRunner(t, svc, "old", "Local runner", "offline", []string{"bash"})
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-g', 'g-scope', 'cronomicon', ?)`, now())
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES('sc-g', 'old', 'Local runner', 'test', ?)`, now())
	svc.deregisterRunner(ctx, "old", "Local runner")
	// The local runner, and a re-enrolled agent of the same name.
	insertRunner(t, svc, "local", "Local runner", "online", []string{"bash"})
	exec(`UPDATE runners SET kind = 'server' WHERE id = 'local'`)
	insertRunner(t, svc, "new", "Local runner", "online", []string{"bash"})

	sugg, err := suggestionsFor(ctx, svc.db)
	if err != nil {
		t.Fatal(err)
	}
	if sugg["local"] != nil {
		t.Errorf("the local runner was offered an agent's placement: %+v", sugg["local"])
	}
	if sugg["new"] == nil {
		t.Error("the re-enrolled agent of that name lost its offer")
	}
}
