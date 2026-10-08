package runner

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// The local runner claims by the runner rule (LR-40): the one claim, for its
// own row. So everything that answers "can a runner take this run" or "may this
// scope be bound to that runner" counts it like an agent — with the one clause
// that is its alone: it does not take a run whose job binds an SSH key (LR-47).
// And when it is the runner that would take a run and it is turned off, the
// reason says that, not "no runner is online".
func TestTheLocalRunnerClaimsByTheRunnerRule(t *testing.T) {
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
	shell := []string{"bash", "perl", "powershell", "python"}
	insertRunner(t, svc, "local", "Local runner", "online", shell)
	exec(`UPDATE runners SET kind = 'server', allow_secret_injection = 1 WHERE id = 'local'`)
	addRunnerAgency(t, svc, "local", "a1")
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-a', 'a-scope', 'cronomicon', ?)`, now())
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-a'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-a', 'a1')`)
	queue := func(id, jobUID string) {
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, agencies_json, status, triggered_by, trigger_kind, executor, created_at)
		      VALUES (?, 'j', 'cronomicon', ?, 'bash', 'a-scope', '["alpha"]', 'queued', 'test', 'manual', 'runner', ?)`, id, jobUID, now())
		exec(`INSERT OR IGNORE INTO run_agencies(run_id, agency) VALUES(?, 'alpha')`, id)
	}
	queue("plain", "uid-plain")
	queue("keyed", "uid-keyed")
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, owner_uid, ref_kind, ref_name, created_at)
	      VALUES('job', 'cronomicon', 'j', 'uid-keyed', 'key', 'deploy_key', ?)`, now())
	mirrors := func(run string) (eligible bool, reason string) {
		t.Helper()
		eligible, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, run)
		if err != nil {
			t.Fatalf("EligibleOnlineRunnerForRun(%s): %v", run, err)
		}
		reason, err = execspec.UnclaimableReason(ctx, svc.db, run)
		if err != nil {
			t.Fatalf("UnclaimableReason(%s): %v", run, err)
		}
		return eligible, reason
	}

	// On: the mirrors count it for the plain run, and it may be bound.
	if ok, reason := mirrors("plain"); !ok || reason != "" {
		t.Errorf("the local runner on: eligible=%v reason=%q, want eligible and no reason", ok, reason)
	}
	if ok, err := execspec.AgenciesHaveOnlineRunner(ctx, svc.db, []string{"alpha"}); err != nil || !ok {
		t.Errorf("AgenciesHaveOnlineRunner = %v, %v; want true", ok, err)
	}
	if ok, err := execspec.RunnerEligibleForScope(ctx, svc.db, "sc-a", "local"); err != nil || !ok {
		t.Errorf("RunnerEligibleForScope(local) = %v, %v; a scope it serves may be bound to it", ok, err)
	}
	// The key-bound run is not its to take, and the reason says why.
	if ok, reason := mirrors("keyed"); ok || reason != execspec.ReasonKeyNeedsAgent {
		t.Errorf("a key-bound run with only the local runner: eligible=%v reason=%q, want %q", ok, reason, execspec.ReasonKeyNeedsAgent)
	}

	// The claim agrees with both: as the local runner it takes the plain run
	// and never the keyed one.
	local := ClaimRequest{RunnerID: "local", Caps: shell, InjectionOK: true, Local: true, Actor: "local-runner"}
	got, err := Claim(ctx, svc.db, svc.log, local)
	if err != nil || got == nil || got.TraceID != "plain" {
		t.Fatalf("the local runner's claim = %+v, %v; want the plain run", got, err)
	}
	if got, err := Claim(ctx, svc.db, svc.log, local); err != nil || got != nil {
		t.Fatalf("the local runner claimed a key-bound run: %+v, %v", got, err)
	}
	var actor, runnerName string
	if err := svc.db.QueryRow(`SELECT actor, COALESCE(runner_name, '') FROM activity WHERE kind = 'run-start' AND trace_id = 'plain'`).
		Scan(&actor, &runnerName); err != nil || actor != "local-runner" || runnerName != "Local runner" {
		t.Errorf("run-start row: actor=%q runner=%q (%v), want local-runner / Local runner (LR-53)", actor, runnerName, err)
	}

	// Off: it is the runner that would take a plain run, and the reason says so.
	queue("waits", "uid-plain")
	exec(`UPDATE runners SET status = 'offline' WHERE id = 'local'`)
	if ok, reason := mirrors("waits"); ok || reason != execspec.ReasonLocalRunnerOff {
		t.Errorf("the local runner off: eligible=%v reason=%q, want %q", ok, reason, execspec.ReasonLocalRunnerOff)
	}
	// Off leaves a scope bound only to it closed (LR-43): the binding outlives
	// the switch, the run waits, and the reason is still the switch.
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_by, bound_at) VALUES('sc-a', 'local', 'Local runner', 'test', ?)`, now())
	if ok, reason := mirrors("waits"); ok || reason != execspec.ReasonLocalRunnerOff {
		t.Errorf("off, on a scope bound to it: eligible=%v reason=%q, want %q", ok, reason, execspec.ReasonLocalRunnerOff)
	}
	insertRunner(t, svc, "bystander", "bystander", "online", []string{"bash"})
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'bystander'`)
	addRunnerAgency(t, svc, "bystander", "a1")
	if got, err := Claim(ctx, svc.db, svc.log, ClaimRequest{RunnerID: "bystander", Caps: []string{"bash"}, InjectionOK: true, Actor: "runner:bystander"}); err != nil || got != nil {
		t.Fatalf("an agent that is not bound took the run of a scope bound to the local runner: %+v, %v", got, err)
	}
	exec(`DELETE FROM runners WHERE id = 'bystander'`)
	exec(`DELETE FROM scope_runners`)
	// It would NOT take the keyed one, so that run's reason is not "turn it on".
	if _, reason := mirrors("keyed"); reason == execspec.ReasonLocalRunnerOff {
		t.Errorf("a key-bound run blamed the local runner being off: %q", reason)
	}

	// An agent in the same place takes the keyed run (it is flagged for
	// injection, which a key binding needs on an agent).
	insertRunner(t, svc, "agent", "agent", "online", []string{"bash"})
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'agent'`)
	addRunnerAgency(t, svc, "agent", "a1")
	exec(`UPDATE runners SET allow_secret_injection = 1 WHERE id = 'agent'`)
	if ok, reason := mirrors("keyed"); !ok || reason != "" {
		t.Errorf("with an agent online, the key-bound run: eligible=%v reason=%q", ok, reason)
	}
	if got, err := Claim(ctx, svc.db, svc.log, ClaimRequest{RunnerID: "agent", Caps: []string{"bash"}, InjectionOK: true, Actor: "runner:agent"}); err != nil || got == nil {
		t.Fatalf("the agent's claim = %+v, %v; want a run", got, err)
	}
}

// A run that waited and was then claimed does not carry its "why is this
// waiting" sentence into History.
func TestTheClaimClearsTheWaitingReason(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	insertRunner(t, svc, "r1", "r1", "online", []string{"bash"})
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, queued_reason, triggered_by, trigger_kind, executor, created_at)
		VALUES ('run', 'j', 'bash', 'queued', 'no runner is online', 'test', 'manual', 'runner', ?)`, now()); err != nil {
		t.Fatal(err)
	}
	if got, err := svc.claimRun(ctx, "r1", []string{"bash"}, false); err != nil || got == nil {
		t.Fatalf("claim = %+v, %v", got, err)
	}
	var reason sql.NullString
	if err := svc.db.QueryRow(`SELECT queued_reason FROM runs WHERE id = 'run'`).Scan(&reason); err != nil || reason.Valid {
		t.Errorf("queued_reason after the claim = %q (%v), want NULL", reason.String, err)
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

// A key added to ONE run (the Run dialog's references, kept in the run's
// override envelope) keeps the local runner away like a declared one. Left out
// of the claim, the same run would succeed on an agent and fail on the server
// according to which asked first.
func TestAKeyNamedOnTheRunKeepsTheLocalRunnerAway(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	shell := []string{"bash", "perl", "powershell", "python"}
	insertRunner(t, svc, "local", "Local runner", "online", shell)
	exec(`UPDATE runners SET kind = 'server', allow_secret_injection = 1 WHERE id = 'local'`)
	for id, override := range map[string]string{
		"adhoc-key":    `{"references":[{"kind":"secret","name":"TOKEN"},{"kind":"key","name":"deploy_key"}]}`,
		"adhoc-secret": `{"references":[{"kind":"secret","name":"TOKEN"}]}`,
		"junk":         `not json`,
	} {
		exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, override_json, created_at)
		      VALUES (?, 'j', 'bash', 'queued', 'test', 'manual', 'runner', ?, ?)`, id, override, now())
	}
	local := ClaimRequest{RunnerID: "local", Caps: shell, InjectionOK: true, Local: true, Actor: "local-runner"}
	taken := map[string]bool{}
	for {
		got, err := Claim(ctx, svc.db, svc.log, local)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			break
		}
		taken[got.TraceID] = true
	}
	if taken["adhoc-key"] || !taken["adhoc-secret"] || !taken["junk"] {
		t.Errorf("the local runner took %v; want the secret-only run and the unreadable one, never the run with a key", taken)
	}
	if ok, _, err := execspec.EligibleOnlineRunnerForRun(ctx, svc.db, "adhoc-key"); err != nil || ok {
		t.Errorf("EligibleOnlineRunnerForRun for the run with a key = %v, %v; want false", ok, err)
	}
	if reason, err := execspec.UnclaimableReason(ctx, svc.db, "adhoc-key"); err != nil || reason != execspec.ReasonKeyNeedsAgent {
		t.Errorf("its reason = %q, %v; want %q", reason, err, execspec.ReasonKeyNeedsAgent)
	}
}
