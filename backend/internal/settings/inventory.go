package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/inventory"
)

// Sentinel errors the API handlers map to HTTP status codes (M5).
var (
	// ErrInventoryGitReadOnly — in-app inventory ops are cronomicon-only (409).
	ErrInventoryGitReadOnly = errors.New("only cronomicon-source scopes can be authored in-app; git scopes are managed in GitLab")
	// ErrInventoryUnsupportedFormat — only INI inventories are parsed in v1 (422).
	ErrInventoryUnsupportedFormat = errors.New("only 'ini' inventories are supported")
	// ErrNoInventory — import-hosts needs a stored inventory (409).
	ErrNoInventory = errors.New("scope has no inventory to import hosts from")
	// ErrProjectionDegraded — can't import from an unparsed inventory (409).
	ErrProjectionDegraded = errors.New("inventory projection is unavailable; fix the inventory before importing hosts")
)

// InventoryInput is the PUT /scopes/{id}/inventory body.
type InventoryInput struct {
	Raw    string
	Format string
}

// InventoryImportResult is the import-hosts outcome.
type InventoryImportResult struct {
	Created  int      `json:"created"`
	Updated  int      `json:"updated"`
	Skipped  []string `json:"skipped"`
	Rejected []string `json:"rejected"`
}

// InventoryDocument is the GET /scopes/{id}/inventory response: the scope's
// inventory and its ADVISORY parsed projection (M2). The projection is never
// authoritative for execution — ansible reads the raw file via `-i`; this drives
// the read-only UI (and, M3, group targeting).
type InventoryDocument struct {
	Source       string               `json:"source"`
	Format       *string              `json:"format,omitempty"`
	Editable     bool                 `json:"editable"` // cronomicon-source (in-app authoring lands in M5)
	HasInventory bool                 `json:"hasInventory"`
	Raw          *string              `json:"raw,omitempty"`         // cronomicon-source only
	ParseStatus  string               `json:"parseStatus"`           // ok | unavailable (projection is all-or-nothing)
	ParseReason  *string              `json:"parseReason,omitempty"` // why the preview is unavailable
	ParseLine    *int                 `json:"parseLine,omitempty"`   // 1-based line of the first out-of-subset construct
	Projection   *InventoryProjection `json:"projection,omitempty"`  // nil when unavailable / no inventory
}

// InventoryProjection is the advisory parsed tree. Advisory is always true — a
// visible, structural reminder that it is non-authoritative.
type InventoryProjection struct {
	Advisory bool             `json:"advisory"`
	Hosts    []InventoryHost  `json:"hosts"`
	Groups   []InventoryGroup `json:"groups"`
}

// InventoryGroup is one parsed group.
type InventoryGroup struct {
	Name     string            `json:"name"`
	Hosts    []string          `json:"hosts,omitempty"`
	Children []string          `json:"children,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
}

// InventoryHost is one parsed host with its host_vars.
type InventoryHost struct {
	Name string            `json:"name"`
	Vars map[string]string `json:"vars,omitempty"`
}

// GetScopeInventory loads a scope's inventory + advisory projection. Returns
// (nil, nil) when no scope matches the id. Each table is queried sequentially and
// fully drained before the next (SQLite pool rule — no nested cursors).
func GetScopeInventory(ctx context.Context, database *sql.DB, id string) (*InventoryDocument, error) {
	var source string
	var raw, format, projStatus, projJSON sql.NullString
	err := database.QueryRowContext(ctx,
		`SELECT source, raw_inventory, inventory_format, projection_status, projection_json
		 FROM scopes WHERE id=?`, id).
		Scan(&source, &raw, &format, &projStatus, &projJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	doc := &InventoryDocument{
		Source:       source,
		Editable:     source == "cronomicon",
		HasInventory: raw.Valid && raw.String != "",
		ParseStatus:  "ok",
	}
	if format.Valid && format.String != "" {
		doc.Format = &format.String
	}
	if projStatus.Valid && projStatus.String != "" {
		doc.ParseStatus = projStatus.String
	}
	// Raw is exposed for cronomicon-source scopes only (git content lives in GitLab;
	// the Scope carries its blob URL).
	if source == "cronomicon" && raw.Valid && raw.String != "" {
		doc.Raw = &raw.String
	}
	// Degrade reason/line from projection_json.
	if projJSON.Valid && projJSON.String != "" {
		var meta struct {
			Reason string `json:"reason"`
			Line   int    `json:"line"`
		}
		if json.Unmarshal([]byte(projJSON.String), &meta) == nil {
			if meta.Reason != "" {
				doc.ParseReason = &meta.Reason
			}
			if meta.Line > 0 {
				doc.ParseLine = &meta.Line
			}
		}
	}

	// No usable projection: serve no tree (the raw still ships to ansible). Guard
	// on != "ok" (not just "unavailable") so any future non-ok status can never
	// surface a partial/lying tree — the projection is strictly all-or-nothing.
	if !doc.HasInventory || doc.ParseStatus != "ok" {
		return doc, nil
	}

	proj, err := loadProjection(ctx, database, id)
	if err != nil {
		return nil, err
	}
	doc.Projection = proj
	return doc, nil
}

// loadProjection reads the normalized projection tables into an InventoryProjection.
func loadProjection(ctx context.Context, database *sql.DB, scopeID string) (*InventoryProjection, error) {
	groupHosts := map[string][]string{}
	groupChildren := map[string][]string{}
	groupVars := map[string]map[string]string{}
	hostVars := map[string]map[string]string{}
	groupNames := map[string]bool{}

	// scope_groups (group identity).
	if err := eachRow(ctx, database,
		`SELECT name FROM scope_groups WHERE scope_id=? ORDER BY name`, scopeID,
		func(rows *sql.Rows) error {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			groupNames[n] = true
			return nil
		}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, database,
		`SELECT group_name, host FROM scope_group_hosts WHERE scope_id=? ORDER BY group_name, host`, scopeID,
		func(rows *sql.Rows) error {
			var g, h string
			if err := rows.Scan(&g, &h); err != nil {
				return err
			}
			groupHosts[g] = append(groupHosts[g], h)
			return nil
		}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, database,
		`SELECT parent, child FROM scope_group_children WHERE scope_id=? ORDER BY parent, child`, scopeID,
		func(rows *sql.Rows) error {
			var p, ch string
			if err := rows.Scan(&p, &ch); err != nil {
				return err
			}
			groupChildren[p] = append(groupChildren[p], ch)
			return nil
		}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, database,
		`SELECT group_name, key, value FROM scope_group_vars WHERE scope_id=? ORDER BY group_name, key`, scopeID,
		func(rows *sql.Rows) error {
			var g, k string
			var v sql.NullString
			if err := rows.Scan(&g, &k, &v); err != nil {
				return err
			}
			if groupVars[g] == nil {
				groupVars[g] = map[string]string{}
			}
			groupVars[g][k] = v.String
			return nil
		}); err != nil {
		return nil, err
	}
	if err := eachRow(ctx, database,
		`SELECT host, key, value FROM scope_host_vars WHERE scope_id=? ORDER BY host, key`, scopeID,
		func(rows *sql.Rows) error {
			var h, k string
			var v sql.NullString
			if err := rows.Scan(&h, &k, &v); err != nil {
				return err
			}
			if hostVars[h] == nil {
				hostVars[h] = map[string]string{}
			}
			hostVars[h][k] = v.String
			return nil
		}); err != nil {
		return nil, err
	}

	// Hosts: the scope_hosts membership, with any parsed host_vars attached.
	hosts, _ := getScopeHosts(ctx, database, scopeID)
	proj := &InventoryProjection{Advisory: true, Hosts: []InventoryHost{}, Groups: []InventoryGroup{}}
	for _, h := range hosts {
		proj.Hosts = append(proj.Hosts, InventoryHost{Name: h, Vars: hostVars[h]})
	}

	names := make([]string, 0, len(groupNames))
	for n := range groupNames {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		proj.Groups = append(proj.Groups, InventoryGroup{
			Name:     n,
			Hosts:    groupHosts[n],
			Children: groupChildren[n],
			Vars:     groupVars[n],
		})
	}
	return proj, nil
}

// PutScopeInventory writes an operator-authored inventory to an cronomicon-source
// scope (M5): validates secrets (Path A), parses the projection, and persists raw
// + projection + scope_hosts membership + inferred capability, then audits. Reuses
// the SAME inventory.ValidateSecrets / ParseProjection / WriteProjectionTables /
// InferTypes the git-sync path uses, so authored and synced inventories behave
// identically. Returns:
//   - (nil, nil, nil)          when no scope matches the id (handler → 404)
//   - (nil, lineErrors, nil)   when secret-bearing vars are present (handler → 422;
//     the scope is left unchanged)
//   - (nil, nil, ErrInventoryGitReadOnly / ErrInventoryUnsupportedFormat)
//   - (doc, nil, nil)          on success
func PutScopeInventory(ctx context.Context, database *sql.DB, id, raw, format, actor string) (*InventoryDocument, []LineError, error) {
	var source, name string
	if err := database.QueryRowContext(ctx, `SELECT source, name FROM scopes WHERE id=?`, id).Scan(&source, &name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if source != "cronomicon" {
		return nil, nil, ErrInventoryGitReadOnly
	}
	if format == "" {
		format = "ini"
	}
	if format != "ini" {
		return nil, nil, ErrInventoryUnsupportedFormat
	}

	// Path A — reject secret-bearing inventory; leave the scope unchanged.
	if secErrs := inventory.ValidateSecrets(raw, "inventory"); len(secErrs) > 0 {
		out := make([]LineError, 0, len(secErrs))
		for _, se := range secErrs {
			out = append(out, LineError{File: se.File, Line: se.Line, Message: se.Message})
		}
		return nil, out, nil
	}

	p := inventory.ParseProjection(raw)
	status := "ok"
	var projJSON sql.NullString
	if p.PreviewUnavailable {
		status = "unavailable"
		if b, e := json.Marshal(map[string]any{"reason": p.PreviewReason, "line": p.PreviewLine}); e == nil {
			projJSON = sql.NullString{String: string(b), Valid: true}
		}
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Capability: union the operator's existing supported_types with the inferred
	// types (so uploading an ansible inventory makes the scope ansible-capable
	// without dropping the operator's choices), bash floor enforced (OD-19).
	var curTypes string
	_ = tx.QueryRowContext(ctx, `SELECT supported_types FROM scopes WHERE id=?`, id).Scan(&curTypes)
	typeSet := map[string]bool{}
	var existing []string
	if curTypes != "" {
		_ = json.Unmarshal([]byte(curTypes), &existing)
	}
	for _, t := range existing {
		typeSet[t] = true
	}
	for _, t := range inventory.InferTypes(raw) {
		typeSet[t] = true
	}
	merged := make([]string, 0, len(typeSet))
	for t := range typeSet {
		merged = append(merged, t)
	}
	sort.Strings(merged)
	typesJSON, _ := json.Marshal(enforceBashFloor(merged))

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `
		UPDATE scopes SET raw_inventory=?, inventory_format=?, projection_status=?, projection_json=?,
		                  supported_types=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
		raw, format, status, projJSON, string(typesJSON), actor, now, id); err != nil {
		return nil, nil, err
	}
	if err := inventory.WriteProjectionTables(ctx, tx, id, p); err != nil {
		return nil, nil, err
	}
	// Replace scope_hosts membership. Use the CLEAN ConnHosts set (no
	// [group:vars]/[group:children] phantoms) when parsed cleanly; but on a DEGRADE
	// ConnHosts is only PARTIAL (parsing stops at the offending line), so fall back
	// to the naive, always-complete p.Hosts (degrade-independent, matching git) so a
	// degraded save never silently truncates the scope's targetable membership.
	membership := p.ConnHosts
	if p.PreviewUnavailable {
		membership = p.Hosts
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM scope_hosts WHERE scope_id=?`, id); err != nil {
		return nil, nil, err
	}
	for _, h := range membership {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO scope_hosts(scope_id, host) VALUES(?,?)`, id, h); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	audit(ctx, database, actor, "Scopes", "inventory updated", name, "")
	doc, err := GetScopeInventory(ctx, database, id)
	return doc, nil, err
}

// ImportScopeHosts materializes an cronomicon scope's parsed inventory hosts into
// ssh_hosts (source='cronomicon', scope_id) so the in-app SSH executor can dial them
// (M5, the cap-C completion deferred from M4 — git scopes auto-import via sync).
// Keyed by (scope_id, hostname): an existing row is updated when overwrite is set,
// else skipped. Returns (nil, nil) when no scope matches.
func ImportScopeHosts(ctx context.Context, database *sql.DB, id string, hosts []string, overwrite bool, actor string) (*InventoryImportResult, error) {
	var source, name string
	var raw, projStatus sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT source, name, raw_inventory, projection_status FROM scopes WHERE id=?`, id).
		Scan(&source, &name, &raw, &projStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if source != "cronomicon" {
		return nil, ErrInventoryGitReadOnly
	}
	if !raw.Valid || raw.String == "" {
		return nil, ErrNoInventory
	}
	if projStatus.Valid && projStatus.String != "" && projStatus.String != "ok" {
		return nil, ErrProjectionDegraded
	}

	want := map[string]bool{}
	for _, h := range hosts {
		want[h] = true
	}
	conns := inventory.HostConns(inventory.ParseProjection(raw.String), "")
	res := &InventoryImportResult{Skipped: []string{}, Rejected: []string{}}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	now := time.Now().UTC().Format(time.RFC3339)
	nullOf := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	matched := map[string]bool{}
	for _, hc := range conns {
		if len(hosts) > 0 && !want[hc.Host] {
			continue
		}
		matched[hc.Host] = true
		port := hc.Port
		if port == 0 {
			port = 22
		}
		var existingID string
		var existingAddr sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT id, address FROM ssh_hosts WHERE hostname=? AND source='cronomicon' AND scope_id=?`, hc.Host, id).
			Scan(&existingID, &existingAddr)
		switch {
		case err == nil && existingID != "":
			if !overwrite {
				res.Skipped = append(res.Skipped, hc.Host)
				continue
			}
			// If the dial address moved, the stored TOFU host_key + verification
			// status belong to a DIFFERENT machine — clear them so the next dial
			// re-TOFUs against the new endpoint rather than trusting a stale key.
			if existingAddr.String != hc.Address {
				if _, err := tx.ExecContext(ctx,
					`UPDATE ssh_hosts SET host_key=NULL, status='unverified' WHERE id=?`, existingID); err != nil {
					return nil, err
				}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE ssh_hosts SET address=?, port=?, username=?, auth_key_env_var=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
				nullOf(hc.Address), port, nullOf(hc.User), nullOf(hc.AuthKeyEnvVar), actor, now, existingID); err != nil {
				return nil, err
			}
			res.Updated++
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO ssh_hosts(id, source, scope_id, hostname, address, port, username, auth_key_env_var,
				                      status, created_by, created_at, last_modified_by, last_modified_at)
				VALUES(?, 'cronomicon', ?, ?, ?, ?, ?, ?, 'unverified', ?, ?, ?, ?)`,
				db.NewID(), id, hc.Host, nullOf(hc.Address), port, nullOf(hc.User), nullOf(hc.AuthKeyEnvVar), actor, now, actor, now); err != nil {
				return nil, err
			}
			res.Created++
		default:
			return nil, err
		}
	}
	// A host the caller explicitly requested that is not in the inventory gets
	// reported back rather than silently dropped.
	for _, w := range hosts {
		if !matched[w] {
			res.Rejected = append(res.Rejected, w)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	audit(ctx, database, actor, "SSH Hosts", "imported from inventory", name,
		fmt.Sprintf("%d created, %d updated, %d skipped", res.Created, res.Updated, len(res.Skipped)))
	return res, nil
}

// eachRow runs a query and calls fn for each row, closing the cursor before
// returning so the next query gets a free connection (SQLite pool rule).
func eachRow(ctx context.Context, database *sql.DB, query, arg string, fn func(*sql.Rows) error) error {
	rows, err := database.QueryContext(ctx, query, arg)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
