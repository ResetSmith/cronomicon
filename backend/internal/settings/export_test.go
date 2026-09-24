package settings

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
)

// seedAuditRows inserts one row into each audited table with deterministic,
// distinct timestamps so the streamed export order is verifiable.
func seedAuditRows(t *testing.T, pool *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	exec(`INSERT INTO change_log(at, actor, category, action, target, details, created_at)
		VALUES ('2026-01-01T01:00:00Z','alice','auth','login','session','ok','2026-01-01T01:00:00Z')`)
	exec(`INSERT INTO activity(kind, actor, summary, details, trace_id, at, created_at)
		VALUES ('config','bob','updated','x','t1','2026-01-01T02:00:00Z','2026-01-01T02:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, executor, created_at)
		VALUES ('run-1','job1','git','bash','success','carol','manual','ssh','2026-01-01T03:00:00Z')`)
	exec(`INSERT INTO schedule_pushes(at, actor, schedule_file, status, details, created_at)
		VALUES ('2026-01-01T04:00:00Z','dave','sched.yml','success','pushed','2026-01-01T04:00:00Z')`)
}

// wantRows is the expected unified export, in source-then-time order.
func wantRows() []AuditRow {
	return []AuditRow{
		{Source: "change_log", At: "2026-01-01T01:00:00Z", Actor: "alice", Category: "auth", Action: "login", Target: "session", Details: "ok"},
		{Source: "activity", At: "2026-01-01T02:00:00Z", Actor: "bob", Category: "config", Action: "updated", Details: "x", TraceID: "t1"},
		{Source: "runs", At: "2026-01-01T03:00:00Z", Actor: "carol", Category: "executions", Action: "run", Target: "job1", Status: "success", TraceID: "run-1"},
		{Source: "schedule_pushes", At: "2026-01-01T04:00:00Z", Actor: "dave", Category: "schedulePushes", Action: "push", Target: "sched.yml", Details: "pushed", Status: "success"},
	}
}

// TestExportAuditStreamJSON is the CC.10 golden: the streamed JSON must equal a
// single json.Marshal of the equivalent slice — i.e. streaming row-by-row did
// not change the output for a non-empty export.
func TestExportAuditStreamJSON(t *testing.T) {
	pool := openTestPool(t)
	seedAuditRows(t, pool)

	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", nil, "json"); err != nil {
		t.Fatalf("ExportAudit json: %v", err)
	}
	want, _ := json.Marshal(wantRows())
	if buf.String() != string(want) {
		t.Errorf("streamed JSON mismatch:\n got: %s\nwant: %s", buf.String(), want)
	}
}

func TestExportAuditStreamCSV(t *testing.T) {
	pool := openTestPool(t)
	seedAuditRows(t, pool)

	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", nil, "csv"); err != nil {
		t.Fatalf("ExportAudit csv: %v", err)
	}

	var want bytes.Buffer
	cw := csv.NewWriter(&want)
	_ = cw.Write([]string{"source", "at", "actor", "category", "action", "target", "details", "status", "traceId"})
	for _, r := range wantRows() {
		_ = cw.Write([]string{r.Source, r.At, r.Actor, r.Category, r.Action, r.Target, r.Details, r.Status, r.TraceID})
	}
	cw.Flush()
	if buf.String() != want.String() {
		t.Errorf("streamed CSV mismatch:\n got: %q\nwant: %q", buf.String(), want.String())
	}
}

// TestExportAuditEmpty pins the empty-export shape: a well-formed empty JSON
// array (not null) and a CSV header row with no data.
func TestExportAuditEmpty(t *testing.T) {
	pool := openTestPool(t)

	var jbuf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &jbuf, "", "", nil, "json"); err != nil {
		t.Fatalf("json: %v", err)
	}
	if jbuf.String() != "[]" {
		t.Errorf("empty JSON = %q, want []", jbuf.String())
	}

	var cbuf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &cbuf, "", "", nil, "csv"); err != nil {
		t.Fatalf("csv: %v", err)
	}
	if got := strings.TrimSpace(cbuf.String()); got != "source,at,actor,category,action,target,details,status,traceId" {
		t.Errorf("empty CSV = %q, want header only", got)
	}
}

// TestExportAuditFilters checks the event-type selector and date range still
// scope the streamed rows.
func TestExportAuditFilters(t *testing.T) {
	pool := openTestPool(t)
	seedAuditRows(t, pool)

	// Only executions.
	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", []string{"executions"}, "json"); err != nil {
		t.Fatalf("filtered export: %v", err)
	}
	var got []AuditRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Source != "runs" {
		t.Fatalf("executions filter = %+v, want a single runs row", got)
	}

	// Date range excludes the 04:00 schedule push (to is exclusive).
	buf.Reset()
	if err := ExportAudit(context.Background(), pool, &buf, "2026-01-01T00:00:00Z", "2026-01-01T04:00:00Z", nil, "json"); err != nil {
		t.Fatalf("ranged export: %v", err)
	}
	got = nil
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("range [00:00,04:00) returned %d rows, want 3 (schedule push at 04:00 excluded)", len(got))
	}
	for _, r := range got {
		if r.Source == "schedule_pushes" {
			t.Errorf("schedule push at 04:00 should be excluded by the exclusive upper bound")
		}
	}
}

// ── LU-9: authEvents is a source of its own, not an alias ─────────────────────

// seedAuthEvent inserts one auth_events row. actor is written as SQL NULL when
// empty, which is the legal and expected state for a pre-identity failure.
func seedAuthEvent(t *testing.T, pool *sql.DB, at, kind, outcome, actor string) {
	t.Helper()
	var a any
	if actor != "" {
		a = actor
	}
	if _, err := pool.Exec(`
		INSERT INTO auth_events(at, kind, outcome, actor, reason, target, remote_addr, client_ip, user_agent, details, created_at)
		VALUES (?,?,?,?,'bad_nonce','session','10.0.0.9:5555','198.51.100.7','curl/8.4.0','oidc exchange failed',?)`,
		at, kind, outcome, a, at); err != nil {
		t.Fatalf("seed auth_event: %v", err)
	}
}

// TestExportAuthEventsIsNotAnAliasForConfigChanges is the LU-9 regression, and
// the absence assertion is the load-bearing half.
//
// Before the fix `authEvents` was a pure alias for `configChanges`: selecting it
// returned every config change and not one auth event. A test that only checked
// "the auth event is present" would have passed against that bug, because the
// alias also happened to include… nothing auth-shaped, while the checkbox
// silently produced a config-change report. An auditor asked for the login trail
// and would have received, and filed, the wrong table.
func TestExportAuthEventsIsNotAnAliasForConfigChanges(t *testing.T) {
	pool := openTestPool(t)
	seedAuditRows(t, pool) // includes a change_log row: actor alice, target 'session', details 'ok'
	seedAuthEvent(t, pool, "2026-01-01T05:00:00Z", "login-failed", "failure", "mallory@corp.example")

	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", []string{"authEvents"}, "json"); err != nil {
		t.Fatalf("ExportAudit authEvents: %v", err)
	}

	var got []AuditRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("authEvents export returned %d rows, want exactly 1: %+v", len(got), got)
	}
	r := got[0]
	if r.Source != "auth_events" {
		t.Errorf("source = %q, want auth_events", r.Source)
	}
	if r.Action != "login-failed" || r.Status != "failure" || r.Actor != "mallory@corp.example" {
		t.Errorf("auth event lost fields: %+v", r)
	}
	if !strings.Contains(r.Details, "reason=bad_nonce") ||
		!strings.Contains(r.Details, "clientIp=198.51.100.7") ||
		!strings.Contains(r.Details, "remoteAddr=10.0.0.9:5555") {
		t.Errorf("details = %q, want the folded address/reason context", r.Details)
	}

	// The absence half: not one change_log row may appear when only authEvents
	// was requested.
	if strings.Contains(buf.String(), `"change_log"`) {
		t.Errorf("selecting authEvents alone returned change_log rows — it is still an alias:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "alice") {
		t.Errorf("selecting authEvents alone leaked the config change's actor:\n%s", buf.String())
	}
}

// TestExportAuthEventNullActorRendersUnauthenticated covers the column that is
// deliberately nullable. The events that matter most — a failed login, a CSRF
// rejection — happen before an identity is known, and an empty cell in a CSV is
// ambiguous: it reads as "the export dropped the actor" rather than "there was
// no actor". An explicit marker is the difference between evidence and a gap.
func TestExportAuthEventNullActorRendersUnauthenticated(t *testing.T) {
	pool := openTestPool(t)
	seedAuthEvent(t, pool, "2026-01-01T06:00:00Z", "login-failed", "failure", "")

	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", []string{"authEvents"}, "json"); err != nil {
		t.Fatalf("ExportAudit: %v", err)
	}
	var got []AuditRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	if got[0].Actor != "(unauthenticated)" {
		t.Errorf("actor = %q for a NULL actor, want \"(unauthenticated)\" — an empty cell reads as a lost value", got[0].Actor)
	}

	// The CSV surface must carry it too: that is the format an auditor opens.
	buf.Reset()
	if err := ExportAudit(context.Background(), pool, &buf, "", "", []string{"authEvents"}, "csv"); err != nil {
		t.Fatalf("ExportAudit csv: %v", err)
	}
	if !strings.Contains(buf.String(), "(unauthenticated)") {
		t.Errorf("CSV export = %q, want the (unauthenticated) marker", buf.String())
	}
}

// TestExportAllIncludesAuthEvents pins the default. An export with no type
// filter is what "download everything" produces, and omitting the auth trail
// from it would mean the completeness an auditor assumes is quietly false.
func TestExportAllIncludesAuthEvents(t *testing.T) {
	pool := openTestPool(t)
	seedAuditRows(t, pool)
	seedAuthEvent(t, pool, "2026-01-01T05:00:00Z", "login", "success", "alice@corp.example")

	var buf bytes.Buffer
	if err := ExportAudit(context.Background(), pool, &buf, "", "", nil, "json"); err != nil {
		t.Fatalf("ExportAudit: %v", err)
	}
	var got []AuditRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, r := range got {
		if r.Source == "auth_events" {
			found = true
		}
	}
	if !found {
		t.Errorf("an unfiltered export omitted the auth trail: %+v", got)
	}
}
