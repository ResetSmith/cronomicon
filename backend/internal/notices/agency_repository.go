package notices

import (
	"context"
	"database/sql"
	"sync/atomic"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindAgencyNoRepository — an agency has no repository connected (2.4.0
// Phase I3; §0a, IN-2; the owner's answer to question 39). Every agency has
// one, Global included: it is where the agency's definitions live, and where
// its publishes, and from 2.5.0 the assignments it is given, are committed.
// An agency created since I3 is created with its repository and cannot lose
// it; this is an agency from before, or Global's repository with no URL.
// Nothing is refused while it is so: the agency works as it did, and cannot
// receive an assignment.
//
// Subject: the agency's id. Filed under the agency. It clears when the
// agency's repository is connected (for Global: a URL in its connection, or
// CRONOMICON_GITLAB_BASE_URL).
const KindAgencyNoRepository = "agency_no_repository"

// globalRepoFromEnvironment is whether CRONOMICON_GITLAB_BASE_URL names
// Global's repository: the checks are given the database only.
var globalRepoFromEnvironment atomic.Bool

// SetGlobalRepoFromEnvironment tells the checks whether Global's repository's
// URL comes from the environment (the server sets it at start).
func SetGlobalRepoFromEnvironment(set bool) { globalRepoFromEnvironment.Store(set) }

func checkAgencyNoRepository(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT a.id, a.name FROM agencies a
		 WHERE NOT EXISTS (SELECT 1 FROM git_repos g WHERE g.agency_id = a.id AND COALESCE(g.url, '') <> '')
		 ORDER BY a.name`)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if id == agencyid.Global {
			if globalRepoFromEnvironment.Load() {
				continue
			}
			found = append(found, Finding{
				AgencyID: id,
				Subject:  id,
				Detail: "Global's repository is not connected: its connection has no URL, and CRONOMICON_GITLAB_BASE_URL " +
					"is not set. Every agency has a repository, Global included: it is where Global's definitions live, " +
					"and where its publishes and the assignments it makes are committed. Nothing is refused while it is so. " +
					"Connect it (a URL, and a token that can push) under Settings → Integrations; this notice clears when it is done.",
			})
			continue
		}
		found = append(found, Finding{
			AgencyID: id,
			Subject:  id,
			Detail: "The agency " + name + " has no repository connected. Since 2.4.0 every agency has one: it is where its " +
				"definitions live, and where its publishes and the assignments it is given are committed. An agency is now " +
				"created with its repository; this one is from before. Until it is connected the agency works as it did, and " +
				"cannot receive an assignment. Connect it (a URL on the allowed hosts, and a token that can push) under " +
				"Settings → Integrations; this notice clears when it is done.",
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return Reconcile(ctx, database, KindAgencyNoRepository, found)
}
