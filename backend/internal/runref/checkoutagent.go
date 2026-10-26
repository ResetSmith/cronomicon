package runref

import (
	"context"
	"database/sql"
	"errors"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// CheckoutNeedsAgent reports whether a run of a job is one that no registered
// agent could take because the job is a PROJECT and none that serves it is
// started with -allow-checkout (2.4.0, present defect 44).
//
// A project job's run requires the "checkout" token (the run-row writer adds
// it, present defect 39) and waits for an agent that advertises it. Waiting is
// only safe when there is an agent to wait FOR. A queued run that nothing will
// ever claim counts against the installation-wide concurrency cap, which
// counts queued runs as well as running ones, and nothing expires it: a
// scheduled project job with no such agent left one more waiting run at every
// fire, and after as many fires as the cap allows every other job's fire was
// skipped. So where no REGISTERED agent that serves the job allows checkout,
// the run is refused where it is produced, as a key-bound shell run is when no
// agent could deliver the key (KeyBindingsNeedAgent). Where one is registered
// and is offline or busy, the run waits for it, as every run waits for its
// runner.
//
// "Serves the job" is the claim's agency and binding rule, as
// KeyBindingsNeedAgent asks it: an agent of one of the scope's agencies
// (Global's, for a run with no scope or with a scope that is not there) and,
// where the scope names its runners, one of them. The server's own runner is
// no such agent: it advertises no "checkout".
//
// "Advertises" is what the CLAIM reads: the tokens the agent declares, less
// the ones an administrator has masked (execspec.EffectiveCapsSQL). An agent
// whose "checkout" is masked takes no project's run, so it is no agent to wait
// for: read as declared, its runs waited for ever and counted against the
// installation's concurrency cap, which is what this rule is there to stop.
//
// The job is found by its uid; a caller that has none names it by source and
// name, and it is then a project when one job holds the name and that job is
// one. scope is the scope the RUN would have.
func CheckoutNeedsAgent(ctx context.Context, database *sql.DB, jobUID, jobSource, jobName, scope string) (bool, error) {
	if jobSource == "" {
		jobSource = "git"
	}
	var project bool
	var err error
	if jobUID != "" {
		err = database.QueryRowContext(ctx,
			`SELECT COALESCE(project_root, '') <> '' FROM jobs WHERE uid = ?`, jobUID).Scan(&project)
	} else {
		err = database.QueryRowContext(ctx, `
			SELECT COALESCE(j.project_root, '') <> '' FROM jobs j
			 WHERE j.name = ?1 AND j.source = ?2
			   AND (SELECT COUNT(*) FROM jobs o WHERE o.name = ?1 AND o.source = ?2) = 1`, jobName, jobSource).Scan(&project)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // no such job, or a name two hold: not this question's
	}
	if err != nil || !project {
		return false, err
	}
	var agent bool
	err = database.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM runners rn
			 WHERE rn.kind <> 'server'
			   AND 'checkout' IN `+execspec.EffectiveCapsSQL("rn")+`
			   AND EXISTS (
			       SELECT 1 FROM runner_agencies ra
			        WHERE ra.runner_id = rn.id AND ra.agency_id = rn.owner_agency -- its owner's work only (§0a, question 29)
			          AND ((NOT EXISTS (SELECT 1 FROM scopes sc WHERE sc.name = ?1 AND sc.owner_agency = ?2) AND ra.agency_id = ?2)
			               OR ra.agency_id IN (SELECT sa.agency_id FROM scope_agencies sa
			                                     JOIN scopes sc ON sc.id = sa.scope_id
			                                    WHERE sc.name = ?1 AND sc.owner_agency = ?2)))
			   AND (NOT EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                     WHERE sc.name = ?1 AND sc.owner_agency = ?2)
			        OR EXISTS (SELECT 1 FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			                    WHERE sc.name = ?1 AND sc.owner_agency = ?2 AND sr.runner_id = rn.id)))`, scope, jobOwner(ctx, database, jobUID, jobSource, jobName)).Scan(&agent)
	if err != nil {
		return false, err
	}
	return !agent, nil
}

// ReasonCheckoutNeedsAgent is the stored reason of a run refused by
// CheckoutNeedsAgent. A SENTENCE, by the convention of
// ReasonKeyBindingNeedsAgent: History shows the stored reason verbatim, and a
// standing refusal is recognised by its text.
const ReasonCheckoutNeedsAgent = "Skipped: this job is a project, which only an agent started with -allow-checkout can run, and no such agent serves this job's scope"

// CodeCheckoutNeedsAgent is the API error code (422) the manual and token
// triggers answer with for the same refusal.
const CodeCheckoutNeedsAgent = "checkout_requires_agent"

// jobOwner is whose a job is (agencyid.JobSQL), found by uid, or by source and
// name for a caller with none: the agency its scope is looked for in (I2: a
// scope's name is unique within an agency only). Global's when it cannot be
// told, as such a run is.
func jobOwner(ctx context.Context, database *sql.DB, uid, source, name string) string {
	if source == "" {
		source = "git"
	}
	var a sql.NullString
	err := database.QueryRowContext(ctx, `SELECT `+agencyid.JobSQL("j")+` FROM jobs j
		WHERE CASE WHEN ?1 <> '' THEN j.uid = ?1 ELSE j.name = ?2 AND j.source = ?3 END
		ORDER BY j.rowid LIMIT 1`, uid, name, source).Scan(&a)
	if err != nil || !a.Valid {
		return agencyid.Global
	}
	return a.String
}

// scopeAgencyOr is an agency id, Global's for none.
func scopeAgencyOr(agency string) string {
	if agency == "" {
		return agencyid.Global
	}
	return agency
}

// CheckoutRefusal is the message for that answer: what is missing and what to
// do about it.
const CheckoutRefusal = "this job is a project, which only an agent started with -allow-checkout can run, and no such " +
	"agent serves this job's scope — start an agent of the scope's agency with -allow-checkout (it lists the " +
	"repository and holds a credential for it), or build the job on a script that is not a project"
