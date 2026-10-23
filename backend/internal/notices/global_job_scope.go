package notices

import (
	"context"
	"database/sql"
)

// KindGlobalJobOnAgencyScope — a job of Global's repository names a scope
// that is not Global's (2.4.0, §0a, IN-3, open point 1). Until §0a a job of
// Global's repository might name any agency's scope (I-7). Now it runs on
// Global's scopes only: such a job does not run (each run is refused, with
// the reason), and Global's sync keeps it as it was rather than removing it,
// so that what it was is there to copy.
//
// Subject: the job's uid. Filed under Global. It clears when the job's file
// names a scope of Global's, or none, or leaves the repository.
const KindGlobalJobOnAgencyScope = "global_job_on_agency_scope"

// Retired in 2.5.3 (CA-18): a job of Global's repository is a leftover of a repository that no longer syncs, listed with the rest by central_holds_work. Open notices clear at the next check.
func checkGlobalJobOnAgencyScope(ctx context.Context, database *sql.DB) error {
	return Reconcile(ctx, database, KindGlobalJobOnAgencyScope, nil)
}
