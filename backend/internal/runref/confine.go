package runref

import (
	"context"
	"database/sql"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// Confinement at run time (2.4.0, GR-15).
//
// A job that comes from an AGENCY's repository runs on that agency's scopes
// and on no other. Sync refuses a file that says otherwise (GR-14), but sync
// is not the control: a scope can be given to another agency after the sync
// that accepted the job, a run can be asked for on a scope other than the
// job's own, and a repository may not sync again for a long time. What stops
// the run is this, asked wherever a run is produced.
//
// A job built in the app is confined by it too when it is JOINED to a script
// of an agency's repository (jobs.script_uid), since 2.4.0 Phase R5 (present
// defect 40). Such a job holds a copy of its script, and for a body kept as a
// file the copy is a path: the file is read in that repository at dispatch.
// The composer joins a job only to its own agency's repository's script, or to
// Global's. If the job's scope is given to another agency afterwards, the
// first agency's committers would go on writing the body of a job that runs on
// the other's hosts. What a repository may reach is the rule, and it reaches a
// job through its script as well as through its own file.
//
// A job of Global's repository, and a job built in the app on Global's script,
// are confined to Global's scopes, or none (a run with no scope is Global's),
// since §0a (IN-3; until then the first might name any agency's scope, and the
// second run on any; the second is the owner's yes to question 31). A job
// built in the app on no script is not confined by this.

// ReasonRepoScopeMismatch is what is stored (runs.queued_reason, a reaction's
// delivery, a file sighting's refusal, a pending run's miss) and shown. A
// SENTENCE, by the convention of ReasonKeyBindingNeedsAgent: History shows the
// stored reason verbatim, and a standing refusal is recognised by its text.
const ReasonRepoScopeMismatch = "Refused: this job, or the script it is built on, comes from an agency's repository, and the scope it would run on does not belong to that agency"

// CodeRepoScopeMismatch is the API error code (422) the manual and token
// triggers answer with for the same refusal.
const CodeRepoScopeMismatch = "repo_scope_mismatch"

// RepoScopeMismatch reports whether a run of a job on a scope is to be refused
// because the job, or the script a job built in the app is joined to, comes
// from an agency's repository and the scope is not that agency's: another
// agency's, Global's, several agencies', none at all, or one that is not
// there. scope is the scope the RUN would have, which for a manual run may not
// be the job's own.
//
// The job is found by its uid. A caller that has none (a run parked before a
// run recorded one) names it by source and name, and then every job of that
// source and name that an agency's repository reaches is asked: one that does
// not fit refuses the run. That is stricter than it need be, on a path that is
// not taken by anything written since 1010.
func RepoScopeMismatch(ctx context.Context, database *sql.DB, jobUID, jobSource, jobName, scope string) (bool, error) {
	// Which jobs a caller with no uid means: the Git ones of the name (the
	// default source), or the ones built in the app.
	byNameGit, byNameApp := 0, 0
	if jobUID == "" {
		if jobSource == "" || jobSource == "git" {
			byNameGit = 1
		} else {
			byNameApp = 1
		}
	}
	const counts = `
		SELECT (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?2),
		       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		         WHERE sc.name = ?2 AND sa.agency_id = g.agency_id)`
	// The third column says the row is Global's repository's: for it, a run
	// with no scope is its own. The fourth, that it is a job built in the app
	// on Global's script: for it a run is refused only on a scope that another
	// agency holds (question 31), since a job on no scope, or on a scope that
	// is not there, is Global's, as its runs are.
	rows, err := database.QueryContext(ctx, counts+`, g.id = ?3, 0
		  FROM jobs j JOIN git_repos g ON g.id = j.repo_id
		 WHERE j.source = 'git'
		   AND ((?1 <> '' AND j.uid = ?1) OR (?5 = 1 AND j.name = ?4))
		UNION ALL`+counts+`, g.id = ?3, g.id = ?3
		  FROM jobs j
		  JOIN scripts s ON s.uid = j.script_uid
		  JOIN git_repos g ON g.id = s.repo_id
		 WHERE j.source <> 'git'
		   AND ((?1 <> '' AND j.uid = ?1) OR (?6 = 1 AND j.name = ?4 AND j.source = ?7))`,
		jobUID, scope, repoid.Global, jobName, byNameGit, byNameApp, jobSource)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var agencies, own int
		var global, onGlobalScript bool
		if err := rows.Scan(&agencies, &own, &global, &onGlobalScript); err != nil {
			return false, err
		}
		if global && scope == "" {
			continue
		}
		if onGlobalScript {
			if agencies > 0 && (agencies != 1 || own != 1) {
				return true, nil
			}
			continue
		}
		if scope == "" || agencies != 1 || own != 1 {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ReasonWorkflowScopeMismatch is the stored reason of a workflow step refused
// by ScopeOutsideAgency.
const ReasonWorkflowScopeMismatch = "Refused: this step would run on a scope that is not its workflow's agency's: a workflow runs its own agency's jobs only"

// ScopeOutsideAgency reports whether a scope is one a workflow of the agency
// agencyID may NOT run a step on: the scope is not that agency's alone
// (another agency's, several agencies', not there), or there is none and the
// agency is not Global (a run with no scope is Global's).
//
// RepoScopeMismatch asks about a JOB. A workflow needs it asked of its own
// agency: a workflow is its agency's, and its jobs must be too (§0a, IN-8).
// Until §0a this was asked of a workflow of an agency's repository only;
// Global's repository's and a workflow built in the app might run any
// agency's job, the first for GR-16, the second as GR-7's spanning workflow.
// What spans at an upgrade stops here, step by step, with this reason (§0a,
// open point 1).
func ScopeOutsideAgency(ctx context.Context, database *sql.DB, agencyID, scope string) (bool, error) {
	if scope == "" {
		return agencyID != agencyid.Global, nil
	}
	var agencies, own int
	err := database.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?1),
		       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		         WHERE sc.name = ?1 AND sa.agency_id = ?2)`, scope, agencyID).Scan(&agencies, &own)
	if err != nil {
		return false, err
	}
	return agencies != 1 || own != 1, nil
}

// ScopeOutsideRepository is ScopeOutsideAgency for the agency of a
// repository. No repository ("") confines nothing; a repository that is not
// there has no agency to run for, and confines everything.
func ScopeOutsideRepository(ctx context.Context, database *sql.DB, repoID, scope string) (bool, error) {
	if repoID == "" {
		return false, nil
	}
	var agency string
	err := database.QueryRowContext(ctx, `SELECT agency_id FROM git_repos WHERE id = ?`, repoID).Scan(&agency)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return ScopeOutsideAgency(ctx, database, agency, scope)
}

// WorkflowAgencySQL and DefinitionAgencySQL: agencyid's, here for the callers
// that already speak to this package.
const (
	WorkflowAgencySQL   = agencyid.WorkflowSQL
	DefinitionAgencySQL = agencyid.DefinitionSQL
)

// StepOutsideWorkflowRepository asks ScopeOutsideAgency for a run that is a
// workflow's step, at the moment it is dispatched, of the agency of the
// workflow the run's workflow run is of. The engine asks it when the step is
// queued. A scope can be given to another agency after that, and the job's own
// check (RepoScopeMismatch) does not see it.
//
// False for a run that is no workflow's step, and for a workflow that is no
// longer there. A Git workflow whose repository is gone has no agency to run
// for: true.
func StepOutsideWorkflowRepository(ctx context.Context, database *sql.DB, traceID, scope string) (bool, error) {
	var agency sql.NullString
	err := database.QueryRowContext(ctx, `
		SELECT `+WorkflowAgencySQL+`
		  FROM runs r
		  JOIN workflow_runs wr ON wr.id = r.workflow_run_id
		  JOIN workflows w ON w.uid = wr.workflow_uid
		 WHERE r.id = ?`, traceID).Scan(&agency)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !agency.Valid {
		return true, nil
	}
	return ScopeOutsideAgency(ctx, database, agency.String, scope)
}

// PinOutsideRepository asks ScopeOutsideRepository of the repository a
// checkout run is PINNED to (runs.checkout_repo), at the moment it is
// dispatched. RepoScopeMismatch asks about the job as it is now; a checkout
// run can wait a long time for its agent, and the job can be joined to another
// script meanwhile, while the pin still names the repository the agent is
// about to be sent to.
//
// False for a run that is pinned to no repository, and for a repository that
// is no longer connected: the manifest has its own answer for that one
// (no_checkout_repo), and there is then nowhere to send the agent. A pin to
// Global's repository is asked too since §0a (question 31): Global's checkout
// runs on Global's scopes only.
func PinOutsideRepository(ctx context.Context, database *sql.DB, repoID, scope string) (bool, error) {
	if repoID == "" {
		return false, nil
	}
	var there int
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM git_repos WHERE id = ?`, repoID).Scan(&there); err != nil {
		return false, err
	}
	if there == 0 {
		return false, nil
	}
	return ScopeOutsideRepository(ctx, database, repoID, scope)
}
