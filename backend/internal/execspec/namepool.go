package execspec

import (
	"context"
	"database/sql"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// Per-agency name uniqueness (R2-5, the rbac2 plan).
//
// The invariant: within a source pool, a definition's name must be unique
// within its agency, and a definition of Global's must be unique against
// EVERYTHING — Global's name pool overlaps every agency's. That is a
// set-overlap rule, which no SQLite UNIQUE constraint can express, so it lives
// here and is called by every write path that can create a definition or
// change whose it is: compose create, compose edit, restore from the recycle
// bin.
//
// Whose a definition is comes from its owner (an in-app job's since 1400, a
// workflow's since 1360). Until I2 it was worked out from the scopes of the
// job or of a workflow's step jobs, which cannot say whose once a scope's name
// is unique within an agency only (§0a, IN-5). Global's is the pool a job with
// no scope, or a scope in Global or in several agencies, was in before, so the
// answers are the ones they were.
//
// Schedules are NOT checked here: they keep per-source, per-owner uniqueness in
// the schema (R2-Q1, 1300), and their conflict is the plain 409 the compose
// path has always issued.

// NamePoolConflict reports whether a definition named `name` owned by `owner`
// (an agency id) would collide with an EXISTING definition in the same table
// and source pool, excluding the row identified by excludeUID (pass "" on
// create).
//
// The table name is interpolated and MUST be one of the two callers' constants
// ("jobs" / "workflows") — never caller input.
func NamePoolConflict(ctx context.Context, db *sql.DB, table, source, name, owner, excludeUID string) (bool, error) {
	ownerOf := agencyid.JobSQL("d")
	if table == "workflows" {
		ownerOf = "d.owner_agency"
	}
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(d.uid, ''), COALESCE(`+ownerOf+`, '`+agencyid.Global+`') FROM `+table+` d
		  WHERE d.source = ? AND d.name = ?`, source, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if owner == "" {
		owner = agencyid.Global
	}
	for rows.Next() {
		var uid, theirs string
		if err := rows.Scan(&uid, &theirs); err != nil {
			return false, err
		}
		if uid == excludeUID {
			continue
		}
		if owner == agencyid.Global || theirs == agencyid.Global || owner == theirs {
			return true, nil
		}
	}
	return false, rows.Err()
}

// NamePoolRefusal is the one wording every refusing surface uses. GENERIC on
// purpose — the AF oracle-hygiene rule: it never names the owning agency and
// never points at the recycle bin for a conflict the caller cannot see, so a
// name-taken answer cannot be used to enumerate another department's catalog.
const NamePoolRefusal = "name already in use"

// WorkflowNamePoolConflict is NamePoolConflict for workflows.
func WorkflowNamePoolConflict(ctx context.Context, db *sql.DB, source, name, owner, excludeUID string) (bool, error) {
	return NamePoolConflict(ctx, db, "workflows", source, name, owner, excludeUID)
}
