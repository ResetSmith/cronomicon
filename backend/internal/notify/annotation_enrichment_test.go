package notify

import (
	"database/sql"
	"strings"
	"testing"
)

// AN-4 (the annotations plan) — failure notifications name the
// contact and say when something is critical.
//
// Everything here drives the REAL dispatch and captures at the transport seam,
// because the parts that can silently stop working are the identity resolution
// and the placement, not the string formatting: an enrichment that looks up the
// wrong uid returns no rows and sends a body that is correct-looking and
// missing the point.

// annEnv is one seeded dispatcher plus the captured transport output.
type annEnv struct {
	d       *Dispatcher
	pool    *sql.DB
	exec    func(string, ...any)
	subject *string
	body    *string
	sends   *int
}

// newAnnEnv seeds an all-jobs email rule and captures what the email transport
// is handed.
func newAnnEnv(t *testing.T) annEnv {
	t.Helper()
	d, pool := newDispatcherDB(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// target_mode 'all' so every test's event matches and the assertions are
	// about the BODY, never about whether a rule fired.
	//
	// TWO rules, because one cannot cover both event kinds: `any` deliberately
	// aliases run OUTCOMES only (FX-D4 — letting it cover missed runs would have
	// turned every existing rule in the fleet into a weekend pager), so an alert
	// needs its trigger named explicitly.
	exec(`INSERT INTO alert_config
		(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
		VALUES ('r-all','all',NULL,'any','["email"]','ops@example.com',1,'now')`)
	exec(`INSERT INTO alert_config
		(id, target_mode, job_name, trigger, channels, recipients, enabled, created_at)
		VALUES ('r-missed','all',NULL,'missed-run','["email"]','ops@example.com',1,'now')`)
	exec(`INSERT INTO notification_config
		(id, smtp_host, smtp_port, smtp_from, smtp_encryption, smtp_recipients, apprise_enabled, apprise_targets, last_modified_at)
		VALUES (1,'mail.example.com',587,'cronomicon@example.com','starttls','[]',0,'[]','now')`)

	var subject, body string
	sends := 0
	d.sendEmail = func(_ SMTP, _ []string, s, b string) error {
		sends++
		subject, body = s, b
		return nil
	}
	return annEnv{d: d, pool: pool, exec: exec, subject: &subject, body: &body, sends: &sends}
}

func annotate(t *testing.T, e annEnv, kind, uid, contact string, critical bool) {
	t.Helper()
	crit := 0
	if critical {
		crit = 1
	}
	e.exec(`INSERT INTO annotations(owner_kind, owner_uid, critical, contact, notes, updated_by, updated_at)
	        VALUES (?, ?, ?, ?, 'some notes', 'operator', 't')`, kind, uid, crit, contact)
}

// TestFailureNotificationCarriesCriticalAndContact is the payoff case.
func TestFailureNotificationCarriesCriticalAndContact(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-backup','bash','t')`)
	e.exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	        VALUES('trace-1','backup','git','uid-backup','bash','failure','t','manual','t')`)
	annotate(t, e, "job", "uid-backup", "dba-oncall@corp.example", true)

	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{
		TraceID: "trace-1", JobName: "backup", Status: "failure",
	})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *e.sends != 1 {
		t.Fatalf("sends = %d, want 1", *e.sends)
	}
	if !strings.Contains(*e.body, "Critical: yes") {
		t.Errorf("body missing the criticality line:\n%s", *e.body)
	}
	if !strings.Contains(*e.body, "Contact:  dba-oncall@corp.example") {
		t.Errorf("body missing the contact line:\n%s", *e.body)
	}
	// AN-Q4 — the subject is UNTOUCHED. Operators filter mail on this shape and
	// a silent change breaks those filters at 3am.
	if *e.subject != "[Cronomicon] backup FAILED" {
		t.Errorf("subject = %q, want the unchanged shape", *e.subject)
	}
	// The notes themselves stay out: a notification is not where you read them.
	if strings.Contains(*e.body, "some notes") {
		t.Errorf("body carries the notes; only criticality and contact belong here:\n%s", *e.body)
	}
}

// TestUnannotatedNotificationIsByteIdentical — the un-enriched body must not
// drift by so much as a newline, since that is what every existing deployment
// receives today.
func TestUnannotatedNotificationIsByteIdentical(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-backup','bash','t')`)
	e.exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	        VALUES('trace-1','backup','git','uid-backup','bash','failure','t','manual','t')`)

	ev := RunEvent{TraceID: "trace-1", JobName: "backup", Status: "failure", Scope: "Prod"}
	if err := e.d.dispatch(t.Context(), runDispatchEvent(ev)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, want := renderMessage(ev)
	if *e.body != want {
		t.Errorf("un-annotated body drifted from renderMessage:\n got %q\nwant %q", *e.body, want)
	}

	// An annotation that is empty of the two pushed facts is the same as none:
	// notes alone add no line.
	annotate(t, e, "job", "uid-backup", "", false)
	if err := e.d.dispatch(t.Context(), runDispatchEvent(ev)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if *e.body != want {
		t.Errorf("notes-only annotation changed the body:\n got %q\nwant %q", *e.body, want)
	}
}

// TestAnnotatedWorkflowFailureIsEnriched is the guard against shipping the job
// half alone.
//
// Workflow events carry NO uid — resolveEventJobUID returns "" for them by
// design — so a jobUID-only enrichment would leave this body byte-identical to
// an un-annotated one while the workflow's catalog row wears a Critical chip.
func TestAnnotatedWorkflowFailureIsEnriched(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO workflows(name, source, uid, steps, synced_at) VALUES('release','git','uid-release','[]','t')`)
	annotate(t, e, "workflow", "uid-release", "#platform-oncall", true)

	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{
		JobName: "release", OwnerKind: "workflow", Status: "failure",
	})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(*e.body, "Critical: yes") || !strings.Contains(*e.body, "#platform-oncall") {
		t.Errorf("workflow failure was not enriched:\n%s", *e.body)
	}
}

// TestWorkflowDoesNotBorrowASameNamedJobsAnnotation — the two halves are keyed
// by (kind, uid) and resolved through different tables. A workflow picking up a
// job's contact is the same class of confusion targetMatches' "job rule matches
// JOBS only" arm exists to prevent.
func TestWorkflowDoesNotBorrowASameNamedJobsAnnotation(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('release','git','uid-job','bash','t')`)
	e.exec(`INSERT INTO workflows(name, source, uid, steps, synced_at) VALUES('release','git','uid-wf','[]','t')`)
	annotate(t, e, "job", "uid-job", "job-owner@corp.example", true)

	if err := e.d.dispatch(t.Context(), runDispatchEvent(RunEvent{
		JobName: "release", OwnerKind: "workflow", Status: "failure",
	})); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if strings.Contains(*e.body, "job-owner@corp.example") {
		t.Errorf("workflow event borrowed the same-named JOB's annotation:\n%s", *e.body)
	}
}

// TestMissedRunAlertIsEnrichedThroughTheNameArm pins the decision that replaced
// the band's original "uid only, never a by-name guess" rule.
//
// A missed run has no trace id — there is no run, that is the point — so the
// event's uid can only come from resolveEventJobUID's uniqueness-gated name arm.
// Reading the raw event field instead of the resolved one would blank the
// contact line for exactly the alerts this stage exists to serve.
func TestMissedRunAlertIsEnrichedThroughTheNameArm(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('nightly','git','uid-nightly','bash','t')`)
	annotate(t, e, "job", "uid-nightly", "batch-team@corp.example", true)

	if err := e.d.dispatch(t.Context(), dispatchEvent{
		jobName: "nightly", matchKey: TriggerMissedRun, isRunOutcome: false,
		subject: "missed", body: "no run appeared\n",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if !strings.Contains(*e.body, "batch-team@corp.example") {
		t.Errorf("run-less alert lost its contact line:\n%s", *e.body)
	}
}

// TestAmbiguousNameIsNotEnriched — the uniqueness gate is what makes the name
// arm safe. Two jobs share a name and the event carries no uid, so there is no
// honest answer; the message goes out un-enriched rather than naming one
// department's contact on the other's failure.
func TestAmbiguousNameIsNotEnriched(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','amadeus','uid-fin','bash','t')`)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-plat','bash','t')`)
	annotate(t, e, "job", "uid-fin", "finance-dba@corp.example", true)

	if err := e.d.dispatch(t.Context(), dispatchEvent{
		jobName: "backup", matchKey: TriggerMissedRun, isRunOutcome: false,
		subject: "missed", body: "no run appeared\n",
	}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if strings.Contains(*e.body, "finance-dba@corp.example") {
		t.Errorf("an ambiguous name resolved to one twin's annotation:\n%s", *e.body)
	}
	if *e.sends != 1 {
		t.Errorf("sends = %d, want 1 — declining to enrich must not drop the alert", *e.sends)
	}
}

// TestEnrichmentDoesNotLeakIntoTheEventIdentity — resolveAnnotationOwner may
// read a workflow's uid but must never assign it to ev.jobUID, which feeds
// targetMatches.
func TestEnrichmentDoesNotLeakIntoTheEventIdentity(t *testing.T) {
	d, pool := newDispatcherDB(t)
	if _, err := pool.Exec(`INSERT INTO workflows(name, source, uid, steps, synced_at) VALUES('release','git','uid-wf','[]','t')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ev := dispatchEvent{jobName: "release", ownerKind: "workflow"}
	kind, uid := d.resolveAnnotationOwner(t.Context(), ev)
	if kind != "workflow" || uid != "uid-wf" {
		t.Fatalf("resolveAnnotationOwner = (%q, %q), want (workflow, uid-wf)", kind, uid)
	}
	if ev.jobUID != "" {
		t.Errorf("ev.jobUID = %q after resolution, want empty — a workflow holding a job uid is what targetMatches guards against", ev.jobUID)
	}
}

// TestAnnotationLookupFailureStillSends — failure-open. The annotations table is
// dropped out from under the dispatcher, which is the bluntest possible version
// of "the lookup errored"; the notification must still go out.
func TestAnnotationLookupFailureStillSends(t *testing.T) {
	e := newAnnEnv(t)
	e.exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('backup','git','uid-backup','bash','t')`)
	e.exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	        VALUES('trace-1','backup','git','uid-backup','bash','failure','t','manual','t')`)
	e.exec(`DROP TABLE annotations`)

	ev := RunEvent{TraceID: "trace-1", JobName: "backup", Status: "failure"}
	if err := e.d.dispatch(t.Context(), runDispatchEvent(ev)); err != nil {
		t.Fatalf("dispatch returned an error when the annotation lookup failed: %v", err)
	}
	if *e.sends != 1 {
		t.Fatalf("sends = %d, want 1 — an annotation must never cost a notification", *e.sends)
	}
	_, want := renderMessage(ev)
	if *e.body != want {
		t.Errorf("body after a failed lookup = %q, want the un-enriched message %q", *e.body, want)
	}
}
