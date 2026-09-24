package runref

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

// RA-24 — the unbound-run trap.
//
// A run with no effective scope carries an EMPTY agency snapshot, and that breaks
// a credential-consuming job two ways at once, both late and both opaque:
//
//  1. No agency-owned credential resolves. The visibility clause is
//     "no membership OR membership ∩ run.agencies ≠ ∅"; an owned row HAS membership,
//     which intersects nothing. The run reaches dispatch and dies with
//     409 reference_injection_failed — "reference X is unavailable for this run" —
//     a message describing a permissions problem the operator does not have.
//  2. No departmental runner can claim it. The general-pool rule (AG-Q3a) is
//     DISJOINT: an untagged run is claimable only by a runner with no agencies. On a
//     fully departmentalised fleet it queues forever.
//
// ⚠️ Counter-intuitively, being UNRESTRICTED makes this MORE likely, not less —
// unrestricted is exactly what permits an unbound run in the first place (RB-26), so
// the central operations team is the population most exposed to it.
//
// The plan (§11.1b) originally specified "a job with declared bindings triggered
// with no effective scope ⇒ 422". That is too blunt: a job binding only GLOBAL
// (unowned) rows runs perfectly well unbound, and refusing it would break working
// setups for a hazard it does not have. So the probe below asks the precise
// question instead — which of this owner's declared bindings actually fail to
// resolve for the run as it would be enqueued — using the same predicate dispatch
// uses, so the answer cannot drift from the failure it predicts.

// UnresolvableBindings returns the owner's declared bindings that would NOT resolve
// for a run with the given scope and agency snapshot, in declaration order.
//
// An empty result means every declared binding resolves and the run is safe to
// enqueue on this axis. Ambiguous bindings (RA-17, two departments owning one key)
// count as unresolvable: dispatch fails closed on them, so a caller refusing early
// refuses for a real reason.
//
// Callers should gate this on the run actually being unbound — that is the case
// worth paying a per-binding lookup for, and the case where the failure is
// otherwise unattributable. A BOUND run with a missing row is left alone
// deliberately: the catalogue row is often created minutes later by a different
// person (the same reasoning that keeps git sync advisory), and dispatch's
// fail-closed 409 names it well enough because the scope is right there in the run.
func UnresolvableBindings(ctx context.Context, database *sql.DB, owners []Owner, scope string, agencies []string) ([]Binding, error) {
	var declared []Binding
	for _, o := range owners {
		bs, err := ListBindings(ctx, database, o)
		if err != nil {
			return nil, err
		}
		declared = append(declared, bs...)
	}
	if len(declared) == 0 {
		return nil, nil
	}
	var blocked []Binding
	for _, b := range declared {
		_, found, err := LookupEntityID(ctx, database, b.Kind, b.Name, scope, agencies)
		if err != nil {
			// Ambiguity is a resolution verdict, not a fault: dispatch would fail
			// closed, so the binding is genuinely unusable for this run. Anything
			// else is a real DB error and must not be dressed up as a verdict.
			if errors.Is(err, ErrAmbiguousReference) {
				blocked = append(blocked, b)
				continue
			}
			return nil, err
		}
		if !found {
			blocked = append(blocked, b)
		}
	}
	return blocked, nil
}

// UnboundRunBlocked is RA-24's decision in one call: does this owner declare
// credentials that an UNBOUND run could not resolve? Returns the blocking bindings
// (empty ⇒ let it run). A run with any scope at all is not this function's business
// and returns nil immediately.
func UnboundRunBlocked(ctx context.Context, database *sql.DB, owners []Owner, scope string, agencies []string) ([]Binding, error) {
	if scope != "" || len(agencies) > 0 {
		return nil, nil
	}
	return UnresolvableBindings(ctx, database, owners, "", nil)
}

// RunOwners is the binding-owner set for a run, mirroring
// runner/manifest.go collectReferenceBindings EXACTLY: the job, plus its script
// when it references one. A probe that checked only the job would miss every
// script-declared binding and cheerfully enqueue a run that dies at dispatch —
// which is the failure RA-24 exists to pre-empt, reproduced by its own guard.
//
// R2F-1: jobUID identifies WHICH job when two departments own one of that name.
// Pass it whenever the caller holds it — every one does, from a fetched job row
// or from runs.job_uid — so the probe reads the same bindings dispatch will.
// Empty is legal and means the legacy name-keyed set (see Owner.UID).
func RunOwners(jobSource, jobName, jobUID, scriptRef string) []Owner {
	js := jobSource
	if js == "" {
		js = "git"
	}
	owners := []Owner{{Kind: "job", Source: js, Name: jobName, UID: jobUID}}
	if scriptRef != "" {
		owners = append(owners, Owner{Kind: "script", Name: scriptRef})
	}
	return owners
}

// UnboundRefusal renders the operator-facing sentence for a blocked unbound run.
// One shape, so the trigger's 422, the scheduler's skipped record and the workflow
// step's failure all say the same thing — an operator comparing a History row with
// an API response must not have to work out that they are the same problem.
//
// It names the FIRST blocking binding rather than all of them: the remedy is
// identical for every one (bind a scope), and a list invites fixing them one at a
// time. Naming which reference is safe — the caller already declared it on the job.
func UnboundRefusal(blocked []Binding) string {
	if len(blocked) == 0 {
		return ""
	}
	more := ""
	if len(blocked) > 1 {
		more = " (and " + strconv.Itoa(len(blocked)-1) + " more)"
	}
	return "this job consumes department-owned credentials — " +
		string(blocked[0].Kind) + " " + blocked[0].Name + more +
		" resolves for no department when the run has no scope; bind a scope to run it"
}

// QueuedReasonUnboundReferences is the runs.queued_reason marker for RA-24's
// refusal on the automated paths, where there is no HTTP response to carry the
// sentence. Terminal, like every other queued_reason value.
const QueuedReasonUnboundReferences = "unbound_references"
