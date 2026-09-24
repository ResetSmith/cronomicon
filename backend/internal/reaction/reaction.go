// Package reaction resolves whether one definition's completion should trigger
// another's run (RX, the reactions-update plan §2).
//
// It is deliberately pure: no database, no clock, no logging. Everything here
// is a total function over values the reactor loop has already read, which is
// what makes the outcome table below testable as a table rather than through a
// running scheduler. internal/calendar was carved out the same way and for the
// same reason.
//
// The one idea worth holding onto: a reaction is EDGE-TRIGGERED. There is no
// "prereq satisfied" state anywhere, nothing accumulates, and nothing can be
// inspected as "currently waiting" — because it never waits. It either observes
// a completion and fires, or there is no event at all.
package reaction

// Outcome is the normalised terminal result a reaction matches on.
//
// The two entity types do not share a terminal vocabulary — workflows are
// binary, jobs have five terminal states — so both are projected onto this
// three-value set before any matching happens. Normalising at the edge is what
// lets the rest of the feature be kind-agnostic (§2.2).
type Outcome string

const (
	// Success — the work ran to completion. Includes `warning` (RX-Q1).
	Success Outcome = "success"
	// Failure — the work ran and went wrong, including system failures like
	// executor_lost and drain_timeout.
	Failure Outcome = "failure"
	// Stopped — a human ended it and did not say what it meant (RX-Q4).
	//
	// This exists because both flat alternatives fail loudly in opposite
	// directions. Folding it into failure means cancelling a deploy because you
	// spotted a problem fires the "deploy failed → roll back, page on-call"
	// reaction while you are already hands-on — converting a deliberate human
	// intervention into an automated response. Folding it into nothing means
	// killing a hung job silently skips the downstream cleanup that releases a
	// lock, precisely when something is already wrong, and makes `any` a lie.
	Stopped Outcome = "stopped"
)

// MaxDepth is the runtime reaction-chain ceiling (RX-Q6).
//
// A Go constant, not a setting, and deliberately so. Both idioms exist in this
// codebase — maxConcurrent is a settings row, pendingMissGrace is a const — and
// this is a safety backstop rather than a tuning knob. Exposing it invites
// raising it to 50, which converts a contained runaway into an uncontained one.
//
// It catches what static cycle detection cannot see: a cycle that closes
// through a workflow's step graph rather than through reaction edges, and the
// cross-plane TOCTOU where two authors land conflicting edges on the API and
// Git planes simultaneously (§2.8).
const MaxDepth = 5

// OnOutcomeAny is the wildcard. It matches all three outcomes and never a
// non-event — "react whenever this finishes" cannot mean "react when it didn't
// run".
const OnOutcomeAny = "any"

// ValidOnOutcome reports whether s is a legal `on_outcome` value. Mirrors the
// CHECK constraint on reactions.on_outcome; kept here too so authoring paths can
// reject a bad value with a message instead of a constraint violation.
func ValidOnOutcome(s string) bool {
	switch s {
	case string(Success), string(Failure), string(Stopped), OnOutcomeAny:
		return true
	}
	return false
}

// NormalizeJob projects a `runs.status` value onto an Outcome.
//
// ok=false means NO EVENT — the run reached no terminal state a reaction can
// observe, so nothing fires and no delivery is recorded.
//
// # Why killed_by is not a parameter
//
// After Phase A a dispositioned stop writes the operator's chosen outcome into
// `status` itself, so the disposition is already folded in by the time we read
// the row: a stop recorded as a success IS status='success'. Only an
// UNCLASSIFIED stop remains status='killed', and that is exactly the case
// `stopped` names. Reading killed_by here would double-count the same fact and
// would break the moment an operator records a stop as a success — which is the
// whole point of Phase A.
//
// # Why skipped is not an event
//
// CAL-8 records a calendar-suppressed fire as a terminal `skipped` run
// precisely so it is queryable. Nothing ran, so nothing may cascade. The same
// covers a cancelled workflow's not-yet-started children, which Cancel marks
// skipped: they emit nothing, so one cancel produces exactly ONE `stopped`
// event at the workflow level rather than a scatter of child events.
func NormalizeJob(status string) (Outcome, bool) {
	switch status {
	case "success":
		return Success, true
	case "warning":
		// RX-Q1 — it ran and it completed. This is the one mapping an operator
		// will not guess, which is why the authoring surface has to state it: a
		// job that habitually exits with warnings would otherwise look like a
		// reaction that fires at random.
		return Success, true
	case "failure":
		// executor_lost and drain_timeout live here too. They are the system
		// failing, not an operator intervening, and a failure reaction should
		// fire for them.
		return Failure, true
	case "killed":
		return Stopped, true
	case "skipped":
		return "", false
	default:
		// queued, running, or anything a future migration adds. Not terminal ⇒
		// not an event. Defaulting to "no event" rather than guessing is what
		// keeps an unknown future status from silently cascading.
		return "", false
	}
}

// NormalizeWorkflow projects a workflow_runs row onto an Outcome.
//
// # Why cancelled is read instead of status
//
// A cancelled workflow is `stopped` REGARDLESS of its final status, keyed off
// the flag and not the status column. This is not defensive coding. A soft
// cancel only halts the walk BETWEEN steps, so a cancel arriving during the
// final step is never observed and the run finalises **success** with
// cancelled=1. Reading the status there would cascade a success that happened
// only because the cancel was a moment too late — the exact opposite of what
// the operator expressed.
// # Why the terminal check comes FIRST
//
// The cancelled flag is checked only after the status is known to describe a
// run that actually executed. The flag exists to correct a status that LIES
// about a run that ran (the too-late cancel above); it is not a licence to
// manufacture an event out of a run that never started. A `skipped` workflow
// run means nothing happened, and nothing happening cannot cascade — whether or
// not somebody pressed cancel on the way past.
func NormalizeWorkflow(status string, cancelled bool) (Outcome, bool) {
	switch status {
	case "skipped":
		return "", false
	case "success", "warning", "failure", "killed":
		// Terminal, and the run executed. Now the flag may speak.
	default:
		// queued, running, or an unknown future status.
		return "", false
	}
	if cancelled {
		return Stopped, true
	}
	switch status {
	case "success":
		return Success, true
	case "warning":
		return Success, true
	case "failure":
		return Failure, true
	case "killed":
		// The engine never writes this today (it writes success/failure and
		// carries cancellation as a flag), but the CHECK permits it and a
		// future writer might. Treated as the unclassified stop it is on the
		// job side, rather than falling through to "no event" and silently
		// dropping a terminal transition.
		return Stopped, true
	default:
		// Unreachable: the guard above admits only the four terminal statuses.
		return "", false
	}
}

// Matches reports whether a reaction authored with onOutcome should fire for an
// observed outcome. `any` matches all three real outcomes; it never sees a
// non-event, because callers do not reach here without ok=true.
func Matches(onOutcome string, observed Outcome) bool {
	if onOutcome == OnOutcomeAny {
		return true
	}
	return onOutcome == string(observed)
}

// DepthExceeded reports whether a downstream run at this inherited depth would
// breach the ceiling. The downstream run carries upstreamDepth+1, so the check
// is against that successor value rather than the upstream's own.
func DepthExceeded(upstreamDepth int) bool {
	return upstreamDepth+1 >= MaxDepth
}
