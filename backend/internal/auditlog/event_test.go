// Tests for the audit-stream hook the three writers share (LU-10).
//
// Why these matter: the sink is a side effect of a database write, installed
// once in main and invisible from every call site. Three things can go wrong
// without any caller noticing. The stream can grow to include telemetry nobody
// audits, which roughly doubles a file operators pay to retain for years. It can
// claim events the database does not have, which destroys the one guarantee that
// makes a lossy export stream acceptable — that the tables are authoritative and
// the file is recoverable from them. And it can leak secret material into a
// durable file that outlives every other copy. Each has a test below.
package auditlog

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// sinkCapture records what reached the stream.
type sinkCapture struct {
	mu     sync.Mutex
	events []Event
}

func (c *sinkCapture) record(ev Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
}

func (c *sinkCapture) all() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *sinkCapture) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

// installCapture attaches a recording sink AND restores whatever was there
// before. The sink is process-global, so a test that installs one without
// restoring it silently poisons every later test in the package — including the
// ones asserting that nothing is emitted.
func installCapture(t *testing.T) *sinkCapture {
	t.Helper()
	prev := sink.Load()
	t.Cleanup(func() { sink.Store(prev) })
	c := &sinkCapture{}
	SetSink(c.record)
	return c
}

// installRedactor is the same discipline for the masking hook.
func installRedactor(t *testing.T, f func(string) string) {
	t.Helper()
	prev := redactor.Load()
	t.Cleanup(func() { redactor.Store(prev) })
	SetRedactor(f)
}

// TestWriteActivityStreamsOnlyTheAuditableKinds is LU-Q10(b), and it is the
// property that keeps audit.log affordable.
//
// activity is mostly run telemetry. run-start and workflow-start are the
// announcement half of a pair whose other half already carries the outcome, so
// streaming them would roughly double the file to say nothing an auditor asked
// for — on an install retaining years of compliance evidence that is a real
// cost, and the regression is invisible because the extra lines look perfectly
// well-formed. The named subset (outcomes, config, and changes to what the
// system will execute) is what actually gets streamed.
func TestWriteActivityStreamsOnlyTheAuditableKinds(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	cases := []struct {
		kind   string
		stream bool
		why    string
	}{
		{"run-end", true, "an outcome: what ran, for whom, and whether it succeeded"},
		{"workflow-end", true, "the workflow-level outcome"},
		{"config", true, "carries what change_log cannot — runner token mint/revoke among them"},
		{"gitsync", true, "a change to what the system will execute"},
		{"push", true, "a change to what the system will execute"},
		{"run-start", false, "pure telemetry; its run-end already carries the outcome"},
		{"workflow-start", false, "pure telemetry; its workflow-end already carries the outcome"},
	}

	for _, tc := range cases {
		stream.reset()
		if err := WriteActivity(ctx, pool, ActivityParams{
			Kind: tc.kind, Outcome: "success", Actor: "alice@corp.example",
			JobName: "nightly-backup", TraceID: "trace-" + tc.kind,
		}); err != nil {
			t.Fatalf("WriteActivity(%s): %v", tc.kind, err)
		}

		// The row is written either way — the stream filter must never suppress a
		// database row.
		var rows int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind = ?`, tc.kind).Scan(&rows); err != nil {
			t.Fatalf("count %s rows: %v", tc.kind, err)
		}
		if rows != 1 {
			t.Errorf("activity rows for kind %q = %d, want 1 — every kind is still recorded in the database", tc.kind, rows)
		}

		got := stream.all()
		if tc.stream {
			if len(got) != 1 {
				t.Errorf("kind %q emitted %d events, want 1 (%s)", tc.kind, len(got), tc.why)
				continue
			}
			if got[0].Source != "activity" || got[0].Kind != tc.kind {
				t.Errorf("kind %q emitted %+v, want source=activity kind=%s", tc.kind, got[0], tc.kind)
			}
		} else if len(got) != 0 {
			t.Errorf("kind %q emitted %d events, want 0 — %s", tc.kind, len(got), tc.why)
		}
	}
}

// TestWriteChangeLogAlwaysStreams pins the other half of the filter. change_log
// is the compliance spine — every row of it is an in-app configuration change
// made by a named actor — so unlike activity it has no kinds and no exclusions.
func TestWriteChangeLogAlwaysStreams(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)

	if err := WriteChangeLog(context.Background(), pool,
		"alice@corp.example", "Settings", "updated", "general", "maxConcurrent 5 → 8"); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}

	got := stream.all()
	if len(got) != 1 {
		t.Fatalf("WriteChangeLog emitted %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Source != "change_log" {
		t.Errorf("source = %q, want change_log", ev.Source)
	}
	if ev.Actor != "alice@corp.example" || ev.Category != "Settings" ||
		ev.Action != "updated" || ev.Target != "general" || ev.Details != "maxConcurrent 5 → 8" {
		t.Errorf("emitted event lost fields: %+v", ev)
	}
	if ev.At == "" {
		t.Error("emitted event has no timestamp — the stream must carry the same `at` as the row")
	}
}

// TestWriteAuthEventWritesTheRowAndStreamsIt covers LU-9's new table. Auth
// events are the ones an incident responder reaches for first, and before this
// existed literally none of them were audited anywhere — so both halves must
// land, and the stream must label them source="auth" so a consumer can filter
// the login trail out of a mixed file.
func TestWriteAuthEventWritesTheRowAndStreamsIt(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)

	if err := WriteAuthEvent(context.Background(), pool, AuthEventParams{
		Kind: AuthLoginFailed, Outcome: OutcomeFailure, Actor: "mallory@corp.example",
		Reason: "bad_nonce", RemoteAddr: "10.0.0.9:5555", ClientIP: "198.51.100.7",
		UserAgent: "curl/8.4.0",
	}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	var kind, outcome, reason string
	if err := pool.QueryRow(
		`SELECT kind, outcome, reason FROM auth_events WHERE actor = 'mallory@corp.example'`).
		Scan(&kind, &outcome, &reason); err != nil {
		t.Fatalf("read back auth_events row: %v", err)
	}
	if kind != AuthLoginFailed || outcome != OutcomeFailure || reason != "bad_nonce" {
		t.Errorf("row = %s/%s/%s, want %s/%s/bad_nonce", kind, outcome, reason, AuthLoginFailed, OutcomeFailure)
	}

	got := stream.all()
	if len(got) != 1 {
		t.Fatalf("WriteAuthEvent emitted %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Source != "auth" {
		t.Errorf("source = %q, want auth — consumers filter the login trail on it", ev.Source)
	}
	if ev.Kind != AuthLoginFailed || ev.Outcome != OutcomeFailure || ev.Reason != "bad_nonce" ||
		ev.RemoteAddr != "10.0.0.9:5555" || ev.ClientIP != "198.51.100.7" || ev.UserAgent != "curl/8.4.0" {
		t.Errorf("emitted auth event lost fields: %+v", ev)
	}
}

// TestWriteAuthEventDefaultsAnUnsetOutcomeToFailure pins the fail-safe. An
// audit record whose outcome defaulted to "success" would let a caller that
// forgot the field turn a denial into an apparent grant — the one direction a
// default must never go.
func TestWriteAuthEventDefaultsAnUnsetOutcomeToFailure(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)

	if err := WriteAuthEvent(context.Background(), pool, AuthEventParams{
		Kind: AuthDenied, Actor: "bob@corp.example", Target: "scope:Production",
	}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	var outcome string
	if err := pool.QueryRow(`SELECT outcome FROM auth_events WHERE actor = 'bob@corp.example'`).
		Scan(&outcome); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if outcome != OutcomeFailure {
		t.Errorf("row outcome = %q, want %q — an unspecified outcome must never read as a grant", outcome, OutcomeFailure)
	}
	got := stream.all()
	if len(got) != 1 || got[0].Outcome != OutcomeFailure {
		t.Errorf("emitted outcome = %+v, want the same fail-safe default", got)
	}
}

// TestNothingIsStreamedWhenTheInsertFails is the invariant that makes a lossy
// export stream acceptable at all.
//
// The row and the line are not written in one transaction, and the declared
// asymmetry is that the row goes FIRST: a missing line is recoverable by
// re-exporting from the tables, a line with no row is not recoverable by
// anything and quietly makes the file authoritative over the database. So a
// failed insert must emit nothing whatsoever.
func TestNothingIsStreamedWhenTheInsertFails(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	for _, table := range []string{"change_log", "activity", "auth_events"} {
		if _, err := pool.Exec("DROP TABLE " + table); err != nil { //nolint:gosec // literal table names
			t.Fatalf("drop %s: %v", table, err)
		}
	}

	if err := WriteChangeLog(ctx, pool, "alice", "Settings", "updated", "general", "x"); err == nil {
		t.Error("WriteChangeLog should fail once change_log is gone")
	}
	if err := WriteActivity(ctx, pool, ActivityParams{Kind: "run-end", Outcome: "success", Actor: "alice"}); err == nil {
		t.Error("WriteActivity should fail once activity is gone")
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice"}); err == nil {
		t.Error("WriteAuthEvent should fail once auth_events is gone")
	}

	if got := stream.all(); len(got) != 0 {
		t.Errorf("%d events reached the stream for writes the database rejected: %+v\n"+
			"audit.log must never claim an event the tables do not have", len(got), got)
	}
}

// TestSetSinkNilDetachesCleanly covers teardown. main can re-point or disable
// the stream at runtime; a nil sink must mean "database only", not a nil-pointer
// dereference on the next audited action — which, since every audited action is
// on a request path, would be a crash rather than a degraded log.
func TestSetSinkNilDetachesCleanly(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	if err := WriteChangeLog(ctx, pool, "alice", "Settings", "updated", "a", ""); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if len(stream.all()) != 1 {
		t.Fatalf("sanity: the capture sink is not attached")
	}

	SetSink(nil)
	if err := WriteChangeLog(ctx, pool, "alice", "Settings", "updated", "b", ""); err != nil {
		t.Fatalf("WriteChangeLog after detach: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{Kind: "run-end", Outcome: "success", Actor: "alice"}); err != nil {
		t.Fatalf("WriteActivity after detach: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{Kind: AuthLogout, Outcome: OutcomeSuccess, Actor: "alice"}); err != nil {
		t.Fatalf("WriteAuthEvent after detach: %v", err)
	}

	if got := stream.all(); len(got) != 1 {
		t.Errorf("detached sink still received %d events, want the original 1", len(got))
	}
	// The rows must still be there: detaching the stream is not detaching the audit.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&n); err != nil {
		t.Fatalf("count change_log: %v", err)
	}
	if n != 2 {
		t.Errorf("change_log rows = %d, want 2 — a detached sink must not stop the database writes", n)
	}
}

// TestRedactorMasksEveryAuditlogWriter is the leak guard. `details` is free
// text assembled at dozens of call sites, and audit.log is durable, shipped
// off-box and retained for years — so a secret that lands there outlives every
// other copy of itself. Installing the masker on only two of the three writers
// would leave a hole that no call site can see.
//
// "Every writer" means every writer in THIS package; that this is every writer
// in the repo is a separate invariant, pinned by writer_conformance_test.go.
func TestRedactorMasksEveryAuditlogWriter(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	installRedactor(t, func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") })
	ctx := context.Background()

	const secret = "rotated token to hunter2"

	if err := WriteChangeLog(ctx, pool, "alice", "Secrets", "rotated", "vault-token", secret); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{
		Kind: "config", Actor: "alice", Summary: secret, Details: secret,
	}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{
		Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice", Details: secret,
	}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	got := stream.all()
	if len(got) != 3 {
		t.Fatalf("emitted %d events, want 3 (one per writer)", len(got))
	}
	for _, ev := range got {
		if strings.Contains(ev.Details, "hunter2") {
			t.Errorf("source %q emitted unmasked details %q — the redactor is not applied on this writer", ev.Source, ev.Details)
		}
		if !strings.Contains(ev.Details, "***") {
			t.Errorf("source %q details = %q, want the masked form", ev.Source, ev.Details)
		}
	}
	// Summary is free text too, and the activity writer masks it alongside details.
	for _, ev := range got {
		if ev.Source == "activity" && strings.Contains(ev.Summary, "hunter2") {
			t.Errorf("activity emitted an unmasked summary %q", ev.Summary)
		}
	}
}

// TestTrimForAuditBoundsALongValue stops one pathological value making an audit
// line unbounded. A JSON Lines consumer reads a line at a time; a multi-megabyte
// record — a stack trace, a diff, a base64 blob pasted into a details field —
// can exceed a shipper's line limit and take out the whole stream, not just that
// record.
func TestTrimForAuditBoundsALongValue(t *testing.T) {
	const limit = 4096
	short := strings.Repeat("a", limit)
	if got := TrimForAudit(short); got != short {
		t.Errorf("a value at the limit was altered (len %d → %d)", len(short), len(got))
	}

	long := strings.Repeat("b", 10*limit)
	got := TrimForAudit(long)
	if len(got) >= len(long) {
		t.Errorf("TrimForAudit did not shorten a %d-byte value (got %d)", len(long), len(got))
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("a truncated value must say so; got tail %q", got[max(0, len(got)-20):])
	}
}

// TestEmittedDetailsAreTrimmed is the plumbing half of the bound: TrimForAudit
// existing is worthless if the writers do not call it, and a unit test on the
// function alone would stay green while every writer streamed unbounded lines.
func TestEmittedDetailsAreTrimmed(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()
	huge := strings.Repeat("z", 50000)

	if err := WriteChangeLog(ctx, pool, "alice", "Settings", "updated", "general", huge); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{Kind: "config", Actor: "alice", Details: huge}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{
		Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice", Details: huge,
	}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	for _, ev := range stream.all() {
		if len(ev.Details) >= len(huge) {
			t.Errorf("source %q streamed %d bytes of details unbounded", ev.Source, len(ev.Details))
		}
	}
}

// TestEveryEmittedEventCarriesTheSchemaVersion pins the one field consumers use
// to decide how to read the rest. emit stamps it centrally rather than trusting
// each writer, so a new writer added later cannot ship version-less lines; this
// asserts the stamp survives even when the caller supplies a bogus version.
func TestEveryEmittedEventCarriesTheSchemaVersion(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	if err := WriteChangeLog(ctx, pool, "alice", "Settings", "updated", "general", ""); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{Kind: "run-end", Outcome: "success", Actor: "alice"}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice"}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	got := stream.all()
	if len(got) != 3 {
		t.Fatalf("emitted %d events, want 3", len(got))
	}
	for _, ev := range got {
		if ev.V != EventVersion {
			t.Errorf("source %q emitted v=%d, want %d", ev.Source, ev.V, EventVersion)
		}
	}

	// A caller's own V is overwritten, not trusted.
	var seen Event
	prev := sink.Load()
	t.Cleanup(func() { sink.Store(prev) })
	SetSink(func(ev Event) { seen = ev })
	emit(Event{V: 99, Source: "change_log"})
	if seen.V != EventVersion {
		t.Errorf("emit passed a caller-supplied v=99 through as %d, want %d", seen.V, EventVersion)
	}
}

// TestActivityEventProjectionCarriesExecutionContext guards the fields that make
// an activity line answer an auditor's question. "A job ran" is not auditable;
// which job, under which workflow, in which scope, how long it took and who
// killed it is. These are dropped one at a time by ordinary refactors and
// nothing else in the system notices.
func TestActivityEventProjectionCarriesExecutionContext(t *testing.T) {
	ev, ok := activityEvent("2026-03-14T10:00:00Z", ActivityParams{
		Kind: "run-end", Outcome: "failure", Actor: "runner:r1",
		JobName: "nightly-backup", WorkflowName: "nightly", TraceID: "t-9",
		Scope: "Production", DurationMs: 4321, KilledBy: "operator",
		Category: "executions", Action: "run", Target: "db01", Summary: "exit 1", Details: "boom",
	})
	if !ok {
		t.Fatal("run-end must be streamed")
	}
	want := Event{
		At: "2026-03-14T10:00:00Z", Source: "activity", Kind: "run-end", Actor: "runner:r1",
		Outcome: "failure", Category: "executions", Action: "run", Target: "db01",
		Summary: "exit 1", Details: "boom", JobName: "nightly-backup",
		WorkflowName: "nightly", TraceID: "t-9", Scope: "Production",
		DurationMs: 4321, KilledBy: "operator",
	}
	if ev != want {
		t.Errorf("activity projection dropped or renamed fields:\n got %+v\nwant %+v", ev, want)
	}

	if _, ok := activityEvent("2026-03-14T10:00:00Z", ActivityParams{Kind: "run-start"}); ok {
		t.Error("run-start must not be streamed")
	}
}

// TestRedactorMasksTheDatabaseRowAndNotOnlyTheStream is the regression that
// shipped and had to be fixed after the fact.
//
// The first cut applied the masker inside the emit() call only, so a redactor
// scrubbed a secret out of audit.log while the change_log / activity ROW kept it
// verbatim — and settings.ExportAudit reads those rows directly, so the CSV an
// auditor downloads would have carried the very thing the file was careful to
// remove. WriteAuthEvent masked before its INSERT; the other two did not, and
// nothing noticed because every existing assertion looked at the emitted event.
//
// So this deliberately does NOT look at the stream. It reads the stored columns,
// which is the only place the two implementations differ.
func TestRedactorMasksTheDatabaseRowAndNotOnlyTheStream(t *testing.T) {
	pool := openTestPool(t)
	installCapture(t) // installed but ignored — the row is what is under test
	installRedactor(t, func(s string) string { return strings.ReplaceAll(s, "hunter2", "***") })
	ctx := context.Background()

	const secret = "rotated token to hunter2"

	// Every caller-supplied text column carries the secret (AM-2): target on
	// all three writers and reason on auth events, not only details/summary.
	if err := WriteChangeLog(ctx, pool, "alice", "Secrets", "rotated", secret, secret); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{
		Kind: "config", Actor: "alice", Target: secret, Summary: secret, Details: secret,
	}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{
		Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice",
		Reason: secret, Target: secret, Details: secret,
	}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	for _, q := range []struct{ what, query string }{
		{"change_log.target", `SELECT COALESCE(target,'') FROM change_log`},
		{"change_log.details", `SELECT COALESCE(details,'') FROM change_log`},
		{"activity.target", `SELECT COALESCE(target,'') FROM activity`},
		{"activity.details", `SELECT COALESCE(details,'') FROM activity`},
		{"activity.summary", `SELECT COALESCE(summary,'') FROM activity`},
		{"auth_events.reason", `SELECT COALESCE(reason,'') FROM auth_events`},
		{"auth_events.target", `SELECT COALESCE(target,'') FROM auth_events`},
		{"auth_events.details", `SELECT COALESCE(details,'') FROM auth_events`},
	} {
		var stored string
		if err := pool.QueryRowContext(ctx, q.query).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", q.what, err)
		}
		if strings.Contains(stored, "hunter2") {
			t.Errorf("%s stored the UNMASKED value %q — the masker runs only on the way to the stream, so the row and the CSV export still leak it",
				q.what, stored)
		}
		if !strings.Contains(stored, "***") {
			t.Errorf("%s = %q, want the masked form", q.what, stored)
		}
	}
}

// TestMaskRunsBeforeTrim pins the order of the two transforms (AM-1).
//
// mask(TrimForAudit(x)) — the original order — cuts at 4096 bytes and THEN
// looks for dictionary hits. A secret straddling the cut leaves a prefix the
// dictionary does not contain, so it survives into the row, the stream and the
// export. The token here is placed so the cut falls inside it; neither the
// whole token nor its head may reach any writer's stored columns.
func TestMaskRunsBeforeTrim(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	const token = "SECRET-0123456789abcdef0123456789abcdef" // 40 bytes
	head := token[:20]
	installRedactor(t, func(s string) string { return strings.ReplaceAll(s, token, "***") })

	// 4076 bytes of filler puts the cut 20 bytes into the token.
	text := strings.Repeat("x", 4076) + token + " trailing"

	if err := WriteChangeLog(ctx, pool, "alice", "Secrets", "rotated", "t", text); err != nil {
		t.Fatalf("WriteChangeLog: %v", err)
	}
	if err := WriteActivity(ctx, pool, ActivityParams{Kind: "config", Actor: "alice", Summary: text, Details: text}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}
	if err := WriteAuthEvent(ctx, pool, AuthEventParams{Kind: AuthLogin, Outcome: OutcomeSuccess, Actor: "alice", Details: text}); err != nil {
		t.Fatalf("WriteAuthEvent: %v", err)
	}

	for _, q := range []struct{ what, query string }{
		{"change_log.details", `SELECT COALESCE(details,'') FROM change_log`},
		{"activity.details", `SELECT COALESCE(details,'') FROM activity`},
		{"activity.summary", `SELECT COALESCE(summary,'') FROM activity`},
		{"auth_events.details", `SELECT COALESCE(details,'') FROM auth_events`},
	} {
		var stored string
		if err := pool.QueryRowContext(ctx, q.query).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", q.what, err)
		}
		if strings.Contains(stored, head) {
			t.Errorf("%s kept the head of a secret split by the length cut — trim ran before mask", q.what)
		}
		if !strings.Contains(stored, "***") {
			t.Errorf("%s = …%q, want the masked form", q.what, stored[len(stored)-40:])
		}
	}
	for _, ev := range stream.all() {
		for what, v := range map[string]string{"details": ev.Details, "summary": ev.Summary} {
			if strings.Contains(v, head) {
				t.Errorf("source %q streamed the head of a split secret in %s", ev.Source, what)
			}
		}
	}
}

// TestActivitySummaryIsLengthBounded closes the other half of the same gap:
// details was trimmed and summary was not, so one pathological summary could
// still make an audit record unbounded — in the row, the stream line, and the
// export. Summary is free text from many call sites, same as details.
func TestActivitySummaryIsLengthBounded(t *testing.T) {
	pool := openTestPool(t)
	stream := installCapture(t)
	ctx := context.Background()

	huge := strings.Repeat("x", 20_000)
	if err := WriteActivity(ctx, pool, ActivityParams{
		Kind: "config", Actor: "alice", Summary: huge, Details: huge,
	}); err != nil {
		t.Fatalf("WriteActivity: %v", err)
	}

	var storedSummary string
	if err := pool.QueryRowContext(ctx, `SELECT COALESCE(summary,'') FROM activity`).Scan(&storedSummary); err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if len(storedSummary) >= len(huge) {
		t.Errorf("activity.summary stored %d bytes untrimmed; details is bounded and summary must be too", len(storedSummary))
	}
	got := stream.all()
	if len(got) != 1 {
		t.Fatalf("emitted %d events, want 1", len(got))
	}
	if len(got[0].Summary) >= len(huge) {
		t.Errorf("emitted summary is %d bytes untrimmed", len(got[0].Summary))
	}
}
