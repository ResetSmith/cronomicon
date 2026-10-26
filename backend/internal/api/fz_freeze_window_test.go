package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// The installation freeze with a window (2.5.2 FZ-1 to FZ-6).

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestFZ_TheWindowIsCheckedSetAndRecorded(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	now := time.Now().UTC().Truncate(time.Second)
	put := func(body string) (int, map[string]any) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/settings/freeze", gRoot, body)
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	for _, bad := range []string{
		`{"on":true,"reason":"r","until":"` + rfc(now.Add(-time.Hour)) + `"}`,                                           // ends in the past
		`{"on":true,"reason":"r","from":"` + rfc(now.Add(2*time.Hour)) + `","until":"` + rfc(now.Add(time.Hour)) + `"}`, // until before from
		`{"on":true,"reason":"r","from":"tomorrow"}`,
	} {
		if code, _ := put(bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", bad, code)
		}
	}

	from, until := now.Add(time.Hour), now.Add(3*time.Hour)
	code, f := put(`{"on":true,"reason":"Year-end","from":"` + rfc(from) + `","until":"` + rfc(until) + `"}`)
	if code != http.StatusOK || f["state"] != calendar.FreezeScheduled || f["from"] != rfc(from) || f["until"] != rfc(until) {
		t.Fatalf("a scheduled window: %d %+v", code, f)
	}
	rec := gateReq(t, h, http.MethodGet, "/api/v1/settings/freeze", gFinViewer, "")
	var got map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got["state"] != calendar.FreezeScheduled {
		t.Errorf("anyone reads it as scheduled: %d %s", rec.Code, rec.Body)
	}
	last := func(action string) string {
		t.Helper()
		var s string
		_ = pool.QueryRow(`SELECT details FROM change_log WHERE category = 'Calendars' AND action = ? ORDER BY rowid DESC LIMIT 1`, action).Scan(&s)
		return s
	}
	if d := last("Frozen"); !strings.Contains(d, "from "+rfc(from)+" until "+rfc(until)) {
		t.Errorf("the Frozen record %q does not name the window", d)
	}
	// Lifting a freeze that has not begun cancels it, and says so.
	if code, f = put(`{"on":false}`); code != http.StatusOK || f["state"] != calendar.FreezeOff || f["from"] != nil {
		t.Errorf("lift: %d %+v", code, f)
	}
	if d := last("Thawed"); !strings.Contains(d, "cancelled") {
		t.Errorf("cancelling a scheduled freeze recorded %q", d)
	}
	// The open switch is unchanged: no window, in effect now; lifting it is a thaw.
	if code, f = put(`{"on":true,"reason":"Now"}`); code != http.StatusOK || f["state"] != calendar.FreezeInEffect {
		t.Errorf("an open freeze: %d %+v", code, f)
	}
	put(`{"on":false}`)
	if d := last("Thawed"); d != "The installation is no longer frozen" {
		t.Errorf("lifting an open freeze recorded %q", d)
	}
}

func TestFZ_TheUpcomingListMarksOnlyTheFiresInsideTheWindow(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO jobs(name, run_type, command, content_hash, enabled, synced_at)
		 VALUES('patch','bash','echo hi','sha256:x',1,'2026-01-01T00:00:00Z')`,
		`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
		 VALUES('git','job','patch','every-minute','30 * * * * *',0)`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Whole seconds, as the window is stored: a fire in the same second as from
	// must be judged by the test as the server judges it.
	now := time.Now().UTC().Truncate(time.Second)
	from, until := now.Add(10*time.Minute), now.Add(20*time.Minute)
	raw, _ := json.Marshal(calendar.Freeze{On: true, Reason: "window", From: rfc(from), Until: rfc(until)})
	if _, err := pool.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)`, calendar.FreezeKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	var out struct {
		Items []struct {
			At           string `json:"at"`
			Suppressed   bool   `json:"suppressed"`
			SuppressedBy string `json:"suppressedBy"`
		} `json:"items"`
	}
	getJSON(t, api.client, api.ts+"/api/v1/schedules/upcoming", &out)
	inside, outside := 0, 0
	for _, it := range out.Items {
		at, err := time.Parse(time.RFC3339, it.At)
		if err != nil {
			t.Fatalf("at %q: %v", it.At, err)
		}
		in := !at.Before(from) && at.Before(until)
		frozen := it.Suppressed && it.SuppressedBy == calendar.FreezeCalendar
		if in != frozen {
			t.Errorf("a fire at %s (inside the window: %v) frozen=%v", it.At, in, frozen)
		}
		if in {
			inside++
		} else {
			outside++
		}
	}
	if inside == 0 || outside == 0 {
		t.Errorf("precondition: fires inside (%d) and outside (%d) the window", inside, outside)
	}
}

func TestFZ_ARunIsRefusedOnlyWhileTheWindowIsInEffect(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	workInAgency(t, pool, client)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "billing")
	setWindow := func(from, until time.Time) {
		t.Helper()
		f := calendar.Freeze{On: true, Reason: "window"}
		if !from.IsZero() {
			f.From = rfc(from)
		}
		if !until.IsZero() {
			f.Until = rfc(until)
		}
		raw, _ := json.Marshal(f)
		if _, err := pool.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, calendar.FreezeKey, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	run := func() int {
		t.Helper()
		code, _ := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs/"+itoa(jobID)+"/run", csrf, map[string]any{})
		return code
	}
	now := time.Now()
	setWindow(now.Add(time.Hour), now.Add(2*time.Hour)) // not begun
	if code := run(); code >= 300 {
		t.Errorf("a run before the window = %d, want it to start", code)
	}
	setWindow(now.Add(-time.Hour), now.Add(time.Hour)) // in effect
	if code := run(); code < 400 {
		t.Errorf("a run inside the window = %d, want a refusal", code)
	}
	setWindow(now.Add(-2*time.Hour), now.Add(-time.Hour)) // ended (written directly: PUT refuses a past window)
	if code := run(); code >= 300 {
		t.Errorf("a run after the window = %d, want it to start", code)
	}
}
