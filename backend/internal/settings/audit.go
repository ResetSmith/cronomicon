// Package settings implements all operator-config CRUD routes for B6:
// env vars, scopes, alerts/destinations, SSH hosts/bastions, notification
// config, global settings KV, and audit export.
package settings

import (
	"context"
	"database/sql"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// WriteChangeLog inserts a row into the change_log table (S6) via the single
// shared auditlog writer (CC.13).
// category examples: "Env Vars", "Secrets", "Scopes", "Settings", "Alerts",
//
//	"SSH Hosts", "Bastions", "Alert Destinations".
func WriteChangeLog(ctx context.Context, db *sql.DB, actor, category, action, target, details string) error {
	return auditlog.WriteChangeLog(ctx, db, actor, category, action, target, details)
}

// writeChangeLog is the unexported alias (used inside the package).
func writeChangeLog(ctx context.Context, db *sql.DB, actor, category, action, target, details string) error {
	return WriteChangeLog(ctx, db, actor, category, action, target, details)
}

// writeActivity inserts a kind='config' activity row (S6) for user-visible
// mutations, via the single shared auditlog writer (CC.13). summary mirrors the
// old "action target" concatenation.
func writeActivity(ctx context.Context, db *sql.DB, actor, category, action, target, details string) error {
	return auditlog.WriteActivity(ctx, db, auditlog.ActivityParams{
		Kind:     "config",
		Outcome:  "success",
		Actor:    actor,
		Category: category,
		Summary:  action + " " + target,
		Details:  details,
	})
}

// audit writes both a change_log row and an activity row for a config mutation.
func audit(ctx context.Context, db *sql.DB, actor, category, action, target, details string) {
	_ = writeChangeLog(ctx, db, actor, category, action, target, details)
	_ = writeActivity(ctx, db, actor, category, action, target, details)
}

// Audit is the exported form of audit, for callers in other packages that must
// match a config entity's value-write feed behavior (change_log + activity) —
// e.g. the env-var tags handler, whose value path (UpdateEnvVar) emits both.
func Audit(ctx context.Context, db *sql.DB, actor, category, action, target, details string) {
	audit(ctx, db, actor, category, action, target, details)
}
