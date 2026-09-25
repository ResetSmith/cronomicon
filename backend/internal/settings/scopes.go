package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/inventory"
)

// Scope is the wire representation of a scope (both local and git-source).
type Scope struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Scope       string          `json:"scope"`
	Name        *string         `json:"name,omitempty"`
	Description *string         `json:"description"`
	Hosts       []string        `json:"hosts,omitempty"`
	HostCount   int             `json:"hostCount"`
	GitLabURL   *string         `json:"gitlabUrl,omitempty"`
	SidecarPath *string         `json:"sidecarPath,omitempty"`
	Capability  ScopeCapability `json:"capability"`
	// M2 inventory projection summary (populated for inventory scopes of either
	// source). The full group/host-var tree is served by GET /scopes/{id}/inventory.
	HasInventory     bool         `json:"hasInventory"`
	InventoryFormat  *string      `json:"inventoryFormat,omitempty"`
	ProjectionStatus *string      `json:"projectionStatus,omitempty"` // ok | unavailable (projection is all-or-nothing)
	Groups           []ScopeGroup `json:"groups,omitempty"`           // M3 targetable groups (NAMES only — for the run dialog selector)
	LastChangedAt    *string      `json:"lastChangedAt"`
	CreatedBy        string       `json:"createdBy,omitempty"`
	CreatedAt        string       `json:"createdAt,omitempty"`
	LastModifiedBy   string       `json:"lastModifiedBy,omitempty"`
	LastModifiedAt   string       `json:"lastModifiedAt,omitempty"`
	// Agency is the network-isolation zone this scope's hosts live in (agency-support.md
	// M1). Operator-owned overlay set via PUT /scopes/{id}/agency; survives re-sync.
	// T3.8 — the FULL agency set (migration 670), not a single agency: a scope may
	// belong to several. The scalar `agency` field it replaces was backed by
	// scopes.agency_id, dropped in migration 700.
	Agencies []AgencyRef `json:"agencies"`
}

// AgencyRef is the lightweight {id,name} of a scope's bound agency.
type AgencyRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ScopeGroup is a targetable inventory group (name + member count) — the
// non-sensitive summary the run dialog's group selector needs (M3). The full
// group tree with vars is served only by the ConfigureApp-gated inventory endpoint.
type ScopeGroup struct {
	Name      string `json:"name"`
	HostCount int    `json:"hostCount"`
}

// ScopeCapability represents resolved run-type capability.
type ScopeCapability struct {
	Types  []string    `json:"types"`
	Origin string      `json:"origin"`
	Owner  *string     `json:"owner,omitempty"`
	Errors []LineError `json:"errors,omitempty"`
}

// LineError represents a 1-based validation error from strict parsing (S10).
type LineError struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// InventoryValidationError is returned when secret-bearing variables are present
// in the raw inventory supplied at creation/edit time.
type InventoryValidationError struct {
	Errors []LineError
}

func (e InventoryValidationError) Error() string {
	return fmt.Sprintf("inventory validation failed with %d error(s)", len(e.Errors))
}

// LocalScopeInput is the caller-supplied payload for create/update.
type LocalScopeInput struct {
	Scope          string
	Description    *string
	Hosts          []string
	SupportedTypes []string // bash floor enforced
	RawInventory   *string
}

// BrokenReference describes a dangling scope reference after a rename.
type BrokenReference struct {
	Entity string `json:"entity"`
	Name   string `json:"name"`
}

// ListScopes returns scopes. If sourceFilter is provided, it filters by source ('git' or 'cronomicon').
func ListScopes(ctx context.Context, database *sql.DB, sourceFilter string) ([]Scope, error) {
	// Fetch GitLab config first to resolve GitLabURL for git-source scopes.
	var repoURL, writeBranch string
	row := database.QueryRowContext(ctx, `SELECT repo_url, write_branch FROM gitlab_config WHERE id=1`)
	_ = row.Scan(&repoURL, &writeBranch)
	if writeBranch == "" {
		writeBranch = "main"
	}
	repoURL = strings.TrimSuffix(repoURL, ".git")

	var query string
	var args []any
	if sourceFilter != "" {
		query = `SELECT id, name, source, description, supported_types, created_by, created_at,
		                last_modified_by, last_modified_at, source_path, capability_types, capability_json, synced_at, inventory_format, projection_status, CAST((raw_inventory IS NOT NULL AND raw_inventory != '') AS TEXT) AS has_inventory
		         FROM scopes WHERE source=? ORDER BY name`
		args = append(args, sourceFilter)
	} else {
		query = `SELECT id, name, source, description, supported_types, created_by, created_at,
		                last_modified_by, last_modified_at, source_path, capability_types, capability_json, synced_at, inventory_format, projection_status, CAST((raw_inventory IS NOT NULL AND raw_inventory != '') AS TEXT) AS has_inventory
		         FROM scopes ORDER BY source DESC, name`
	}

	rows, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list scopes: %w", err)
	}
	defer rows.Close()

	var out []Scope
	for rows.Next() {
		var sc Scope
		var desc, createdBy, lastModBy, lastModAt, sourcePath, capTypes, capJSON, syncedAt, invFmt, projStatus, hasInvStr sql.NullString
		var typesJSON string
		if err := rows.Scan(
			&sc.ID, &sc.Scope, &sc.Source, &desc, &typesJSON,
			&createdBy, &sc.CreatedAt, &lastModBy, &lastModAt,
			&sourcePath, &capTypes, &capJSON, &syncedAt, &invFmt, &projStatus, &hasInvStr,
		); err != nil {
			return nil, err
		}

		if desc.Valid {
			sc.Description = &desc.String
		}
		if createdBy.Valid {
			sc.CreatedBy = createdBy.String
		}
		if lastModBy.Valid {
			sc.LastModifiedBy = lastModBy.String
		}
		if lastModAt.Valid {
			sc.LastModifiedAt = lastModAt.String
		}
		applyInvSummary(&sc, invFmt, projStatus, hasInvStr)

		if sc.Source == "git" {
			if capJSON.Valid && capJSON.String != "" {
				var capData struct {
					Types       []string    `json:"types"`
					Origin      string      `json:"origin"`
					Owner       *string     `json:"owner"`
					SidecarPath *string     `json:"sidecarPath"`
					Errors      []LineError `json:"errors"`
				}
				if err := json.Unmarshal([]byte(capJSON.String), &capData); err == nil {
					sc.Capability.Types = capData.Types
					sc.Capability.Origin = capData.Origin
					sc.Capability.Owner = capData.Owner
					sc.Capability.Errors = capData.Errors
					sc.SidecarPath = capData.SidecarPath
				}
			}
			if sc.Capability.Types == nil {
				sc.Capability.Types = []string{"bash"}
			}
			if sourcePath.Valid && sourcePath.String != "" {
				filename := filepath.Base(sourcePath.String)
				sc.Name = &filename
				if repoURL != "" {
					urlVal := fmt.Sprintf("%s/-/blob/%s/%s", repoURL, writeBranch, sourcePath.String)
					sc.GitLabURL = &urlVal
				}
			}
			if syncedAt.Valid && syncedAt.String != "" {
				t := syncedAt.String
				sc.LastChangedAt = &t
			} else {
				t := sc.CreatedAt
				sc.LastChangedAt = &t
			}
		} else {
			// cronomicon source
			if err := json.Unmarshal([]byte(typesJSON), &sc.Capability.Types); err != nil {
				sc.Capability.Types = []string{"bash"}
			}
			sc.Capability.Origin = "local"
			t := sc.LastModifiedAt
			sc.LastChangedAt = &t
		}

		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Now fetch hosts for each scope (separate queries, connection released).
	for i := range out {
		hosts, _ := getScopeHosts(ctx, database, out[i].ID)
		out[i].Hosts = hosts
		out[i].HostCount = len(hosts)
		out[i].Groups, _ = getScopeGroups(ctx, database, out[i].ID)
		out[i].Agencies = resolveAgencyRefs(ctx, database, out[i].ID)
	}
	return out, nil
}

// GetScope fetches a single scope by ID (handles both git and cronomicon sources).
func GetScope(ctx context.Context, database *sql.DB, id string) (*Scope, error) {
	// Fetch GitLab config to resolve GitLabURL.
	var repoURL, writeBranch string
	rowCfg := database.QueryRowContext(ctx, `SELECT repo_url, write_branch FROM gitlab_config WHERE id=1`)
	_ = rowCfg.Scan(&repoURL, &writeBranch)
	if writeBranch == "" {
		writeBranch = "main"
	}
	repoURL = strings.TrimSuffix(repoURL, ".git")

	row := database.QueryRowContext(ctx,
		`SELECT id, name, source, description, supported_types, created_by, created_at,
		        last_modified_by, last_modified_at, source_path, capability_types, capability_json, synced_at, inventory_format, projection_status, CAST((raw_inventory IS NOT NULL AND raw_inventory != '') AS TEXT) AS has_inventory
		 FROM scopes WHERE id=?`, id)

	var sc Scope
	var desc, createdBy, lastModBy, lastModAt, sourcePath, capTypes, capJSON, syncedAt, invFmt, projStatus, hasInvStr sql.NullString
	var typesJSON string
	if err := row.Scan(
		&sc.ID, &sc.Scope, &sc.Source, &desc, &typesJSON,
		&createdBy, &sc.CreatedAt, &lastModBy, &lastModAt,
		&sourcePath, &capTypes, &capJSON, &syncedAt, &invFmt, &projStatus, &hasInvStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	if desc.Valid {
		sc.Description = &desc.String
	}
	if createdBy.Valid {
		sc.CreatedBy = createdBy.String
	}
	if lastModBy.Valid {
		sc.LastModifiedBy = lastModBy.String
	}
	if lastModAt.Valid {
		sc.LastModifiedAt = lastModAt.String
	}
	applyInvSummary(&sc, invFmt, projStatus, hasInvStr)

	if sc.Source == "git" {
		if capJSON.Valid && capJSON.String != "" {
			var capData struct {
				Types       []string    `json:"types"`
				Origin      string      `json:"origin"`
				Owner       *string     `json:"owner"`
				SidecarPath *string     `json:"sidecarPath"`
				Errors      []LineError `json:"errors"`
			}
			if err := json.Unmarshal([]byte(capJSON.String), &capData); err == nil {
				sc.Capability.Types = capData.Types
				sc.Capability.Origin = capData.Origin
				sc.Capability.Owner = capData.Owner
				sc.Capability.Errors = capData.Errors
				sc.SidecarPath = capData.SidecarPath
			}
		}
		if sc.Capability.Types == nil {
			sc.Capability.Types = []string{"bash"}
		}
		if sourcePath.Valid && sourcePath.String != "" {
			filename := filepath.Base(sourcePath.String)
			sc.Name = &filename
			if repoURL != "" {
				urlVal := fmt.Sprintf("%s/-/blob/%s/%s", repoURL, writeBranch, sourcePath.String)
				sc.GitLabURL = &urlVal
			}
		}
		if syncedAt.Valid && syncedAt.String != "" {
			t := syncedAt.String
			sc.LastChangedAt = &t
		} else {
			t := sc.CreatedAt
			sc.LastChangedAt = &t
		}
	} else {
		// cronomicon source
		if err := json.Unmarshal([]byte(typesJSON), &sc.Capability.Types); err != nil {
			sc.Capability.Types = []string{"bash"}
		}
		sc.Capability.Origin = "local"
		t := sc.LastModifiedAt
		sc.LastChangedAt = &t
	}

	hosts, _ := getScopeHosts(ctx, database, sc.ID)
	sc.Hosts = hosts
	sc.HostCount = len(hosts)
	sc.Groups, _ = getScopeGroups(ctx, database, sc.ID)
	sc.Agencies = resolveAgencyRefs(ctx, database, sc.ID)
	return &sc, nil
}

// resolveAgencyRefs resolves a scope's FULL agency membership (migration 670) to
// {id,name} pairs, sorted by name. Best-effort: returns an empty (non-nil) slice
// for an unbound scope or a pre-670 schema, so a scope read never fails on the
// agency lookup — and so the API renders [] rather than null.
func resolveAgencyRefs(ctx context.Context, database *sql.DB, scopeID string) []AgencyRef {
	out := []AgencyRef{}
	rows, err := database.QueryContext(ctx, `
		SELECT a.id, a.name FROM scope_agencies sa
		JOIN agencies a ON a.id = sa.agency_id
		WHERE sa.scope_id = ?
		ORDER BY a.name`, scopeID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var ref AgencyRef
		if err := rows.Scan(&ref.ID, &ref.Name); err != nil {
			return out
		}
		out = append(out, ref)
	}
	return out
}

// CreateScope creates a new local scope.
func CreateScope(ctx context.Context, database *sql.DB, inp LocalScopeInput, actor string) (*Scope, error) {
	if inp.RawInventory != nil && *inp.RawInventory != "" {
		if secErrs := inventory.ValidateSecrets(*inp.RawInventory, "inventory"); len(secErrs) > 0 {
			out := make([]LineError, 0, len(secErrs))
			for _, se := range secErrs {
				out = append(out, LineError{File: se.File, Line: se.Line, Message: se.Message})
			}
			return nil, InventoryValidationError{Errors: out}
		}
	}

	var rawInv, invFmt, projJSON sql.NullString
	projStatus := "ok"
	var p *inventory.Projection
	if inp.RawInventory != nil && *inp.RawInventory != "" {
		raw := *inp.RawInventory
		proj := inventory.ParseProjection(raw)
		p = &proj
		rawInv = sql.NullString{String: raw, Valid: true}
		invFmt = sql.NullString{String: "ini", Valid: true}
		if proj.PreviewUnavailable {
			projStatus = "unavailable"
			if b, e := json.Marshal(map[string]any{"reason": proj.PreviewReason, "line": proj.PreviewLine}); e == nil {
				projJSON = sql.NullString{String: string(b), Valid: true}
			}
		}

		for _, t := range inventory.InferTypes(raw) {
			inp.SupportedTypes = append(inp.SupportedTypes, t)
		}
	}

	types := enforceBashFloor(inp.SupportedTypes)
	typeSet := map[string]bool{}
	for _, t := range types {
		typeSet[t] = true
	}
	merged := make([]string, 0, len(typeSet))
	for t := range typeSet {
		merged = append(merged, t)
	}
	sort.Strings(merged)
	types = enforceBashFloor(merged)
	typesJSON, _ := json.Marshal(types)
	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.ExecContext(ctx,
		`INSERT INTO scopes (id, name, source, description, supported_types, created_by, created_at, last_modified_by, last_modified_at, raw_inventory, inventory_format, projection_status, projection_json)
		 VALUES (?, ?, 'cronomicon', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, inp.Scope, inp.Description, string(typesJSON), actor, now, actor, now, rawInv, invFmt, projStatus, projJSON)
	if err != nil {
		return nil, fmt.Errorf("create scope: %w", err)
	}

	if p != nil {
		if err := inventory.WriteProjectionTables(ctx, tx, id, *p); err != nil {
			return nil, err
		}
		membership := p.ConnHosts
		if p.PreviewUnavailable {
			membership = p.Hosts
		}
		if err := insertScopeHosts(ctx, tx, id, membership); err != nil {
			return nil, err
		}
	} else {
		if err := insertScopeHosts(ctx, tx, id, inp.Hosts); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	audit(ctx, database, actor, "Scopes", "created", inp.Scope, "")
	return GetScope(ctx, database, id)
}

// UpdateScope updates a local scope. On rename it returns broken references (S9).
func UpdateScope(ctx context.Context, database *sql.DB, id string, inp LocalScopeInput, actor string) (*Scope, []BrokenReference, error) {
	existing, err := GetScope(ctx, database, id)
	if err != nil || existing == nil {
		return nil, nil, err
	}
	if existing.Source != "cronomicon" {
		return nil, nil, fmt.Errorf("only cronomicon-source scopes are editable")
	}

	if inp.RawInventory != nil && *inp.RawInventory != "" {
		if secErrs := inventory.ValidateSecrets(*inp.RawInventory, "inventory"); len(secErrs) > 0 {
			out := make([]LineError, 0, len(secErrs))
			for _, se := range secErrs {
				out = append(out, LineError{File: se.File, Line: se.Line, Message: se.Message})
			}
			return nil, nil, InventoryValidationError{Errors: out}
		}
	}

	var rawInv, invFmt, projJSON sql.NullString
	projStatus := "ok"
	var p *inventory.Projection
	var updateInventoryCols bool

	if inp.RawInventory != nil {
		updateInventoryCols = true
		if *inp.RawInventory != "" {
			raw := *inp.RawInventory
			proj := inventory.ParseProjection(raw)
			p = &proj
			rawInv = sql.NullString{String: raw, Valid: true}
			invFmt = sql.NullString{String: "ini", Valid: true}
			if proj.PreviewUnavailable {
				projStatus = "unavailable"
				if b, e := json.Marshal(map[string]any{"reason": proj.PreviewReason, "line": proj.PreviewLine}); e == nil {
					projJSON = sql.NullString{String: string(b), Valid: true}
				}
			}

			for _, t := range inventory.InferTypes(raw) {
				inp.SupportedTypes = append(inp.SupportedTypes, t)
			}
		}
	}

	types := enforceBashFloor(inp.SupportedTypes)
	typeSet := map[string]bool{}
	for _, t := range types {
		typeSet[t] = true
	}
	merged := make([]string, 0, len(typeSet))
	for t := range typeSet {
		merged = append(merged, t)
	}
	sort.Strings(merged)
	types = enforceBashFloor(merged)
	typesJSON, _ := json.Marshal(types)
	now := time.Now().UTC().Format(time.RFC3339)
	oldName := existing.Scope
	newName := inp.Scope

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if updateInventoryCols {
		_, err = tx.ExecContext(ctx,
			`UPDATE scopes SET name=?, description=?, supported_types=?, last_modified_by=?, last_modified_at=?,
			                  raw_inventory=?, inventory_format=?, projection_status=?, projection_json=? WHERE id=?`,
			newName, inp.Description, string(typesJSON), actor, now, rawInv, invFmt, projStatus, projJSON, id)
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE scopes SET name=?, description=?, supported_types=?, last_modified_by=?, last_modified_at=? WHERE id=?`,
			newName, inp.Description, string(typesJSON), actor, now, id)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("update scope: %w", err)
	}

	// Update scope_hosts & projection tables.
	if updateInventoryCols {
		if p != nil {
			if err := inventory.WriteProjectionTables(ctx, tx, id, *p); err != nil {
				return nil, nil, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM scope_hosts WHERE scope_id=?`, id); err != nil {
				return nil, nil, err
			}
			membership := p.ConnHosts
			if p.PreviewUnavailable {
				membership = p.Hosts
			}
			if err := insertScopeHosts(ctx, tx, id, membership); err != nil {
				return nil, nil, err
			}
		} else {
			// Cleared inventory — reset projection tables and write flat hosts.
			if err := inventory.WriteProjectionTables(ctx, tx, id, inventory.Projection{PreviewUnavailable: true}); err != nil {
				return nil, nil, err
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM scope_hosts WHERE scope_id=?`, id); err != nil {
				return nil, nil, err
			}
			if err := insertScopeHosts(ctx, tx, id, inp.Hosts); err != nil {
				return nil, nil, err
			}
		}
	} else {
		// Non-inventory update: only replace hosts if scope does NOT have an inventory.
		if !existing.HasInventory {
			if _, err = tx.ExecContext(ctx, `DELETE FROM scope_hosts WHERE scope_id=?`, id); err != nil {
				return nil, nil, err
			}
			if err := insertScopeHosts(ctx, tx, id, inp.Hosts); err != nil {
				return nil, nil, err
			}
		}
	}

	// The scope_restrictions cascade that used to live here went with the table
	// (RB-19, v0.57.8). Grants are authored against an AGENCY, not a scope name, so
	// a scope rename cannot orphan one — `scope_agencies` keys on scope_id, which a
	// rename does not touch. The rename is now cascade-free by construction rather
	// than by remembering to write the UPDATE.
	var broken []BrokenReference
	if oldName != newName {
		// Lint: find broken references in env_vars and secrets (scope column).
		// Use the tx (not the pool) to avoid a deadlock on the single-connection pool.
		broken = append(broken, brokenEnvVarRefsTx(ctx, tx, oldName)...)
		broken = append(broken, brokenSecretRefsTx(ctx, tx, oldName)...)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	audit(ctx, database, actor, "Scopes", "updated", newName, "")
	sc, err := GetScope(ctx, database, id)
	return sc, broken, err
}

// DeleteScope removes an cronomicon-source scope. Returns 409 if jobs reference it.
func DeleteScope(ctx context.Context, database *sql.DB, id, actor string) (bool, error) {
	existing, err := GetScope(ctx, database, id)
	if err != nil || existing == nil {
		return false, err
	}
	if existing.Source != "cronomicon" {
		return false, fmt.Errorf("only cronomicon-source scopes can be deleted")
	}
	// Check job references (jobs table owned by B3/B5). FX-A4: a binned job is not
	// a live reference — it cannot run, and blocking the scope delete on one made
	// the operator empty the recycle bin to proceed.
	var refCount int
	_ = database.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE scope=? AND deleted_at IS NULL`, existing.Scope).Scan(&refCount)
	if refCount > 0 {
		return false, fmt.Errorf("scope is still referenced by %d job(s)", refCount)
	}

	res, err := database.ExecContext(ctx, `DELETE FROM scopes WHERE id=?`, id)
	if err != nil {
		return false, fmt.Errorf("delete scope: %w", err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		audit(ctx, database, actor, "Scopes", "deleted", existing.Scope, "")
	}
	return n > 0, nil
}

// applyInvSummary fills the M2 inventory-projection summary fields from the
// scanned columns (shared by ListScopes and GetScope). has_inventory arrives as
// the text '0'/'1' of a CAST boolean so it folds into the NullString scan group.
func applyInvSummary(sc *Scope, invFmt, projStatus, hasInvStr sql.NullString) {
	sc.HasInventory = hasInvStr.String == "1"
	if invFmt.Valid && invFmt.String != "" {
		sc.InventoryFormat = &invFmt.String
	}
	if projStatus.Valid && projStatus.String != "" {
		sc.ProjectionStatus = &projStatus.String
	}
}

// getScopeGroups fetches the targetable groups (name + member count) of a scope's
// projection. Best-effort: callers ignore the error so a pre-360 schema (isolated
// tests) yields no groups rather than failing the scope read.
func getScopeGroups(ctx context.Context, database *sql.DB, scopeID string) ([]ScopeGroup, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT sg.name, COUNT(sgh.host)
		FROM scope_groups sg
		LEFT JOIN scope_group_hosts sgh ON sgh.scope_id = sg.scope_id AND sgh.group_name = sg.name
		WHERE sg.scope_id = ?
		GROUP BY sg.name
		ORDER BY sg.name`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScopeGroup
	for rows.Next() {
		var g ScopeGroup
		if err := rows.Scan(&g.Name, &g.HostCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// getScopeHosts fetches the host list for a scope.
func getScopeHosts(ctx context.Context, database *sql.DB, scopeID string) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT host FROM scope_hosts WHERE scope_id=? ORDER BY host`, scopeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// insertScopeHosts bulk-inserts scope hosts.
func insertScopeHosts(ctx context.Context, ex dbExecer, scopeID string, hosts []string) error {
	for _, h := range hosts {
		if _, err := ex.ExecContext(ctx,
			`INSERT OR IGNORE INTO scope_hosts (scope_id, host) VALUES (?, ?)`, scopeID, h); err != nil {
			return fmt.Errorf("insert scope host %q: %w", h, err)
		}
	}
	return nil
}

// enforceBashFloor ensures "bash" is always present in supportedTypes (S10).
func enforceBashFloor(types []string) []string {
	if len(types) == 0 {
		return []string{"bash"}
	}
	for _, t := range types {
		if strings.EqualFold(t, "bash") {
			return types
		}
	}
	return append([]string{"bash"}, types...)
}

// dbQuerier is a minimal interface satisfied by *sql.DB and *sql.Tx.
type dbQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// brokenEnvVarRefs finds env_vars that still reference oldScope.
func brokenEnvVarRefs(ctx context.Context, q dbQuerier, oldScope string) []BrokenReference {
	rows, _ := q.QueryContext(ctx, `SELECT key FROM env_vars WHERE scope=?`, oldScope)
	if rows == nil {
		return nil
	}
	defer rows.Close()
	var out []BrokenReference
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out = append(out, BrokenReference{Entity: "envVars", Name: k})
	}
	return out
}

// brokenSecretRefs finds secrets that still reference oldScope.
func brokenSecretRefs(ctx context.Context, q dbQuerier, oldScope string) []BrokenReference {
	rows, _ := q.QueryContext(ctx, `SELECT key FROM secrets WHERE scope=?`, oldScope)
	if rows == nil {
		return nil
	}
	defer rows.Close()
	var out []BrokenReference
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out = append(out, BrokenReference{Entity: "secrets", Name: k})
	}
	return out
}

// brokenEnvVarRefsTx is like brokenEnvVarRefs but uses a transaction.
func brokenEnvVarRefsTx(ctx context.Context, tx *sql.Tx, oldScope string) []BrokenReference {
	return brokenEnvVarRefs(ctx, tx, oldScope)
}

// brokenSecretRefsTx is like brokenSecretRefs but uses a transaction.
func brokenSecretRefsTx(ctx context.Context, tx *sql.Tx, oldScope string) []BrokenReference {
	return brokenSecretRefs(ctx, tx, oldScope)
}
