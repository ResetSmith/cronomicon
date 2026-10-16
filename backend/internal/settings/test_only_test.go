package settings

import (
	"context"
	"database/sql"
)

// Test-only since 2.4.2: nothing in the server calls these any more (the review of 2.4.x).

// DeleteAgency removes an agency. It returns *AgencyInUseError (wrapping
// ErrAgencyInUse, mapped 409) when the agency is still referenced — by a scope (M1),
// a runner (M2), a membership row (T2.8), or an OWNED entity (RA-15/RA-19).
// Historical runs.agency name snapshots never block deletion (immutable facts).
// A connected repository blocks it too; DeleteAgencyWith takes it.
func DeleteAgency(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	return DeleteAgencyWith(ctx, database, id, actor, nil)
}
