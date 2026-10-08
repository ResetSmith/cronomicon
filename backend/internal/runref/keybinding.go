package runref

import (
	"context"
	"database/sql"
	"strconv"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// KeyBindingsNeedAgent returns the declared SSH-key bindings (KindKey) of a
// shell run that no registered agent could take, and nil otherwise (LR-47;
// the KB band's refusal, narrowed).
//
// A bound key is delivered as a file on the machine that runs the job. An agent
// can do that; the local runner connects FROM the server TO the target, so it
// would have to place the key on the target host — a different problem, dropped
// in KB and not rebuilt (it needs a per-host trust flag, a cleanup path for a
// killed session and a Windows story). The claim therefore keeps the local
// runner away from a run that binds a key (execspec.RunBindsKeySQL), and such a
// run waits for an agent.
//
// Waiting is only safe when there is an agent to wait FOR. A queued row nothing
// will ever claim counts against the fleet cap, holds its Forbid key, fills a
// Queue and suppresses the missed-run alert. So when no registered agent serves
// the run's agency and passes its scope's binding — whether or not one is
// online, which is the ordinary wait every agent-bound run already has — the
// run is refused here instead, the way every producer refused a key-bound run
// on the SSH executor until 2.3.0.
//
// Shell run types only: an ansible or terraform run was never the server's to
// take, and one that binds a key waits for a capable agent as it always has.
//
// Every run producer calls this beside UnboundRunBlocked, with the scope the
// run will carry ("" for a run with no scope, which is Global's).
//
// perRun are the references added to this one run (the manual trigger's
// `references`); the automatic producers have none and pass nil. A key among
// them counts like a declared one, as it does in the claim.
func KeyBindingsNeedAgent(ctx context.Context, database *sql.DB, owners []Owner, perRun []Binding, runType, scope string) ([]Binding, error) {
	if !execspec.SupportedRunType(runType) {
		return nil, nil
	}
	var keys []Binding
	for _, b := range perRun {
		if b.Kind == KindKey {
			keys = append(keys, b)
		}
	}
	for _, o := range owners {
		bs, err := ListBindings(ctx, database, o)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			if b.Kind == KindKey {
				keys = append(keys, b)
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// An agent that serves one of the scope's agencies and is named by the
	// scope's binding, if it has one — the claim's agency and binding rules,
	// asked of every registered agent. A run with no scope is Global's, and so
	// is one whose scope names no scope row (execspec.ScopeAgencies stamps it
	// Global; this must agree, or a run a Global agent would take is refused).
	//
	// Capability and the secret-injection flag are deliberately not asked. An
	// agent that lacks one is an agent an operator can fix, and the run that
	// waits for it carries the reason; what is refused here is a run with
	// nobody to wait for at all.
	var agent bool
	err := database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM runners rn
			 WHERE rn.kind <> 'server'
			   AND EXISTS (
			       SELECT 1 FROM runner_agencies ra
			        WHERE ra.runner_id = rn.id
			          AND ((NOT EXISTS (SELECT 1 FROM scopes sc WHERE sc.name = ?1) AND ra.agency_id = 'global')
			               OR ra.agency_id IN (SELECT sa.agency_id FROM scope_agencies sa
			                                     JOIN scopes sc ON sc.id = sa.scope_id
			                                    WHERE sc.name = ?1)))
			   AND (NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                     WHERE sc.name = ?1)
			        OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                    WHERE sc.name = ?1 AND sr.runner_id = rn.id)))`, scope).Scan(&agent)
	if err != nil {
		return nil, err
	}
	if agent {
		return nil, nil
	}
	return keys, nil
}

// ReasonKeyBindingNeedsAgent is the stored queued_reason for a fire refused by
// KeyBindingsNeedAgent. A SENTENCE, per the scheduler's convention
// (reasonJobPaused, reasonConcurrencyCap): History shows queued_reason
// verbatim, there is no token→text map on the frontend.
const ReasonKeyBindingNeedsAgent = "Skipped: this job binds an SSH key, which only an agent can deliver, and no agent serves this job's scope"

// CodeKeyBindingNeedsAgent is the API error code (422) the manual and token
// triggers return for the same refusal. The value predates 2.3.0, when the
// refusal was about the SSH executor; callers match on it, so it is kept.
const CodeKeyBindingNeedsAgent = "key_binding_requires_runner"

// KeyBindingRefusal is the message for a KeyBindingsNeedAgent refusal: it names
// the first key by its REFERENCE (the alias when one was declared — RA-5 —
// since that is the name the job body reads) and says what to do instead.
func KeyBindingRefusal(keys []Binding) string {
	if len(keys) == 0 {
		return ""
	}
	more := ""
	if len(keys) > 1 {
		more = " (and " + strconv.Itoa(len(keys)-1) + " more)"
	}
	return "this job binds SSH key " + keys[0].InjectReference() + more +
		", which only an agent can deliver as a file, and no agent serves this job's scope " +
		"(the local runner connects from the server and cannot place a key on the target) — " +
		"enrol an agent for the scope's agency, or bind the key as a Secret and write the file in the job body"
}
