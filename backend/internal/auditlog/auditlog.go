// Package auditlog is the single writer for the change_log and activity audit
// tables (CC.13). It merges the previously-duplicated settings.WriteChangeLog /
// writeActivity and workflow.InsertChangeLog / EmitActivity so there is exactly
// one INSERT site per table.
//
// Optional change_log columns (target, details) are stored as empty strings —
// CC-D2's canonical form. The settings writer already did this; the workflow
// writer used to store NULLs, so migration 500 normalizes the older NULL rows.
//
// Optional activity columns keep NULL-when-empty semantics: the activity.outcome
// CHECK constraint forbids ” and the numeric columns have no empty form, so a
// uniform empty-string rule is not possible there.
//
// It is a leaf package (takes an Execer, imports nothing from internal but the
// leaf agencyid) so the
// settings, workflow and api packages can all depend on it without a cycle.
package auditlog

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// Execer is the subset of *sql.DB / *sql.Tx these writers need.
//
// Taking the interface is what lets the demo seeder — which writes its audit
// rows inside the same transaction as the data they describe — use the shared
// writer instead of hand-rolled SQL. Without it, "single writer" would have been
// true of every path except the one that populates a fresh install.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ActivityParams holds the fields for inserting an activity row (the superset of
// both former writers). Zero-valued optional fields are stored as NULL.
type ActivityParams struct {
	// At is the event time (RFC3339 UTC). Empty means "now".
	//
	// It exists because most callers deliberately share one timestamp between an
	// activity row and the runs row it describes — the reaper's
	// `UPDATE runs SET completed_at = ?`, the claim's `started_at`, the SSH
	// probe's CheckedAt. Stamping time.Now() inside this writer instead would put
	// sub-second-to-multi-second skew between the pair and could invert their
	// order in the History timeline. The seeder additionally backdates its rows
	// to produce a realistic spread.
	At string

	Kind    string // run-start | run-end | workflow-start | workflow-end | config | gitsync | push
	Outcome string // success | failure | warning | "" (null)
	Actor   string
	// RunnerName is the runner's DISPLAY name for events a runner agent
	// performed or that happened to one (AA-1). A snapshot, not a lookup:
	// `Actor` carries `runner:<id>` and that id stops resolving the moment
	// the runner is deregistered, so the name is copied in at write time.
	// Empty for every non-runner event, which is most of them.
	RunnerName   string
	JobName      string
	JobID        int64
	WorkflowName string
	WorkflowID   int64
	// JobUID / WorkflowUID are the definition's permanent identity (R2-1).
	// Optional — the INSERT falls back, in order, to the run this event
	// describes (TraceID; runs and workflow_runs carry the uid since R2-1,
	// which covers every run-start/run-end emitter without touching them) and
	// then to the name, but only when that name is unique across both source
	// pools. activity carries no source column, so guessing would attribute
	// one pool's history to the other; NULL is the honest value for an
	// ambiguous or vanished name.
	JobUID       string
	WorkflowUID  string
	TraceID      string
	Scope        string
	Category     string
	Action       string
	Target       string
	Details      string
	Summary      string
	DurationMs   int64
	KilledBy     string
	CommitSha    string
	Repository   string
	Branch       string
	ScheduleFile string
	// AgencyID is the agency the event is of: its OBJECT's, never the agency
	// the request was viewing (2.5.0 C1; the owner's answer to question 74).
	// Empty: the agency of the run, the workflow run, the job or the workflow
	// the row names (TraceID, JobUID, WorkflowUID, as the INSERT resolves
	// them), else none, which reads as Global's. A row with none of those keys
	// names its agency here; writer_conformance_test holds every literal to it.
	// An installation-wide change is Global's (agencyid.Global), read by those
	// whose grant reaches every agency (the owner's answer to question 76).
	AgencyID string
}

// actingKey carries the agency a global administrator works in (2.4.4).
type actingKey struct{}

// WithActingAgency marks ctx as a global administrator's request made while
// working in agencyID (the agency switcher, 2.4.4 S3; the 2.5.0 plan's IN-9):
// each change_log row written under it records both, "actor, acting in
// agencyID". auth marks it when it attaches the identity; this package stays a
// leaf.
func WithActingAgency(ctx context.Context, agencyID string) context.Context {
	return context.WithValue(ctx, actingKey{}, agencyID)
}

// ActingAgency is the agency WithActingAgency marked ctx with, or "".
func ActingAgency(ctx context.Context) string {
	a, _ := ctx.Value(actingKey{}).(string)
	return a
}

// WriteChangeLog inserts one change_log row (S6). Optional columns (target,
// details) are stored verbatim — empty string, never NULL (CC-D2).
//
// agency is the agency of the object changed (2.5.0 C1, CF-3; question 74):
// never the agency the request was viewing. An installation-wide change is
// Global's. Empty stores none, which reads as Global's and is a writer's
// defect: every caller states one.
func WriteChangeLog(ctx context.Context, db Execer, agency, actor, category, action, target, details string) error {
	return WriteChangeLogAt(ctx, db, "", agency, actor, category, action, target, details)
}

// WriteChangeLogAt is WriteChangeLog with an explicit event time (RFC3339 UTC);
// empty means now. Only the seeder needs it, to backdate demo history.
func WriteChangeLogAt(ctx context.Context, db Execer, at, agency, actor, category, action, target, details string) error {
	now := stampOr(at)
	// Mask and bound BEFORE the INSERT, not just on the way to the stream.
	// Masking only the emitted event would scrub a secret out of audit.log while
	// leaving it in the change_log row — and settings.ExportAudit reads those rows
	// directly, so the CSV an auditor downloads would carry the very thing the
	// file was careful to remove.
	//
	// Mask FIRST, then trim (AM-1): trimming first can cut a secret at the
	// 4096-byte boundary, and the surviving prefix is no longer a dictionary
	// hit. target is masked too (AM-2) — it is filled from operator input at
	// several sites (env-var names, hosts, the denied scope).
	target = TrimForAudit(mask(target))
	details = TrimForAudit(mask(details))
	acting := ActingAgency(ctx)
	_, err := db.ExecContext(ctx,
		`INSERT INTO change_log (at, actor, category, action, target, details, created_at, acting_agency, agency_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		now, actor, category, action, target, details, now, nullStr(acting), nullStr(agency))
	if err != nil {
		return fmt.Errorf("write change_log: %w", err)
	}
	// Stream AFTER the row is durable — the database is authoritative and the
	// file is an export of it (see Sink).
	emit(Event{
		At: now, Source: "change_log",
		Actor: actor, ActingAgency: acting, Agency: agency, Category: category, Action: action,
		Target: target, Details: details,
	})
	return nil
}

// WriteActivity inserts one activity row (the full 22-column schema). Optional
// text columns are NULL when empty, numeric columns NULL when zero, and outcome
// NULL when empty (its CHECK constraint forbids ”).
func WriteActivity(ctx context.Context, db Execer, p ActivityParams) error {
	now := stampOr(p.At)
	// See WriteChangeLogAt: the row and the stream line must carry the same text,
	// and masking runs before trimming. Summary is bounded as well as masked — it
	// is free text from many call sites, so leaving it untrimmed left a hole in
	// the "one pathological value cannot make an audit record unbounded"
	// guarantee that details already had. Job/workflow names are deliberately
	// NOT masked: they come from definitions, never from secret material, and
	// History joins on them.
	p.Target = TrimForAudit(mask(p.Target))
	p.Details = TrimForAudit(mask(p.Details))
	p.Summary = TrimForAudit(mask(p.Summary))
	res, err := db.ExecContext(ctx, `
		INSERT INTO activity
			(kind, outcome, actor, job_name, job_id, workflow_name, workflow_id,
			 job_uid, workflow_uid,
			 target, scope, schedule_file, category, action, summary, details,
			 trace_id, commit_sha, repository, branch, duration_ms, killed_by, at, created_at,
			 runner_name)
		VALUES (?,?,?,?,?,?,?,
			COALESCE(?,
			         (SELECT r.job_uid FROM runs r WHERE r.id = ?),
			         (SELECT j.uid FROM jobs j WHERE j.name = ?
			           AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name) = 1)),
			COALESCE(?,
			         (SELECT wr.workflow_uid FROM workflow_runs wr WHERE wr.id = ?),
			         (SELECT w.uid FROM workflows w WHERE w.name = ?
			           AND (SELECT COUNT(*) FROM workflows w2 WHERE w2.name = w.name) = 1)),
			?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
			?)
	`,
		p.Kind, nullStr(p.Outcome), p.Actor,
		nullStr(p.JobName), nullInt64(p.JobID),
		nullStr(p.WorkflowName), nullInt64(p.WorkflowID),
		nullStr(p.JobUID), nullStr(p.TraceID), nullStr(p.JobName),
		nullStr(p.WorkflowUID), nullStr(p.TraceID), nullStr(p.WorkflowName),
		nullStr(p.Target), nullStr(p.Scope),
		nullStr(p.ScheduleFile), nullStr(p.Category),
		nullStr(p.Action), nullStr(p.Summary), nullStr(p.Details),
		nullStr(p.TraceID),
		nullStr(p.CommitSha), nullStr(p.Repository), nullStr(p.Branch),
		nullInt64(p.DurationMs), nullStr(p.KilledBy),
		now, now,
		nullStr(p.RunnerName),
	)
	if err != nil {
		return err
	}
	// The agency the event is of (2.5.0 C1): the one the writer named, else
	// the row's own object's, read from the keys the INSERT just resolved, in
	// migration 1520's order. Never the agency the request was viewing.
	if id, ierr := res.LastInsertId(); ierr == nil {
		if _, err := db.ExecContext(ctx, `UPDATE activity SET agency_id = `+activityAgencySQL+` WHERE id = ?`,
			nullStr(p.AgencyID), id); err != nil {
			return err
		}
		// The stream line says whose too; read back what the row worked out.
		if q, ok := db.(rowQuerier); ok && p.AgencyID == "" {
			var a sql.NullString
			if q.QueryRowContext(ctx, `SELECT agency_id FROM activity WHERE id = ?`, id).Scan(&a) == nil {
				p.AgencyID = a.String
			}
		}
	}
	if ev, ok := activityEvent(now, p); ok {
		emit(ev)
	}
	return nil
}

// rowQuerier is the read half of a *sql.DB or *sql.Tx, when the Execer has it.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// activityAgencySQL is an activity row's agency, over the row itself: the
// writer's (?), else its run's owner, its workflow run's agency, its job's,
// its workflow's. NULL when none resolves: the row reads as Global's.
var activityAgencySQL = `COALESCE(?,
	NULLIF((SELECT ru.owner_agency FROM runs ru WHERE ru.id = activity.trace_id), ''),
	NULLIF((SELECT wr.agency_id FROM workflow_runs wr WHERE wr.id = activity.trace_id), ''),
	NULLIF((SELECT ` + agencyid.JobSQL("aj") + ` FROM jobs aj WHERE aj.uid = activity.job_uid), ''),
	NULLIF((SELECT ` + agencyid.WorkflowSQL + ` FROM workflows w WHERE w.uid = activity.workflow_uid), ''))`

// stampOr returns at when set, else the current UTC time in RFC3339.
func stampOr(at string) string {
	if at != "" {
		return at
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
