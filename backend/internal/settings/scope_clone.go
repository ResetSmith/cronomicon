package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// Cloning a scope to an agency (2.4.0 Phase R6; the owner's decision of
// 2026-10-13).
//
// Giving a scope to another agency is rare and deliberate, and it strands the
// giving agency's jobs on the day. What a handover lacked is a way for the
// receiving agency to have the HOSTS without that: a clone.
//
// A clone is a NEW scope, built in the app, owned by the target agency,
// whatever the original's source (Git owns a Git scope, and goes on owning
// it). It is a snapshot and does not follow the original afterwards. It takes
// a new name: a scope's name is unique across the installation (GR-19).
//
// Copied: the hosts, the groups and their nesting, the host and group
// variables, the inventory as written, and the tags.
//
// NOT copied:
//   - runner bindings: the target agency has its own agents;
//   - approved host keys: each runner approves its own;
//   - jobs: they are the giving agency's;
//   - the keys, and the bastion, of the host records. Each host's record
//     (address, port, user, operating system) is copied into a record of the
//     clone, which is the target agency's, with the key and the bastion left
//     unset: the giving agency's SSH keys and bastions are not the receiving
//     agency's to use. The clone connects to nothing until its agency sets its
//     own.

// ScopeCloneCounts is what a clone copied.
type ScopeCloneCounts struct {
	Hosts       int `json:"hosts"`
	Groups      int `json:"groups"`
	HostVars    int `json:"hostVars"`
	GroupVars   int `json:"groupVars"`
	HostRecords int `json:"hostRecords"`
}

// ErrScopeNameTaken is CloneScope's answer for a name a scope already has.
var ErrScopeNameTaken = errors.New("scope name already exists")

// ErrNoSuchScope is CloneScope's answer for a scope that is not there.
var ErrNoSuchScope = errors.New("no such scope")

// CloneScope copies the scope srcID into a new in-app scope called name, owned
// by the agency targetAgency, and returns it with what was copied. The caller
// has decided who may (who may move the scope: requireMove).
func CloneScope(ctx context.Context, database *sql.DB, srcID, name, targetAgency, actor string) (*Scope, ScopeCloneCounts, error) {
	// LR-78: a new scope changes what an access grant reaches.
	defer auth.GrantsChanged()
	var counts ScopeCloneCounts
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, counts, fmt.Errorf("%w: the clone needs a name of its own", ErrValidation)
	}
	var one int
	if err := database.QueryRowContext(ctx, `SELECT 1 FROM agencies WHERE id = ?`, targetAgency).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, counts, ErrUnknownAgency
		}
		return nil, counts, err
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, counts, err
	}
	defer tx.Rollback() //nolint:errcheck

	var description, rawInv, invFmt, projStatus, projJSON, tags sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT description, raw_inventory, inventory_format, projection_status, projection_json, tags FROM scopes WHERE id = ?`, srcID).
		Scan(&description, &rawInv, &invFmt, &projStatus, &projJSON, &tags)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, counts, ErrNoSuchScope
	}
	if err != nil {
		return nil, counts, fmt.Errorf("clone scope: read the scope: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	id := db.NewID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO scopes (id, name, source, description, created_by, created_at, last_modified_by, last_modified_at,
		                     raw_inventory, inventory_format, projection_status, projection_json, tags, owner_agency)
		 VALUES (?, ?, 'cronomicon', ?, ?, ?, ?, ?, ?, ?, COALESCE(?, 'ok'), ?, COALESCE(?, '[]'), ?)`,
		id, name, description, actor, now, actor, now, rawInv, invFmt, projStatus, projJSON, tags, targetAgency); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, counts, ErrScopeNameTaken
		}
		return nil, counts, fmt.Errorf("clone scope: %w", err)
	}
	// The clone is the target agency's, and no other's: it is born there, by
	// the trigger that gives every scope its owner's membership (1420; until
	// then it was born in Global and moved). Taking it out of Global and back in
	// would, for a moment, make it Global's, and a name Global holds would
	// refuse it.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, id, targetAgency); err != nil {
		return nil, counts, fmt.Errorf("clone scope: place it in its agency: %w", err)
	}

	for _, c := range []struct {
		into  *int
		query string
	}{
		{&counts.Hosts, `INSERT INTO scope_hosts (scope_id, host) SELECT ?1, host FROM scope_hosts WHERE scope_id = ?2`},
		{&counts.Groups, `INSERT INTO scope_groups (scope_id, name) SELECT ?1, name FROM scope_groups WHERE scope_id = ?2`},
		{nil, `INSERT INTO scope_group_hosts (scope_id, group_name, host) SELECT ?1, group_name, host FROM scope_group_hosts WHERE scope_id = ?2`},
		{nil, `INSERT INTO scope_group_children (scope_id, parent, child) SELECT ?1, parent, child FROM scope_group_children WHERE scope_id = ?2`},
		{&counts.HostVars, `INSERT INTO scope_host_vars (scope_id, host, key, value) SELECT ?1, host, key, value FROM scope_host_vars WHERE scope_id = ?2`},
		{&counts.GroupVars, `INSERT INTO scope_group_vars (scope_id, group_name, key, value) SELECT ?1, group_name, key, value FROM scope_group_vars WHERE scope_id = ?2`},
	} {
		res, err := tx.ExecContext(ctx, c.query, id, srcID)
		if err != nil {
			return nil, counts, fmt.Errorf("clone scope: copy its contents: %w", err)
		}
		if c.into != nil {
			n, _ := res.RowsAffected()
			*c.into = int(n)
		}
	}

	// The host records: where each host is, and as whom. Not with what key,
	// and not through whose bastion.
	rows, err := tx.QueryContext(ctx,
		`SELECT hostname, port, username, address, os FROM ssh_hosts WHERE scope_id = ? ORDER BY hostname`, srcID)
	if err != nil {
		return nil, counts, fmt.Errorf("clone scope: read its host records: %w", err)
	}
	type record struct {
		hostname              string
		port                  sql.NullInt64
		username, address, os sql.NullString
	}
	var records []record
	for rows.Next() {
		var r record
		if err := rows.Scan(&r.hostname, &r.port, &r.username, &r.address, &r.os); err != nil {
			rows.Close()
			return nil, counts, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, counts, err
	}
	rows.Close()
	for _, r := range records {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ssh_hosts (id, hostname, port, username, address, os, source, scope_id, owner_agency,
			                        created_by, created_at, last_modified_by, last_modified_at)
			 VALUES (?, ?, COALESCE(?, 22), ?, ?, ?, 'cronomicon', ?, ?, ?, ?, ?, ?)`,
			db.NewID(), r.hostname, r.port, r.username, r.address, r.os, id, targetAgency, actor, now, actor, now); err != nil {
			return nil, counts, fmt.Errorf("clone scope: copy the record of %s: %w", r.hostname, err)
		}
		counts.HostRecords++
	}

	if err := tx.Commit(); err != nil {
		return nil, counts, err
	}
	// The copy's agency's row, naming where it came from (D1).
	audit(ctx, database, ownerOrGlobalAgency(targetAgency), actor, "Scopes", "created", name,
		"cloned from the scope "+srcID+" of "+RowAgency(ctx, database, "scopes", "owner_agency", srcID))
	sc, err := GetScope(ctx, database, id)
	return sc, counts, err
}
