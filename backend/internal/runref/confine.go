package runref

import (
	"context"
	"database/sql"

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
// A job of Global's repository, and a job built in the app on Global's script
// or on none, are not confined by it.

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
	rows, err := database.QueryContext(ctx, counts+`
		  FROM jobs j JOIN git_repos g ON g.id = j.repo_id
		 WHERE j.source = 'git' AND g.id <> ?3
		   AND ((?1 <> '' AND j.uid = ?1) OR (?5 = 1 AND j.name = ?4))
		UNION ALL`+counts+`
		  FROM jobs j
		  JOIN scripts s ON s.uid = j.script_uid
		  JOIN git_repos g ON g.id = s.repo_id
		 WHERE j.source <> 'git' AND g.id <> ?3
		   AND ((?1 <> '' AND j.uid = ?1) OR (?6 = 1 AND j.name = ?4 AND j.source = ?7))`,
		jobUID, scope, repoid.Global, jobName, byNameGit, byNameApp, jobSource)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var agencies, own int
		if err := rows.Scan(&agencies, &own); err != nil {
			return false, err
		}
		if scope == "" || agencies != 1 || own != 1 {
			return true, nil
		}
	}
	return false, rows.Err()
}

// ReasonWorkflowScopeMismatch is the stored reason of a workflow step refused
// by ScopeOutsideRepository.
const ReasonWorkflowScopeMismatch = "Refused: this workflow comes from an agency's repository, and this step would run on a scope that does not belong to that agency"

// ScopeOutsideRepository reports whether a scope is one a workflow of the
// repository repoID may NOT run a step on: the repository belongs to an
// agency, and the scope is not that agency's alone (another agency's,
// Global's, several agencies', none at all, or not there). Global's
// repository, and no repository, may run on any.
//
// RepoScopeMismatch asks this of a JOB's repository. A workflow needs it asked
// of its own: a step of an agency's repository's workflow may resolve to a job
// of GLOBAL's repository (GR-16), and a Global job may name any agency's scope
// (GR-14). Without this an agency's committers could run Global's job on
// another agency's hosts, on their schedule and with their environment.
func ScopeOutsideRepository(ctx context.Context, database *sql.DB, repoID, scope string) (bool, error) {
	if repoID == "" || repoID == repoid.Global {
		return false, nil
	}
	var agencies, own int
	err := database.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?1),
		       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		         WHERE sc.name = ?1 AND sa.agency_id = g.agency_id)
		  FROM git_repos g WHERE g.id = ?2`, scope, repoID).Scan(&agencies, &own)
	if err == sql.ErrNoRows {
		// A repository that is not there has no agency to run for.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return scope == "" || agencies != 1 || own != 1, nil
}

// StepOutsideWorkflowRepository asks ScopeOutsideRepository for a run that is
// a workflow's step, at the moment it is dispatched: the workflow the run's
// workflow run is of comes from an agency's repository, and scope is not that
// agency's alone. The engine asks it when the step is queued. A scope can be
// given to another agency after that, and the job's own check
// (RepoScopeMismatch) does not see it when the step's job is Global's.
//
// False for a run that is no workflow's step, for a workflow built in the app
// or of Global's repository, and for one that is no longer there: there is
// then no repository to confine it to, as when the engine asked.
func StepOutsideWorkflowRepository(ctx context.Context, database *sql.DB, traceID, scope string) (bool, error) {
	var home string
	err := database.QueryRowContext(ctx, `
		SELECT COALESCE(w.repo_id, ?)
		  FROM runs r
		  JOIN workflow_runs wr ON wr.id = r.workflow_run_id
		  JOIN workflows w ON w.uid = wr.workflow_uid
		 WHERE r.id = ? AND w.source = 'git'`, repoid.Global, traceID).Scan(&home)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ScopeOutsideRepository(ctx, database, home, scope)
}
