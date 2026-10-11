package workflow

import (
	"context"
	"database/sql"
	"log/slog"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// The scheduler cannot start a workflow itself (it must not import this
// package, which imports it), so it is given two callbacks: one for a schedule
// entry that fires, one for a parked run whose time has come. They are built
// here and not in the server's main so that a test can drive them.

// ScheduledFirer is the scheduler's callback for a workflow's schedule entry:
// it starts a run of the entry's workflow, tagged scheduled, with the entry's
// name and its environment snapshot.
func ScheduledFirer(database *sql.DB, log *slog.Logger) scheduler.WorkflowFirer {
	return func(ctx context.Context, f scheduler.WorkflowFire) {
		// The entry's own workflow, by its uid: a source and a name may be
		// another agency's workflow too (present defect 34).
		rowid, steps, err := LoadForFire(ctx, database, f.UID, f.Source, f.Name)
		if err != nil {
			log.Error("scheduler firer: look up workflow", "workflow", f.Name, "source", f.Source, "error", err)
			return
		}
		if _, err := New(database, log).Trigger(ctx, TriggerParams{
			WorkflowName:   f.Name,
			WorkflowSource: f.Source,
			WorkflowID:     rowid,
			Steps:          steps,
			TriggeredBy:    "scheduler",
			TriggerKind:    "scheduled",
			ScheduleName:   f.ScheduleName,
			EnvJSON:        f.EnvJSON,
		}); err != nil {
			log.Error("scheduler firer: trigger workflow", "workflow", f.Name, "error", err)
		}
	}
}

// PendingFirer is the scheduler's callback for a parked WORKFLOW run that the
// pending loop promotes (AR, a deferred ad-hoc run; RX, a reaction). It goes
// through the same engine seam as the cron firer, but keeps the scheduling
// operator as triggered_by so the run's audit trail names a person.
func PendingFirer(database *sql.DB, log *slog.Logger) scheduler.PendingWorkflowFirer {
	return func(ctx context.Context, p scheduler.PendingWorkflowFire) {
		// The parked workflow, by the uid the promotion resolved.
		rowid, steps, err := LoadForFire(ctx, database, p.UID, p.Source, p.Name)
		if err != nil {
			log.Error("pending firer: look up workflow", "workflow", p.Name, "source", p.Source, "error", err)
			return
		}
		// RX-11 — the trigger kind and the reaction provenance ride the envelope
		// rather than being hardcoded here. A reaction-fired workflow that
		// reported "manual" would be a lie in History, and a depth that reset to
		// zero on every hop would disarm the runaway ceiling for whole chains.
		if _, err := New(database, log).Trigger(ctx, TriggerParams{
			WorkflowName:   p.Name,
			WorkflowSource: p.Source,
			WorkflowID:     rowid,
			Steps:          steps,
			TriggeredBy:    p.TriggeredBy,
			TriggerKind:    p.TriggerKind,
			EnvJSON:        p.EnvJSON,
			ReactionDepth:  p.ReactionDepth,
			ReactedToRunID: p.ReactedToRunID,
		}); err != nil {
			log.Error("pending firer: trigger workflow", "workflow", p.Name, "error", err)
		}
	}
}
