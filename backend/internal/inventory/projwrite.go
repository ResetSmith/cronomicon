package inventory

import (
	"context"
	"database/sql"
)

// WriteProjectionTables replace-the-sets a scope's advisory projection tables
// (scope_groups / scope_group_hosts / scope_group_children / scope_host_vars /
// scope_group_vars) from a parsed Projection, inside the caller's transaction. It
// is the SINGLE writer shared by git sync (gitlab.writeScopeProjection) and the
// in-app inventory upload handler (settings, M5) so the two can't drift.
//
// On a degraded projection it clears the tables and writes NO tree (the
// all-or-nothing rule — never persist a half-parsed tree). The caller owns
// scopes.projection_status / projection_json.
func WriteProjectionTables(ctx context.Context, tx *sql.Tx, scopeID string, p Projection) error {
	// Delete in FK order (leaves before scope_groups).
	for _, stmt := range []string{
		`DELETE FROM scope_group_children WHERE scope_id=?`,
		`DELETE FROM scope_group_hosts WHERE scope_id=?`,
		`DELETE FROM scope_group_vars WHERE scope_id=?`,
		`DELETE FROM scope_host_vars WHERE scope_id=?`,
		`DELETE FROM scope_groups WHERE scope_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, scopeID); err != nil {
			return err
		}
	}
	if p.PreviewUnavailable {
		return nil // never persist a half-parsed tree
	}
	for gname, g := range p.Groups {
		// Insert the group row first so the (scope_id, group_name) FKs resolve.
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO scope_groups(scope_id, name) VALUES(?,?)`, scopeID, gname); err != nil {
			return err
		}
		for _, h := range g.Hosts {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO scope_group_hosts(scope_id, group_name, host) VALUES(?,?,?)`, scopeID, gname, h); err != nil {
				return err
			}
		}
		for _, ch := range g.Children {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO scope_group_children(scope_id, parent, child) VALUES(?,?,?)`, scopeID, gname, ch); err != nil {
				return err
			}
		}
		for k, v := range g.Vars {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO scope_group_vars(scope_id, group_name, key, value) VALUES(?,?,?,?)`, scopeID, gname, k, v); err != nil {
				return err
			}
		}
	}
	for h, vars := range p.HostVars {
		for k, v := range vars {
			if _, err := tx.ExecContext(ctx,
				`INSERT OR IGNORE INTO scope_host_vars(scope_id, host, key, value) VALUES(?,?,?,?)`, scopeID, h, k, v); err != nil {
				return err
			}
		}
	}
	return nil
}
