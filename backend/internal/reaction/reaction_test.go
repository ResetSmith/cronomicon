package reaction

import "testing"

// The §2.3 outcome table, as a table. Each row is pinned to a case that would
// silently regress: an outcome mapping that drifts does not fail loudly, it
// fires the wrong automation at the wrong moment.

func TestNormalizeJob(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   Outcome
		event  bool
		why    string
	}{
		{"success", Success, true, "the ordinary case"},
		{"warning", Success, true, "RX-Q1 — it ran and completed; a warning is not a failure"},
		{"failure", Failure, true, "the ordinary case, incl. executor_lost / drain_timeout"},
		{"killed", Stopped, true, "RX-Q4 — an UNCLASSIFIED stop; the operator said nothing"},
		{"skipped", "", false, "CAL-8 suppression: nothing ran, so nothing may cascade"},
		{"queued", "", false, "not terminal"},
		{"running", "", false, "not terminal"},
		{"", "", false, "unknown ⇒ no event, never a guess"},
		{"some-future-status", "", false, "an unknown future status must not silently cascade"},
	} {
		got, ok := NormalizeJob(tc.status)
		if got != tc.want || ok != tc.event {
			t.Errorf("NormalizeJob(%q) = (%q, %v), want (%q, %v) — %s",
				tc.status, got, ok, tc.want, tc.event, tc.why)
		}
	}
}

// A dispositioned stop is indistinguishable from an ordinary run of that status
// HERE, and that is the design. Phase A folds the operator's choice into
// `status`, so this package never needs killed_by — and a stop recorded as a
// success must satisfy on_outcome: success, or Phase A's payoff does not reach
// reactions at all.
func TestDispositionedStopFollowsItsDisposition(t *testing.T) {
	if got, _ := NormalizeJob("success"); got != Success {
		t.Errorf("a stop recorded as success = %q, want success", got)
	}
	if got, _ := NormalizeJob("failure"); got != Failure {
		t.Errorf("a stop recorded as failure = %q, want failure", got)
	}
	if got, _ := NormalizeJob("warning"); got != Success {
		t.Errorf("a stop recorded as warning = %q, want success", got)
	}
	// And only the unclassified one reads stopped.
	if got, _ := NormalizeJob("killed"); got != Stopped {
		t.Errorf("an unclassified stop = %q, want stopped", got)
	}
}

func TestNormalizeWorkflow(t *testing.T) {
	for _, tc := range []struct {
		status    string
		cancelled bool
		want      Outcome
		event     bool
		why       string
	}{
		{"success", false, Success, true, "the ordinary case"},
		{"failure", false, Failure, true, "the ordinary case"},
		{"failure", true, Stopped, true, "cancelled wins over the status"},
		// The case the whole cancelled-flag rule exists for.
		{"success", true, Stopped, true,
			"a cancel landing during the FINAL step finalises success with cancelled=1; " +
				"reading status here would cascade a success that happened only because the cancel was too late"},
		{"skipped", false, "", false, "nothing ran"},
		{"running", false, "", false, "not terminal"},
	} {
		got, ok := NormalizeWorkflow(tc.status, tc.cancelled)
		if got != tc.want || ok != tc.event {
			t.Errorf("NormalizeWorkflow(%q, cancelled=%v) = (%q, %v), want (%q, %v) — %s",
				tc.status, tc.cancelled, got, ok, tc.want, tc.event, tc.why)
		}
	}
}

// A cancelled workflow that is ALSO skipped still emits nothing: the child was
// never started, so there is no completion to observe. Guards against the flag
// check being hoisted above the terminal-state check.
func TestCancelledButSkippedIsStillNoEvent(t *testing.T) {
	// A skipped workflow run with the cancel flag set is the "cancelled before
	// it ever started" shape. Today the engine cannot produce it, but the rule
	// that matters is that `skipped` means nothing ran — so if this ever starts
	// emitting, it is a deliberate decision and not a drift.
	if got, ok := NormalizeWorkflow("skipped", true); ok {
		t.Errorf("NormalizeWorkflow(skipped, cancelled) = (%q, true); a cancel flag must not "+
			"manufacture an event out of a run that never started", got)
	}
}

func TestMatches(t *testing.T) {
	for _, o := range []Outcome{Success, Failure, Stopped} {
		if !Matches(OnOutcomeAny, o) {
			t.Errorf("any should match %q — otherwise \"react whenever this finishes\" is a lie", o)
		}
		if !Matches(string(o), o) {
			t.Errorf("%q should match itself", o)
		}
	}
	// The pairing that must NOT hold: an unclassified stop does not satisfy a
	// failure reaction. This is the single most important negative in the
	// feature — it is what stops a deliberate human intervention from firing
	// the rollback automation.
	if Matches(string(Failure), Stopped) {
		t.Error("on_outcome=failure must not match a stopped run")
	}
	if Matches(string(Success), Stopped) {
		t.Error("on_outcome=success must not match a stopped run")
	}
	if Matches(string(Stopped), Failure) {
		t.Error("on_outcome=stopped must not match a genuine failure")
	}
}

func TestValidOnOutcome(t *testing.T) {
	for _, ok := range []string{"success", "failure", "stopped", "any"} {
		if !ValidOnOutcome(ok) {
			t.Errorf("%q should be a valid on_outcome", ok)
		}
	}
	for _, bad := range []string{"", "killed", "warning", "danger", "cancelled", "Success", "ANY"} {
		if ValidOnOutcome(bad) {
			t.Errorf("%q must not be a valid on_outcome", bad)
		}
	}
	// `killed` and `warning` are deliberately absent: they are runs.status
	// values, not reaction vocabulary. Accepting either would let an author
	// write a reaction that can never fire, because normalisation has already
	// mapped them to stopped and success respectively.
}

func TestDepthCeiling(t *testing.T) {
	if DepthExceeded(0) {
		t.Error("a first-generation reaction (depth 0 → 1) must be allowed")
	}
	if !DepthExceeded(MaxDepth - 1) {
		t.Errorf("depth %d → %d must be refused at the ceiling of %d", MaxDepth-1, MaxDepth, MaxDepth)
	}
	if !DepthExceeded(MaxDepth + 10) {
		t.Error("a depth already past the ceiling must stay refused")
	}
	// The ceiling bounds the CHAIN, so exactly MaxDepth-1 hops are possible from
	// a depth-0 origin. Pinned as a number because a fencepost slip here is
	// invisible until someone builds a five-deep chain in production.
	hops := 0
	for d := 0; !DepthExceeded(d); d++ {
		hops++
	}
	if hops != MaxDepth-1 {
		t.Errorf("chain length from depth 0 = %d hops, want %d", hops, MaxDepth-1)
	}
}
