package execspec

import (
	"context"
	"strings"
	"testing"
)

// SB-1 — the scope binding in the two places that mirror claimRun's predicate:
// EligibleOnlineRunnerForRun (is there anyone at all) and UnclaimableReason
// (why not). A hint that disagreed with the claim query would send an operator
// looking in the wrong place, so both are pinned against the same situations.

// bindingFixture seeds a scope in agency alpha (the fixture's 'a1') and helpers
// to bind it and to queue a run on it.
type bindingFixture struct{ *unclaimFixture }

func newBindingFixture(t *testing.T) bindingFixture {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO scopes(id,name,source,created_at) VALUES('s-dmz','dmz-web','cronomicon','t')`)
	f.exec(`INSERT INTO scope_agencies(scope_id,agency_id) VALUES('s-dmz','a1')`)
	return bindingFixture{f}
}

func (f bindingFixture) bind(runnerID, name string) {
	f.exec(`INSERT INTO scope_runners(scope_id,runner_id,runner_name,bound_by,bound_at) VALUES('s-dmz',?,?,'test','t')`,
		runnerID, name)
}

func (f bindingFixture) scopedRun(id string) {
	f.exec(`INSERT INTO runs(id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind,
	                         executor, agencies_json, requires_json, created_at)
	        VALUES(?, 'j', 'git', 'bash', 'dmz-web', 'queued', 'seed', 'manual', 'runner', '["alpha"]', '[]', 't')`, id)
}

func (f bindingFixture) eligible(t *testing.T, runID string) bool {
	t.Helper()
	ok, _, err := EligibleOnlineRunnerForRun(context.Background(), f.pool, runID)
	if err != nil {
		t.Fatalf("EligibleOnlineRunnerForRun: %v", err)
	}
	return ok
}

// An unrestricted scope is untouched by the binding rule: an agency member
// claims, and there is nothing to explain.
func TestScopeBindingUnboundScopeIsUnchanged(t *testing.T) {
	f := newBindingFixture(t)
	f.runner("r-alpha", "online", `["bash"]`, "a1", 0)
	f.scopedRun("run-1")

	if !f.eligible(t, "run-1") {
		t.Error("an unbound scope's run has no eligible runner, want the agency member")
	}
	if got := f.reason(t, "run-1"); got != "" {
		t.Errorf("reason = %q, want none", got)
	}
}

// TestScopeBindingReasonsAreToldApart walks the three situations a restricted
// scope can be stuck in. Each fix moves the sentence on, and none of them may
// collapse into the generic "no online runner belongs to alpha" — that complaint
// is false here (r-other is online and in alpha) and would send the operator to
// the agency matrix instead of the scope.
func TestScopeBindingReasonsAreToldApart(t *testing.T) {
	f := newBindingFixture(t)
	f.runner("r-other", "online", `["bash"]`, "a1", 0) // in the agency, never named
	f.scopedRun("run-1")

	// 1. Bound to a runner whose row is gone.
	f.bind("r-gone", "runner-dmz-01")
	if f.eligible(t, "run-1") {
		t.Fatal("a scope bound only to a deregistered runner reports an eligible runner")
	}
	got := f.reason(t, "run-1")
	if !strings.Contains(got, "dmz-web") || !strings.Contains(got, "runner-dmz-01") ||
		!strings.Contains(got, "registered any more") {
		t.Errorf("deregistered binding = %q, want it to name the scope, the runner and say it is no longer registered", got)
	}

	// 2. The runner exists again but sits outside the scope's agency.
	f.runner("r-gone", "online", `["bash"]`, "", 0)
	got = f.reason(t, "run-1")
	if !strings.Contains(got, "in the scope's agency") {
		t.Errorf("out-of-agency binding = %q, want it to say the bound runner is not in the scope's agency", got)
	}

	// 3. In the agency, but offline.
	f.exec(`INSERT INTO runner_agencies(runner_id,agency_id) VALUES('r-gone','a1')`)
	f.exec(`UPDATE runners SET status='offline' WHERE id='r-gone'`)
	got = f.reason(t, "run-1")
	if !strings.Contains(got, "online and otherwise eligible") {
		t.Errorf("offline binding = %q, want it to say no bound runner is online and eligible", got)
	}

	// Fixed: online, in the agency, named.
	f.exec(`UPDATE runners SET status='online' WHERE id='r-gone'`)
	if !f.eligible(t, "run-1") {
		t.Error("the bound runner is online and in the agency, yet nothing is eligible")
	}
	if got = f.reason(t, "run-1"); got != "" {
		t.Errorf("reason = %q, want none once the bound runner can claim", got)
	}
}

// Nothing claims a row frozen onto the SSH executor since 2.3.0 (LR-42), and a
// queued one says so: the upgrade converts the rows that were waiting, so one
// that is still here would otherwise wait in silence.
func TestUnclaimableReasonForALeftoverSSHRun(t *testing.T) {
	f := newUnclaimFixture(t)
	f.exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind,
	                         executor, agencies_json, requires_json, created_at)
	        VALUES('run-ssh', 'j', 'git', 'bash', 'queued', 'seed', 'manual', 'ssh', '[]', '[]', 't')`)
	if got := f.reason(t, "run-ssh"); got != ReasonQueuedForSSHExecutor {
		t.Errorf("reason on a leftover ssh run = %q, want %q", got, ReasonQueuedForSSHExecutor)
	}
}

func TestScopeBindingPermits(t *testing.T) {
	f := newBindingFixture(t)
	ctx := context.Background()
	permits := func(scope, runnerID string) bool {
		t.Helper()
		ok, err := ScopeBindingPermits(ctx, f.pool, scope, runnerID)
		if err != nil {
			t.Fatalf("ScopeBindingPermits: %v", err)
		}
		return ok
	}
	if !permits("dmz-web", "r-any") || !permits("", "r-any") || !permits("never-created", "r-any") {
		t.Error("an unrestricted scope, no scope and an unknown scope must all permit any runner")
	}
	f.bind("r-dmz", "runner-dmz-01")
	if !permits("dmz-web", "r-dmz") {
		t.Error("the named runner is not permitted")
	}
	if permits("dmz-web", "r-any") {
		t.Error("a runner that is not named is permitted on a restricted scope")
	}
}
