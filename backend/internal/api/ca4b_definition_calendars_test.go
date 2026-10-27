package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 2.5.3 CA4b (CA-30 to CA-33): a job's and a workflow's own Forbid and Allow
// calendars, set in the composer (validated as an entry's), gate every
// automatic start: a service account's is refused, told why, and recorded;
// a person's Run is not gated, and the detail says what today is.

func todayUTC() string { return time.Now().UTC().Format("2006-01-02") }

func seedCalendarOf(t *testing.T, pool *sql.DB, agency, name string, global, record bool, days ...string) {
	t.Helper()
	b := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	if _, err := pool.Exec(`INSERT INTO calendars (source, name, global, record_suppressed, created_at, owner_agency) VALUES ('cronomicon', ?, ?, ?, 't', ?)`,
		name, b(global), b(record), agency); err != nil {
		t.Fatal(err)
	}
	for _, d := range days {
		if _, err := pool.Exec(`INSERT INTO calendar_days (calendar_owner, calendar_source, calendar_name, day, label) VALUES (?, 'cronomicon', ?, ?, 'L')`, agency, name, d); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCA4b_TheComposerTakesAJobsCalendarsAndTheDetailSaysWhatTodayIs(t *testing.T) {
	h, pool := composeRBACServer(t)
	seedCalendarOf(t, pool, "ag:FIN", "change-days", false, false, todayUTC())
	seedCalendarOf(t, pool, "ag:FIN", "freeze", true, false)
	seedCalendarOf(t, pool, "ag:TAX", "tax-only", false, false)

	post := func(name, cals string) *httptest.ResponseRecorder {
		return composeReq(t, h, http.MethodPost, "/api/v1/jobs", "fin-composers",
			`{"name":"`+name+`","scriptRef":"backup-db","scope":"fin-prod",`+cals+`}`)
	}
	for body, why := range map[string]string{
		`"forbidCalendars":["tax-only"]`:                                     "another agency's calendar",
		`"allowCalendars":["freeze"]`:                                        "a freeze calendar as an Allow",
		`"forbidCalendars":["change-days"],"allowCalendars":["change-days"]`: "a calendar in both",
	} {
		if rec := post("bad", body); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "forbidCalendars/allowCalendars") {
			t.Errorf("%s: %d %s, want 422 in the definition's words", why, rec.Code, rec.Body)
		}
	}
	if rec := post("patch", `"forbidCalendars":["change-days"]`); rec.Code != http.StatusCreated {
		t.Fatalf("a job with a Forbid calendar = %d (%s)", rec.Code, rec.Body)
	}
	rec := composeReq(t, h, http.MethodGet, "/api/v1/jobs/"+composeJobID(t, pool, "patch"), "fin-composers", "")
	var job struct {
		ForbidCalendars []string `json:"forbidCalendars"`
		CalendarToday   *struct {
			Polarity, Calendar, Reason string
		} `json:"calendarToday"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &job); err != nil {
		t.Fatal(err)
	}
	if strings.Join(job.ForbidCalendars, ",") != "change-days" || job.CalendarToday == nil ||
		job.CalendarToday.Calendar != "change-days" || job.CalendarToday.Polarity != "skip" {
		t.Errorf("the detail = %s, want the Forbid list and today forbidden by change-days", rec.Body)
	}
	// Bound by the job, the calendar is not deleted without force, and the
	// refusal names the job's own binding.
	if rec := composeReq(t, h, http.MethodDelete, "/api/v1/calendars/change-days?agencyId=ag:FIN", "fin-composers", ""); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "job:patch (its own Forbid)") {
		t.Errorf("deleting a calendar the job forbids by = %d %s", rec.Code, rec.Body)
	}
}

// triggerWithBody is triggerAs with a body.
func triggerWithBody(t *testing.T, ts *httptest.Server, token, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", ts.URL+path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestCA4b_AServiceAccountsStartTheCalendarsRefuseIsRecorded(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "export", 1)
	seedTriggerJob(t, pool, "sweep", 1)
	// An Allow calendar that does not list today, and does not opt into
	// recording: a dropped token start is recorded all the same.
	seedCalendarOf(t, pool, workAgency, "month-end", false, false, "2000-01-31")
	seedCalendarOf(t, pool, workAgency, "change-days", false, false, todayUTC(), time.Now().UTC().Add(48*time.Hour).Format("2006-01-02"))
	if _, err := pool.Exec(`UPDATE jobs SET uid = 'uid-'||name, allow_calendars = '["month-end"]' WHERE name = 'export'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`UPDATE jobs SET uid = 'uid-'||name, forbid_calendars = '["change-days"]' WHERE name = 'sweep'`); err != nil {
		t.Fatal(err)
	}
	_, token := mintAccount(t, ts, client, csrf, map[string]any{"name": "nagios", "role": "admin", "allScopes": true})

	resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/export")
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body.String(), "suppressed_by_calendar") || !strings.Contains(body.String(), "month-end") {
		t.Fatalf("a token start on a day the job does not allow = %d %s", resp.StatusCode, body.String())
	}
	var kind, by, byCal string
	if err := pool.QueryRow(`SELECT trigger_kind, triggered_by, suppressed_by_calendar FROM runs WHERE job_name = 'export' AND status = 'skipped'`).Scan(&kind, &by, &byCal); err != nil {
		t.Fatalf("the refused token start left no record: %v", err)
	}
	if kind != "webhook" || by != "svc:nagios" || byCal != "month-end" {
		t.Errorf("recorded %s by %s, calendar %s", kind, by, byCal)
	}
	var pending int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pending_runs`).Scan(&pending)
	if pending != 0 {
		t.Errorf("a refused start left %d pending rows", pending)
	}

	// Deferred to a day the job forbids: refused now, never parked.
	in2Days := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339)
	if time.Now().UTC().Add(48*time.Hour).Truncate(time.Hour).Add(time.Hour).Format("2006-01-02") != time.Now().UTC().Add(48*time.Hour).Format("2006-01-02") {
		in2Days = time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	}
	resp = triggerWithBody(t, ts, token, "/api/v1/trigger/jobs/sweep", `{"runAt":"`+in2Days+`"}`)
	body.Reset()
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body.String(), "change-days") {
		t.Errorf("a deferred token start on a forbidden day = %d %s, want 409", resp.StatusCode, body.String())
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pending_runs`).Scan(&pending)
	if pending != 0 {
		t.Errorf("the forbidden deferral was parked (%d rows)", pending)
	}

	// A person's Run is not gated by the job's calendars.
	var id int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name = 'sweep'`).Scan(&id)
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/jobs/"+itoa(id)+"/run", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusAccepted {
		t.Errorf("a person's Run on a day the job forbids = %d, want 202", r2.StatusCode)
	}
}

func TestCA4b_AServiceAccountsWorkflowStartIsGatedByTheWorkflowsCalendars(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "step", 1)
	if _, err := pool.Exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, owner_agency) VALUES ('wf-rel', 'release', 'cronomicon', '[{"type":"job","name":"step"}]', 1, 't', ?)`, workAgency); err != nil {
		t.Fatal(err)
	}
	seedCalendarOf(t, pool, workAgency, "change-days", false, false, todayUTC())
	if _, err := pool.Exec(`UPDATE workflows SET forbid_calendars = '["change-days"]' WHERE uid = 'wf-rel'`); err != nil {
		t.Fatal(err)
	}
	_, token := mintAccount(t, ts, client, csrf, map[string]any{"name": "ci", "role": "admin", "allScopes": true})
	resp := triggerAs(t, ts, token, "/api/v1/trigger/workflows/release")
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(body.String(), "the workflow's calendar") {
		t.Fatalf("a token start of a workflow its calendar forbids = %d %s", resp.StatusCode, body.String())
	}
	var kind string
	if err := pool.QueryRow(`SELECT trigger_kind FROM workflow_runs WHERE workflow_uid = 'wf-rel' AND status = 'skipped'`).Scan(&kind); err != nil || kind != "webhook" {
		t.Errorf("the refused workflow start = %q (%v), want a skipped webhook record", kind, err)
	}
}

func TestCA4b_UpcomingMarksTheDaysAJobForbids(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)
	seedTriggerJob(t, pool, "daily", 1)
	var days []string
	for i := 0; i < 9; i++ {
		days = append(days, time.Now().UTC().Add(time.Duration(i)*24*time.Hour).Format("2006-01-02"))
	}
	seedCalendarOf(t, pool, workAgency, "blackout", false, false, days...)
	if _, err := pool.Exec(`UPDATE jobs SET uid = 'uid-daily', forbid_calendars = '["blackout"]' WHERE name = 'daily'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES ('git', 'job', 'daily', 'noon', '0 12 * * *', 0, 'uid-daily')`); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(ts.URL + "/api/v1/schedules/upcoming")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, it := range out.Items {
		if it["ownerName"] != "daily" {
			continue
		}
		n++
		if it["suppressed"] != true || it["suppressedBy"] != "blackout" {
			t.Errorf("an upcoming fire on a forbidden day is not marked: %v", it)
		}
	}
	if n == 0 {
		t.Fatalf("Upcoming has no fires of the job: %v", out.Items)
	}
}
