// Package settings implements all operator-config CRUD routes for B6:
// env vars, scopes, alerts/destinations, SSH hosts/bastions, notification
// config, global settings KV, and audit export.
package settings

import (
	"context"
	"database/sql"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// RowAgency is the agency of the row id of table, read from its owner column
// (owner_agency or agency_id) by the no-owner rule: the agency an audit row
// about that object is of (2.5.0 C1). Global's when the row is not there.
func RowAgency(ctx context.Context, db *sql.DB, table, column, id string) string {
	var a string
	if err := db.QueryRowContext(ctx,
		`SELECT `+agencyid.ColumnOwnerSQL("", column)+` FROM `+table+` WHERE id = ?`, id).Scan(&a); err != nil {
		return agencyid.Global
	}
	return a
}

// WriteChangeLog inserts a row into the change_log table (S6) via the single
// shared auditlog writer (CC.13).
// category examples: "Env Vars", "Secrets", "Scopes", "Settings", "Alerts",
//
//	"SSH Hosts", "Bastions", "Alert Destinations".
//
// agency is the agency of the object changed (2.5.0 C1, question 74);
// agencyid.Global for an installation-wide change.
func WriteChangeLog(ctx context.Context, db *sql.DB, agency, actor, category, action, target, details string) error {
	return auditlog.WriteChangeLog(ctx, db, agency, actor, category, action, target, details)
}

// writeChangeLog is the unexported alias (used inside the package).
func writeChangeLog(ctx context.Context, db *sql.DB, agency, actor, category, action, target, details string) error {
	return WriteChangeLog(ctx, db, agency, actor, category, action, target, details)
}

// writeActivity inserts a kind='config' activity row (S6) for user-visible
// mutations, via the single shared auditlog writer (CC.13). summary mirrors the
// old "action target" concatenation.
func writeActivity(ctx context.Context, db *sql.DB, agency, actor, category, action, target, details string) error {
	return auditlog.WriteActivity(ctx, db, auditlog.ActivityParams{
		AgencyID: agency,
		Kind:     "config",
		Outcome:  "success",
		Actor:    actor,
		Category: category,
		Summary:  action + " " + target,
		Details:  details,
	})
}

// audit writes both a change_log row and an activity row for a config mutation,
// both of agency: the object's (2.5.0 C1, question 74), agencyid.Global for an
// installation-wide change.
func audit(ctx context.Context, db *sql.DB, agency, actor, category, action, target, details string) {
	_ = writeChangeLog(ctx, db, agency, actor, category, action, target, details)
	_ = writeActivity(ctx, db, agency, actor, category, action, target, details)
}

// Audit is the exported form of audit, for callers in other packages that must
// match a config entity's value-write feed behavior (change_log + activity) —
// e.g. the env-var tags handler, whose value path (UpdateEnvVar) emits both.
func Audit(ctx context.Context, db *sql.DB, agency, actor, category, action, target, details string) {
	audit(ctx, db, agency, actor, category, action, target, details)
}
