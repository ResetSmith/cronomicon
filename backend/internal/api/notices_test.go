package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// LR-85 — one inbox. A notice belongs to an agency and is read and dismissed by
// whoever administers that agency: an agency's own, Global's by a global
// administrator, who therefore sees them all.

type noticeRow struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	Subject    string `json:"subject"`
	Detail     string `json:"detail"`
}

func listNotices(t *testing.T, h http.Handler, who string) map[string]noticeRow {
	t.Helper()
	rec := gateReq(t, h, http.MethodGet, "/api/v1/notices", who, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s listing notices = %d (%s)", who, rec.Code, rec.Body)
	}
	var rows []noticeRow
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	out := map[string]noticeRow{}
	for _, r := range rows {
		out[r.Kind+"/"+r.Subject] = r
	}
	return out
}

func TestNotices_AreReadAndDismissedByTheirAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	ctx := context.Background()

	// One of Global's (a row in no agency, found by the check when the inbox is
	// opened), one of FIN's and one of TAX's.
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at) VALUES ('r-lost','lost-runner','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r-lost'`)
	for agency, subject := range map[string]string{"ag:FIN": "fin-thing", "ag:TAX": "tax-thing"} {
		if err := notices.Upsert(ctx, pool, "test_kind", notices.Finding{AgencyID: agency, Subject: subject, Detail: "about " + subject}); err != nil {
			t.Fatal(err)
		}
	}

	root := listNotices(t, h, gRoot)
	if len(root) != 3 {
		t.Fatalf("a global administrator sees %d notices, want all 3: %v", len(root), root)
	}
	orphan := root["orphaned/runner:r-lost"]
	if orphan.AgencyID != "global" || orphan.AgencyName != "Global" {
		t.Errorf("the orphaned runner's notice = %+v, want Global's, found by the check on opening the inbox", orphan)
	}
	if root["test_kind/fin-thing"].AgencyName != "FIN" {
		t.Errorf("FIN's notice does not carry its agency's name: %+v", root["test_kind/fin-thing"])
	}

	fin := listNotices(t, h, gFinAdmin)
	if len(fin) != 1 || fin["test_kind/fin-thing"].ID == "" {
		t.Fatalf("FIN's administrator sees %v, want FIN's notice alone", fin)
	}
	// Reach is not authority: neither a FIN viewer, nor a viewer of every scope,
	// nor that viewer who also administers FIN, sees Global's or TAX's.
	if got := listNotices(t, h, gFinViewer); len(got) != 0 {
		t.Errorf("a FIN viewer sees %v, want nothing", got)
	}
	if got := listNotices(t, h, gAllViewer); len(got) != 0 {
		t.Errorf("an all-scopes viewer sees %v, want nothing", got)
	}
	if got := listNotices(t, h, gMixed); len(got) != 1 || got["test_kind/fin-thing"].ID == "" {
		t.Errorf("an all-scopes viewer who administers FIN sees %v, want FIN's alone", got)
	}

	dismiss := func(who string, ids ...string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ids": ids})
		return gateReq(t, h, http.MethodPost, "/api/v1/notices/dismiss", who, string(body)).Code
	}
	finID, taxID, orphanID := root["test_kind/fin-thing"].ID, root["test_kind/tax-thing"].ID, orphan.ID

	// Another agency's, Global's, and a list that mixes one's own with another's:
	// refused whole, nothing dismissed.
	if code := dismiss(gFinAdmin, taxID); code != http.StatusForbidden {
		t.Errorf("FIN dismissing TAX's notice = %d, want 403", code)
	}
	if code := dismiss(gFinAdmin, orphanID); code != http.StatusForbidden {
		t.Errorf("FIN dismissing Global's notice = %d, want 403", code)
	}
	if code := dismiss(gFinAdmin, finID, taxID); code != http.StatusForbidden {
		t.Errorf("FIN dismissing its own with TAX's = %d, want 403", code)
	}
	if code := dismiss(gFinViewer, finID); code != http.StatusForbidden {
		t.Errorf("a FIN viewer dismissing FIN's notice = %d, want 403", code)
	}
	if got := listNotices(t, h, gRoot); len(got) != 3 {
		t.Fatalf("a refused dismissal hid something: %v", got)
	}

	// Its own, with an id that matches nothing beside it.
	if code := dismiss(gFinAdmin, finID, "no-such-notice"); code != http.StatusOK {
		t.Errorf("FIN dismissing its own notice = %d, want 200", code)
	}
	if got := listNotices(t, h, gFinAdmin); len(got) != 0 {
		t.Errorf("FIN still sees %v after dismissing", got)
	}
	var by string
	if err := pool.QueryRow(`SELECT dismissed_by FROM notices WHERE id = ?`, finID).Scan(&by); err != nil || by == "" {
		t.Errorf("the dismissal does not record who: %q (%v)", by, err)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM change_log WHERE category = 'Notices' AND action = 'Dismissed'`); n != 1 {
		t.Errorf("%d change-log rows for one dismissal", n)
	}
	if code := dismiss(gRoot, taxID, orphanID); code != http.StatusOK {
		t.Errorf("a global administrator dismissing TAX's and Global's = %d, want 200", code)
	}
	if got := listNotices(t, h, gRoot); len(got) != 0 {
		t.Errorf("open after everything was dismissed: %v", got)
	}
}

// The runner-tag pins of 2.2.0 are in the inbox too, for whoever administers
// the pin's scope, and are dismissed through it.
func TestNotices_IncludeRetiredRunnerPins(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO retired_runner_pins (job_uid, job_name, job_source, scope, runner_tag, reason, recorded_at) VALUES
	      ('u1', 'fin-nightly', 'git', 'fin-hosts', 'dmz', 'no_eligible_runner', '2026-10-01T00:00:00Z'),
	      ('u2', 'tax-nightly', 'git', 'tax-hosts', 'dmz', 'mixed_pins', '2026-10-01T00:00:00Z'),
	      ('u3', 'platform-job', 'git', '', 'dmz', 'no_scope', '2026-10-01T00:00:00Z')`)

	root := listNotices(t, h, gRoot)
	if len(root) != 3 {
		t.Fatalf("a global administrator sees %v, want the three pins", root)
	}
	finPin := root["retired_runner_pin/fin-nightly"]
	if finPin.AgencyID != "ag:FIN" || finPin.ID[:4] != "pin:" {
		t.Errorf("FIN's pin = %+v, want filed under FIN with a pin: id", finPin)
	}
	if root["retired_runner_pin/platform-job"].AgencyID != "global" {
		t.Errorf("a pin on a job with no scope is filed under %q, want global", root["retired_runner_pin/platform-job"].AgencyID)
	}
	fin := listNotices(t, h, gFinAdmin)
	if len(fin) != 1 || fin["retired_runner_pin/fin-nightly"].ID == "" {
		t.Fatalf("FIN's administrator sees %v, want FIN's pin alone", fin)
	}

	body := func(ids ...string) string {
		b, _ := json.Marshal(map[string]any{"ids": ids})
		return string(b)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/notices/dismiss", gFinAdmin, body(root["retired_runner_pin/tax-nightly"].ID)); rec.Code != http.StatusForbidden {
		t.Errorf("FIN dismissing TAX's pin = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/notices/dismiss", gFinAdmin, body(root["retired_runner_pin/platform-job"].ID)); rec.Code != http.StatusForbidden {
		t.Errorf("FIN dismissing a pin with no scope = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/notices/dismiss", gFinAdmin, body(finPin.ID)); rec.Code != http.StatusOK {
		t.Errorf("FIN dismissing its own pin = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM retired_runner_pins WHERE dismissed_at IS NOT NULL`); n != 1 {
		t.Errorf("%d pins dismissed, want FIN's one", n)
	}
	if got := listNotices(t, h, gRoot); len(got) != 2 {
		t.Errorf("open after FIN dismissed its pin: %v", got)
	}
}

// listNoticesFresh is listNotices for a test that changes the world between
// reads: the inbox runs its checks at most once in 30 seconds, so the checks
// are run here first, as the next due refresh would.
func listNoticesFresh(t *testing.T, h http.Handler, pool *sql.DB, who string) map[string]noticeRow {
	t.Helper()
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatalf("notices checks: %v", err)
	}
	return listNotices(t, h, who)
}
