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
// A job of Global's repository, and a job built in the app, are not confined
// by it: this is a rule about what a repository may reach.

// ReasonRepoScopeMismatch is what is stored (runs.queued_reason, a reaction's
// delivery, a file sighting's refusal, a pending run's miss) and shown. A
// SENTENCE, by the convention of ReasonKeyBindingNeedsAgent: History shows the
// stored reason verbatim, and a standing refusal is recognised by its text.
const ReasonRepoScopeMismatch = "Refused: this job comes from an agency's repository, and the scope it would run on does not belong to that agency"

// CodeRepoScopeMismatch is the API error code (422) the manual and token
// triggers answer with for the same refusal.
const CodeRepoScopeMismatch = "repo_scope_mismatch"

// RepoScopeMismatch reports whether a run of a job on a scope is to be refused
// because the job comes from an agency's repository and the scope is not that
// agency's: another agency's, Global's, several agencies', none at all, or one
// that is not there. scope is the scope the RUN would have, which for a manual
// run may not be the job's own.
//
// The job is found by its uid. A caller that has none (a run parked before a
// run recorded one) names it by source and name, and then every Git job of
// that name from an agency's repository is asked: one that does not fit
// refuses the run. That is stricter than it need be, on a path that is not
// taken by anything written since 1010.
func RepoScopeMismatch(ctx context.Context, database *sql.DB, jobUID, jobSource, jobName, scope string) (bool, error) {
	if jobUID == "" && jobSource != "" && jobSource != "git" {
		return false, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id WHERE sc.name = ?2),
		       (SELECT COUNT(*) FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		         WHERE sc.name = ?2 AND sa.agency_id = g.agency_id)
		  FROM jobs j JOIN git_repos g ON g.id = j.repo_id
		 WHERE j.source = 'git' AND g.id <> ?3
		   AND ((?1 <> '' AND j.uid = ?1) OR (?1 = '' AND j.name = ?4))`,
		jobUID, scope, repoid.Global, jobName)
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
