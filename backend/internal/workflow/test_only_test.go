package workflow

import "context"

// Test-only since 2.4.2: the server asks the AgencyScopes forms (R4, I2); these are what the older tests assert on (the review of 2.4.x).

// JobScopes returns the distinct effective scopes of every job referenced
// anywhere in the step graph, applying the same A11 step-source precedence the
// engine uses at run time. Its only caller is the API's workflow authorization
// guard (workflowScopesPermit), which enforces scope access and the RB-2 verb on
// the workflow trigger/patch/cancel the way runJob does per job.
//
// ⚠️ An UNSCOPED constituent job yields an EMPTY STRING entry, and that entry is
// load-bearing. This function used to filter empties out, which made the caller's
// `sc == ""` branch — the RB-26 rule that an unscoped job inside a workflow is
// unrestricted-only — unreachable dead code: a workflow of entirely unscoped jobs
// produced an empty list and therefore no check at all. Fixed in RF-6
// (the RBAC-fixes plan). If this is ever changed back to skipping
// empties, that guard silently stops running.
//
// GC-10 (v2.2.2): the list covers the jobs of every SUB-WORKFLOW too. It used to
// stop at the workflow's own job steps, because collectStepRefs deliberately
// does not flatten a child's jobs into the parent's lookup. That is right for
// resolution and was wrong for authorization: a workflow whose only step is a
// sub-workflow produced no scopes, so triggering, pausing or cancelling it was
// authorized on nothing while the engine went on to run the child's jobs with
// the caller as actor.
func (e *Engine) JobScopes(ctx context.Context, steps []Step, wfSource string) ([]string, error) {
	return e.JobScopesAt(ctx, steps, wfSource, "")
}

// JobScopesAt is JobScopes for a workflow whose home is known (see Home): the
// scopes of the jobs the engine will run for THAT workflow.
func (e *Engine) JobScopesAt(ctx context.Context, steps []Step, wfSource, home string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(scopes []string) {
		for _, sc := range scopes {
			if !seen[sc] {
				seen[sc] = true
				out = append(out, sc)
			}
		}
	}
	own, err := e.ownJobScopes(ctx, steps, wfSource, home)
	if err != nil {
		return nil, err
	}
	add(own)
	nested, err := e.SubWorkflowJobScopesAt(ctx, steps, wfSource, home)
	if err != nil {
		return nil, err
	}
	add(nested)
	return out, nil
}

// ownJobScopes is the scopes of the job steps this graph names directly.
func (e *Engine) ownJobScopes(ctx context.Context, steps []Step, wfSource, home string) ([]string, error) {
	defs, err := e.lookupJobDefs(ctx, steps, wfSource, home)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, jd := range defs {
		if !seen[jd.scope] {
			seen[jd.scope] = true
			out = append(out, jd.scope)
		}
	}
	return out, nil
}

// SubWorkflowJobScopes returns the scopes of every job reachable THROUGH the
// sub-workflow steps of a graph — the jobs the engine will run on the caller's
// behalf that the graph itself never names (GC-10). An unscoped job yields ""
// here exactly as it does in JobScopes, and for the same reason.
//
// Each child is resolved by loadChildWorkflow, the function the engine itself
// calls when the step runs, so the walk authorizes the workflow that would
// actually run and nothing else. Authorizing against every workflow that merely
// SHARES the name would be wrong in the other direction: names are per-agency,
// so another department could veto this one's trigger, pause and cancel by
// creating a workflow of the same name. A child that does not resolve
// contributes nothing — it will not run either.
//
// The answer is true at the moment it is given. A child that is disabled now
// and enabled before its step is reached runs on an authorization that did not
// see it; closing that window needs the run to carry the resolution it was
// authorized on, which it does not yet.
//
// Descent stops at MaxWorkflowDepth, the engine's own ceiling. A workflow is
// walked again when it is reached at a SHALLOWER depth than before: its
// subtree was cut off earlier at the ceiling, and from nearer the root more of
// it is reachable — the engine would run that part.
func (e *Engine) SubWorkflowJobScopes(ctx context.Context, steps []Step, wfSource string) ([]string, error) {
	return e.SubWorkflowJobScopesAt(ctx, steps, wfSource, "")
}

// SubWorkflowJobScopesAt is SubWorkflowJobScopes for a workflow whose home is
// known (see Home). Each child is walked from ITS OWN home: a Git child's
// steps resolve in the child's repository.
func (e *Engine) SubWorkflowJobScopesAt(ctx context.Context, steps []Step, wfSource, home string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	err := e.walkSubWorkflowJobs(ctx, steps, wfSource, home, func(jd jobDef) {
		if !seen[jd.scope] {
			seen[jd.scope] = true
			out = append(out, jd.scope)
		}
	})
	return out, err
}

// DescendantRunScopes returns the distinct scopes of the job runs that belong to
// the sub-workflow runs UNDER a workflow run — everything CancelTree would stop
// beyond the addressed run's own children (GC-10). An unbound run yields "".
func (e *Engine) DescendantRunScopes(ctx context.Context, wfTraceID string) ([]string, error) {
	pairs, err := e.DescendantRunAgencyScopes(ctx, wfTraceID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range pairs {
		if !seen[p.Scope] {
			seen[p.Scope] = true
			out = append(out, p.Scope)
		}
	}
	return out, nil
}
