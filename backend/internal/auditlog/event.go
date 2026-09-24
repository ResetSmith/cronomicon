package auditlog

import (
	"strings"
	"sync/atomic"
)

// EventVersion is the schema version stamped on every emitted event.
//
// audit.log is consumed by things outside this repo — a SIEM, a log shipper, an
// auditor's script — so the line shape is a contract. Bump this when a key
// changes meaning or is removed; adding an optional key is backward-compatible
// and does not.
const EventVersion = 1

// Event is one line of the audit stream (LU-10).
//
// It is a flattened union of the three sources rather than a nested per-source
// shape, because the consumer's first question is almost always "everything this
// actor did, in order" — which a flat record answers with a grep. Empty fields
// are omitted so a login event does not carry a dozen null job columns.
type Event struct {
	V      int    `json:"v"`
	At     string `json:"at"`
	Source string `json:"source"` // change_log | activity | auth
	Kind   string `json:"kind,omitempty"`

	Actor    string `json:"actor,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Category string `json:"category,omitempty"`
	Action   string `json:"action,omitempty"`
	Target   string `json:"target,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Details  string `json:"details,omitempty"`

	// Execution context, present on activity rows.
	JobName      string `json:"jobName,omitempty"`
	WorkflowName string `json:"workflowName,omitempty"`
	TraceID      string `json:"traceId,omitempty"`
	Scope        string `json:"scope,omitempty"`
	DurationMs   int64  `json:"durationMs,omitempty"`
	KilledBy     string `json:"killedBy,omitempty"`

	// Auth context, present on auth_events rows.
	Reason     string `json:"reason,omitempty"`
	RemoteAddr string `json:"remoteAddr,omitempty"`
	ClientIP   string `json:"clientIp,omitempty"`
	UserAgent  string `json:"userAgent,omitempty"`
}

// Sink receives each audited event AFTER it has been durably written to the
// database.
//
// The database is authoritative and the file is an export stream — they are not
// written in one transaction, so a crash between them can leave a row with no
// line. Declaring which one wins is what makes that survivable: a missing line
// is recoverable from the tables, and the reverse is not, so the row goes first.
type Sink func(Event)

// sink is process-global because auditlog is the single writer for all three
// tables and is a leaf package taking *sql.DB — threading a sink through every
// caller would mean touching every audit site in the codebase to deliver one
// value that is set once, in main. Tests set and restore it directly.
var sink atomic.Pointer[Sink]

// SetSink installs the audit-stream sink. Passing nil detaches it, after which
// events are written to the database only.
func SetSink(s Sink) {
	if s == nil {
		sink.Store(nil)
		return
	}
	sink.Store(&s)
}

// emit forwards an event to the sink, if one is installed.
func emit(ev Event) {
	p := sink.Load()
	if p == nil {
		return
	}
	ev.V = EventVersion
	(*p)(ev)
}

// streamedActivityKinds is the subset of activity kinds that reach audit.log
// (LU-Q10(b): the change_log spine plus a NAMED subset of activity).
//
// The line is drawn at "would an auditor ask about this?", which excludes pure
// run telemetry and includes outcomes:
//
//   - run-end / workflow-end are outcomes — what ran, for whom, and whether it
//     succeeded. change_log alone loses all of it.
//   - config carries the events change_log cannot: runner registration-token
//     mint and revoke among them.
//   - gitsync / push are changes to what the system will execute.
//
// run-start and workflow-start are deliberately excluded: they are the
// announcement half of a pair whose other half already carries the outcome, so
// including them would roughly double the stream to say nothing new.
var streamedActivityKinds = map[string]bool{
	"run-end":      true,
	"workflow-end": true,
	"config":       true,
	"gitsync":      true,
	"push":         true,
}

// activityEvent projects an activity row onto the stream shape, or reports false
// when the kind is not streamed.
func activityEvent(at string, p ActivityParams) (Event, bool) {
	if !streamedActivityKinds[p.Kind] {
		return Event{}, false
	}
	return Event{
		At:           at,
		Source:       "activity",
		Kind:         p.Kind,
		Actor:        p.Actor,
		Outcome:      p.Outcome,
		Category:     p.Category,
		Action:       p.Action,
		Target:       p.Target,
		Summary:      p.Summary,
		Details:      p.Details,
		JobName:      p.JobName,
		WorkflowName: p.WorkflowName,
		TraceID:      p.TraceID,
		Scope:        p.Scope,
		DurationMs:   p.DurationMs,
		KilledBy:     p.KilledBy,
	}, true
}

// redactor is the hook that masks secret material in audit values.
//
// main installs one at boot — redactdict.Install — over the process-wide
// redaction dictionary (every stored secret, stored SSH credential, encrypted
// settings column and multi-line env_var the server can decrypt), so the
// audit stream carries the same masking as run logs. It rebuilds on every
// write to those sources and reports a degraded build once per outage as a
// `system / Audit / redactor-unavailable` change_log row (AM,
// the audit-redaction plan). Vault-sourced values exist only at
// dispatch time, per run, and are the documented residual.
//
// The exposure here was always narrower than the process log's: audit rows
// are assembled from structured fields rather than arbitrary process output.
// What the masker covers is exactly the caller-supplied text columns, on
// every writer, before the INSERT (AM-2):
//
//	change_log:  target, details
//	activity:    target, summary, details
//	auth_events: reason, target, details
//
// Job and workflow names are NOT masked — they come from definitions, never
// from secret material, and History joins on them. Masking runs BEFORE
// TrimForAudit (AM-1) so a secret straddling the length cut cannot survive as
// a prefix. This package stays a leaf: it takes the masker as a plain
// func(string) string and never imports the dictionary.
var redactor atomic.Pointer[func(string) string]

// SetRedactor installs a masking function applied to the free-text audit
// fields before they reach the stream and the database. Passing nil removes
// it. Installed once by main (redactdict.Install); tests install their own.
func SetRedactor(f func(string) string) {
	if f == nil {
		redactor.Store(nil)
		return
	}
	redactor.Store(&f)
}

func mask(s string) string {
	if s == "" {
		return s
	}
	p := redactor.Load()
	if p == nil {
		return s
	}
	return (*p)(s)
}

// TrimForAudit bounds a free-text field so one pathological value cannot make an
// audit line unbounded. Exported for callers assembling `details` themselves.
func TrimForAudit(s string) string {
	const max = 4096
	if len(s) <= max {
		return s
	}
	return strings.TrimSpace(s[:max]) + "…[truncated]"
}
