package scheduler

import (
	"context"
	"database/sql"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// A definition's own calendars (2.5.3, CA-30 to CA-33): a job and a workflow
// may name calendars to Forbid and to Allow, which gate every AUTOMATIC start
// of it (originPolicy.definitionCalendars: the schedule, a service account's
// trigger, a reaction, a file arrival, the promotion of a queue- or
// reaction-parked row), and not a person's Run nor the promotion of a run a
// person deferred. A start they stop is dropped, never postponed, and
// recorded. A workflow's step is not gated by its job's calendars (CA-33):
// the engine starts steps without asking the gates.

// DefinitionBindings are the Forbid and Allow calendars of a definition, by
// its uid where the caller has it, else by source and name. None when it
// cannot be read: a calendar that cannot be read must not stop a start (the
// fail-open stance of calendarVerdict).
func DefinitionBindings(ctx context.Context, database *sql.DB, kind, source, name, uid string) calendar.Bindings {
	b, _ := definitionBindings(ctx, database, kind, source, name, uid)
	return b
}

// definitionBindings is DefinitionBindings and the uid of the row it read: a
// definition found by its name is of its own agency, which its uid tells
// (definitionAgency reads Global's for none, the review of CA4b).
func definitionBindings(ctx context.Context, database *sql.DB, kind, source, name, uid string) (calendar.Bindings, string) {
	if kind == "" {
		kind = "job"
	}
	b := calendar.Bindings{Kind: kind}
	table := "jobs"
	if kind == "workflow" {
		table = "workflows"
	}
	q, args := `SELECT forbid_calendars, allow_calendars, COALESCE(uid, '') FROM `+table+` WHERE source = ? AND name = ? ORDER BY deleted_at IS NOT NULL, rowid LIMIT 1`, []any{source, name}
	if uid != "" {
		q, args = `SELECT forbid_calendars, allow_calendars, COALESCE(uid, '') FROM `+table+` WHERE uid = ?`, []any{uid}
	}
	var forbid, allow sql.NullString
	var found string
	//nolint:gosec // table is one of two literals
	if err := database.QueryRowContext(ctx, q, args...).Scan(&forbid, &allow, &found); err != nil {
		return b, uid
	}
	b.DefForbid, b.DefAllow = calendar.ParseNames(forbid.String), calendar.ParseNames(allow.String)
	return b, found
}

// DefinitionVerdictAt is what the definition's own Forbid and Allow
// calendars say of a start at a moment, on its day in the application's zone
// (loc; nil reads the setting): what the Run dialog warns of for today, and
// what a service account's deferred trigger is judged by on the day it would
// start. Its own calendars alone: a freeze is judged at the start itself, and
// must not hide them (the review of CA4b: a deferral into a freeze window was
// parked, and its promotion, a hand deferral's, never asked them). The day is
// returned with it.
func DefinitionVerdictAt(ctx context.Context, database *sql.DB, kind, source, name, uid string, at time.Time, loc *time.Location) (calendar.Verdict, string, error) {
	day := calendar.DayOf(at, appLocationOrDB(ctx, database, loc))
	b, found := definitionBindings(ctx, database, kind, source, name, uid)
	if len(b.DefForbid) == 0 && len(b.DefAllow) == 0 {
		return calendar.Verdict{}, day, nil
	}
	sets, err := calendar.Load(ctx, database, definitionAgency(ctx, database, found, kind), b.Names())
	if err != nil {
		return calendar.Verdict{}, day, err
	}
	return calendar.EvaluateDefinition(sets, b, day), day, nil
}

// RecordRefusedStart records a start of a job its calendars refused, as a
// skipped run (2.5.3, CA-32: every dropped start leaves a record, an Allow
// miss included). For a producer outside this package: a service account's
// trigger, which is answered 409 and would otherwise leave nothing behind.
// Each refusal is a row: an outside system's every call is told of.
// It reports whether the row was written, for the caller's notification
// (a "skipped" rule hears it, as it hears a schedule's suppression).
func RecordRefusedStart(ctx context.Context, database *sql.DB, p EnqueueParams, triggerKind, triggeredBy string, g GateOutcome) (bool, error) {
	return recordSkippedFire(ctx, database, p, skipRecord{
		Reason: g.Reason, Mode: dedupeDay, Calendar: g.Calendar,
		TriggerKind: triggerKind, TriggeredBy: triggeredBy,
	})
}

// RecordRefusedWorkflowStart is RecordRefusedStart for a workflow.
func RecordRefusedWorkflowStart(ctx context.Context, database *sql.DB, uid, source, name, triggerKind, triggeredBy string, g GateOutcome) (bool, error) {
	return recordSkippedWorkflowFire(ctx, database, uid, source, name, "", skipRecord{
		Reason: g.Reason, Mode: dedupeDay, Calendar: g.Calendar,
		TriggerKind: triggerKind, TriggeredBy: triggeredBy,
	})
}
