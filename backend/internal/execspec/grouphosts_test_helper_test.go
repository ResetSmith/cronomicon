package execspec

import (
	"context"
	"database/sql"
	"fmt"
)

// GroupHosts returns the member host names of one group in a scope's projection.
// Test-only: kept here (not in the production build) because only the group/limit
// targeting tests read a single group's membership directly; ResolveRun is the
// production path.
func GroupHosts(ctx context.Context, db *sql.DB, scope, group string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sgh.host
		FROM scope_group_hosts sgh
		JOIN scopes s ON s.id = sgh.scope_id
		WHERE s.name = ? AND sgh.group_name = ?
		ORDER BY sgh.host`, scope, group)
	if err != nil {
		return nil, fmt.Errorf("read group hosts: %w", err)
	}
	defer rows.Close()
	var hosts []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	return hosts, rows.Err()
}
