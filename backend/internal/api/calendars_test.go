package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// CAL-22 / CAL-30 / CAL-31 — the Phase 1B authoring and audit surfaces.
//
// The theme running through these: every refusal here exists because the
// alternative fails SILENTLY. A typo'd calendar name stops suppressing without
// telling anyone; a global calendar bound as a run-day list produces an entry
// that can never fire; an audit trail keyed on message text breaks on a copy
// edit. Each is cheap to catch at authoring time and expensive to notice later.

type calAPI struct {
	t      *testing.T
	ts     string
	client *http.Client
	csrf   string
}

func (c calAPI) do(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.ts+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func newCalAPI(t *testing.T) (calAPI, *sql.DB) {
	t.Helper()
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	return calAPI{t: t, ts: ts.URL, client: client, csrf: csrf}, pool
}

// today is the current wall-clock day IN THE APP ZONE, which for a test server
// is UTC: GetGlobalSettings defaults the timezone setting to "UTC" when no row
// exists, so ResolveEffectiveTimezone never reaches its time.Local fallback.
//
// It was time.Now().Format(...) — the MACHINE-LOCAL date — which made every
// test seeding "today" fail whenever the local date and the UTC date disagreed:
// on this machine, every day from 17:00 to midnight PDT; on any machine east of
// UTC, every morning. The projection evaluates each instant's day in the app
// zone, exactly as fire() does, so a day seeded in any other zone is a day that
// sometimes is not today.
//
// Deterministic repro of the original bug: TZ=Pacific/Kiritimati go test -run
// TestUpcomingAnnotatesSuppressedInstants (Kiritimati's local date is ahead of
// UTC for half of every UTC day).
func today() string { return time.Now().UTC().Format("2006-01-02") }

// tomorrow is today's UTC successor. Tests that assert against PROJECTED
// instants must seed both: near UTC midnight the next projected instant already
// falls on the next day, so seeding today alone leaves a nightly one-minute
// window where nothing matches — a flake with a 1-in-1440 duty cycle, which is
// the worst kind, because it passes every re-run that investigates it.
func tomorrow() string { return time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02") }

// ── CAL-4: CRUD, validation, coverage ──────────────────────────────────────

func TestCalendarCRUDAndDayValidation(t *testing.T) {
	api, _ := newCalAPI(t)

	code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name":        "federal-holidays",
		"description": "US federal holidays, observed",
		"days": []map[string]string{
			{"day": "2026-07-03", "label": "Independence Day (observed)"},
			{"day": "2026-01-01", "label": "New Year's Day"},
			{"day": "2026-01-01", "label": "duplicate, collapses"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d, want 201", code)
	}

	code, got := api.do(http.MethodGet, "/api/v1/calendars/federal-holidays", nil)
	if code != http.StatusOK {
		t.Fatalf("get = %d, want 200", code)
	}
	days, _ := got["days"].([]any)
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2 (the repeated date is the same fact twice, not an error)", len(days))
	}
	// Stored sorted, so lastDay is meaningful and the UI needs no client-side sort.
	first := days[0].(map[string]any)["day"]
	if first != "2026-01-01" {
		t.Errorf("days[0] = %v, want the earliest date — days are stored sorted", first)
	}

	// A date that looks like one but is not.
	code, body := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "bad", "days": []map[string]string{{"day": "2026-02-30"}},
	})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("create with 2026-02-30 = %d, want 422 (Feb 30 is not a real day)", code)
	}
	if msg, _ := body["message"].(string); msg == "" {
		t.Error("422 carried no message")
	}

	// Duplicate name.
	code, _ = api.do(http.MethodPost, "/api/v1/calendars", map[string]any{"name": "federal-holidays"})
	if code != http.StatusConflict {
		t.Errorf("duplicate create = %d, want 409", code)
	}

	// Bulk day replacement is wholesale.
	code, got = api.do(http.MethodPut, "/api/v1/calendars/federal-holidays/days", map[string]any{
		"days": []map[string]string{{"day": "2027-01-01", "label": "New Year's Day"}},
	})
	if code != http.StatusOK {
		t.Fatalf("replace days = %d, want 200", code)
	}
	if n, _ := got["dayCount"].(float64); n != 1 {
		t.Errorf("dayCount after replace = %v, want 1 (replacement is wholesale)", n)
	}

	code, _ = api.do(http.MethodDelete, "/api/v1/calendars/federal-holidays", nil)
	if code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", code)
	}
	code, _ = api.do(http.MethodGet, "/api/v1/calendars/federal-holidays", nil)
	if code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", code)
	}
}

// CAL-30/CAL-16 — an expired calendar is detectable from the API alone. With no
// shipped holiday content, an unrenewed calendar is this feature's likeliest
// failure and nothing else notices it.
func TestCalendarCoverageReportsExpiry(t *testing.T) {
	api, _ := newCalAPI(t)

	past := time.Now().AddDate(0, 0, -10).Format("2006-01-02")
	future := time.Now().AddDate(0, 0, 200).Format("2006-01-02")
	mustCreate := func(name, day string) {
		if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
			"name": name, "days": []map[string]string{{"day": day}},
		}); code != http.StatusCreated {
			t.Fatalf("create %s = %d", name, code)
		}
	}
	mustCreate("expired", past)
	mustCreate("healthy", future)
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{"name": "empty"}); code != http.StatusCreated {
		t.Fatal("create empty calendar failed")
	}

	code, body := api.do(http.MethodGet, "/api/v1/calendars", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200", code)
	}
	if thr, _ := body["expiryWarningDays"].(float64); thr <= 0 {
		t.Errorf("expiryWarningDays = %v, want a positive threshold shipped with the data", thr)
	}
	byName := map[string]map[string]any{}
	for _, it := range body["items"].([]any) {
		m := it.(map[string]any)
		byName[m["name"].(string)] = m
	}

	exp := byName["expired"]
	rem, ok := exp["daysRemaining"].(float64)
	if !ok || rem >= 0 {
		t.Errorf("expired calendar daysRemaining = %v, want negative — this is the state that silently stops suppressing", exp["daysRemaining"])
	}
	if exp["lastDay"] != past {
		t.Errorf("expired lastDay = %v, want %s", exp["lastDay"], past)
	}

	if rem, _ := byName["healthy"]["daysRemaining"].(float64); rem < 60 {
		t.Errorf("healthy calendar daysRemaining = %v, want well beyond the warning threshold", rem)
	}
	if byName["empty"]["lastDay"] != nil {
		t.Errorf("empty calendar lastDay = %v, want null", byName["empty"]["lastDay"])
	}
}

// ── CAL-22: binding validation through the compose path ────────────────────

func TestComposeRejectsBadCalendarBindings(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, content_hash, synced_at)
		VALUES('s1','bash','echo hi','sha256:x','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "holidays", "days": []map[string]string{{"day": "2026-07-03"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "freeze", "global": true, "days": []map[string]string{{"day": "2026-08-15"}},
	}); code != http.StatusCreated {
		t.Fatal("seed global calendar failed")
	}

	composeJob := func(name string, entry map[string]any) (int, map[string]any) {
		return api.do(http.MethodPost, "/api/v1/jobs", map[string]any{
			"name": name, "scriptRef": "s1", "scope": "", "schedules": []map[string]any{entry},
		})
	}

	// Unknown name — the typo that would otherwise silently stop suppressing.
	code, body := composeJob("j-unknown", map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *", "skipCalendars": []string{"holidayz"},
	})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("unknown calendar = %d, want 422", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "holidayz") {
		t.Errorf("422 message %q should name the offending calendar", msg)
	}

	// Same name in both roles — incoherent, not merely redundant.
	code, _ = composeJob("j-both", map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *",
		"skipCalendars": []string{"holidays"}, "onlyCalendars": []string{"holidays"},
	})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("calendar in both roles = %d, want 422", code)
	}

	// CAL-31 — a global calendar as a run-day list: an entry that can never fire.
	code, body = composeJob("j-globalonly", map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *", "onlyCalendars": []string{"freeze"},
	})
	if code != http.StatusUnprocessableEntity {
		t.Errorf("global calendar in only role = %d, want 422", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "global") {
		t.Errorf("422 message %q should explain the global/only incompatibility", msg)
	}

	// The valid binding persists and copies down to the runtime row.
	code, _ = composeJob("j-ok", map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *", "skipCalendars": []string{"holidays"},
	})
	if code != http.StatusCreated {
		t.Fatalf("valid binding = %d, want 201", code)
	}
	var stored sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT skip_calendars FROM definition_schedules WHERE owner_name='j-ok' AND name='nightly'`).Scan(&stored); err != nil {
		t.Fatalf("read runtime binding: %v", err)
	}
	if stored.String != `["holidays"]` {
		t.Errorf("stored skip_calendars = %q, want [\"holidays\"] on the runtime row", stored.String)
	}
}

// CAL-31's other refusal: a calendar already bound as a run-day list cannot be
// promoted to global. The refusals matter more than the suppression — a global
// only-calendar is a system-wide outage behind one checkbox.
func TestCalendarCannotBecomeGlobalWhileBoundAsOnly(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "fiscal-close", "days": []map[string]string{{"day": "2026-09-30"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, only_calendars)
		VALUES('git','job','sweep','nightly','0 0 2 * * *',0,'["fiscal-close"]')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	code, body := api.do(http.MethodPut, "/api/v1/calendars/fiscal-close", map[string]any{"global": true})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("promote bound-as-only calendar to global = %d, want 422", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "fiscal-close") {
		t.Errorf("422 message %q should name the calendar and the binding", msg)
	}

	// Without the only-binding it promotes fine.
	if _, err := pool.ExecContext(ctx, `UPDATE definition_schedules SET only_calendars=NULL`); err != nil {
		t.Fatal(err)
	}
	if code, _ := api.do(http.MethodPut, "/api/v1/calendars/fiscal-close", map[string]any{"global": true}); code != http.StatusOK {
		t.Errorf("promote unbound calendar = %d, want 200", code)
	}
}

// CAL-22 — deleting a bound calendar 409s; ?force=true is the explicit override,
// and it leaves the binding dangling by design.
func TestDeleteBoundCalendarRequiresForce(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "holidays", "days": []map[string]string{{"day": "2026-07-03"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, skip_calendars)
		VALUES('git','job','patch','nightly','0 0 2 * * *',0,'["holidays"]')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	code, body := api.do(http.MethodDelete, "/api/v1/calendars/holidays", nil)
	if code != http.StatusConflict {
		t.Fatalf("delete bound calendar = %d, want 409", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "patch") {
		t.Errorf("409 message %q should name the binding that blocks the delete", msg)
	}

	if code, _ := api.do(http.MethodDelete, "/api/v1/calendars/holidays?force=true", nil); code != http.StatusNoContent {
		t.Errorf("forced delete = %d, want 204", code)
	}
	// The binding survives as a dangling name — §2.2 defines its behaviour, and
	// leaving it is what makes force honest rather than silently rewriting jobs.
	var raw sql.NullString
	_ = pool.QueryRowContext(ctx, `SELECT skip_calendars FROM definition_schedules WHERE owner_name='patch'`).Scan(&raw)
	if raw.String != `["holidays"]` {
		t.Errorf("binding after forced delete = %q, want it left dangling", raw.String)
	}
}

// FX2-C1 — a calendar whose only referrer sits in the RECYCLE BIN still 409s.
// The delete is hard (no bin, no tombstone) while the binned owner is fully
// recoverable: silently deleting the calendar leaves the restored job's entry
// naming a calendar that no longer exists, which evaluates as no suppression —
// the job fires on every holiday it was configured to skip, with no signal.
// FX2-Q1 split the old blanket ruling (FX-Q9) over exactly this asymmetry.
func TestDeleteCalendarBlocksOnBinnedReferrers(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "holidays", "days": []map[string]string{{"day": "2026-07-03"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	// The owner exists and is BINNED; its entry names the calendar (one of each
	// polarity, since a dangling `only` is the silent half of §2.2).
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs(name, source, run_type, command, content_hash, enabled, synced_at, deleted_at)
		VALUES('patch','git','bash','true','h',1,'2026-08-12T00:00:00Z','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("seed binned job: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, skip_calendars)
		VALUES('git','job','patch','nightly','0 0 2 * * *',0,'["holidays"]')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	code, body := api.do(http.MethodDelete, "/api/v1/calendars/holidays", nil)
	if code != http.StatusConflict {
		t.Fatalf("delete calendar with a binned referrer = %d, want 409 — the guard counted "+
			"only live owners, so binning a job silently unlocked hard-deleting its holiday "+
			"protection", code)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "patch") {
		t.Errorf("409 message %q should name the binned referrer", msg)
	}
	if !strings.Contains(msg, "in recycle bin") {
		t.Errorf("409 message %q should annotate the referrer as binned, so the operator "+
			"knows force detaches something dormant rather than live", msg)
	}

	// Force remains the informed override.
	if code, _ := api.do(http.MethodDelete, "/api/v1/calendars/holidays?force=true", nil); code != http.StatusNoContent {
		t.Errorf("forced delete = %d, want 204", code)
	}

	// And the READ surfaces still exclude the binned owner's binding (FX-A4):
	// the guard sees more than the pages do, deliberately.
	if code, cal := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "fiscal", "days": []map[string]string{{"day": "2026-09-30"}},
	}); code != http.StatusCreated {
		t.Fatalf("seed second calendar: %v", cal)
	}
	if _, err := pool.ExecContext(ctx, `
		UPDATE definition_schedules SET skip_calendars='["fiscal"]' WHERE owner_name='patch'`); err != nil {
		t.Fatal(err)
	}
	_, got := api.do(http.MethodGet, "/api/v1/calendars/fiscal", nil)
	if usedBy, ok := got["usedBy"].([]any); ok && len(usedBy) > 0 {
		t.Errorf("usedBy lists %d binding(s) for a binned owner — the list/get surfaces must "+
			"keep the FX-A4 live-only filter", len(usedBy))
	}
}

// FX2-C1 — the CAL-27 make-global guard counts binned referrers too, for the
// same reason the delete guard does. `global` + `only` on one entry is
// unfireable BY CONSTRUCTION (a global calendar is unioned into every entry's
// skip set, so its days are vetoed and every other day is suppressed by the
// only-rule). Judging on live bindings alone let an operator reach that state
// by binning the sole referrer first — and the job's silence only began on its
// restore, with nothing connecting the two events.
func TestCalendarCannotBecomeGlobalWhileABinnedOwnerBindsItAsOnly(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "fiscal-close", "days": []map[string]string{{"day": "2026-09-30"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs(name, source, run_type, command, content_hash, enabled, synced_at, deleted_at)
		VALUES('sweep','git','bash','true','h',1,'2026-08-12T00:00:00Z','2026-08-12T00:00:00Z')`); err != nil {
		t.Fatalf("seed binned job: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, only_calendars)
		VALUES('git','job','sweep','nightly','0 0 2 * * *',0,'["fiscal-close"]')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	code, body := api.do(http.MethodPut, "/api/v1/calendars/fiscal-close", map[string]any{"global": true})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("promote to global with a binned only-binding = %d, want 422 — binning the "+
			"sole referrer must not unlock a state that silences the job forever on restore", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "in recycle bin") {
		t.Errorf("422 message %q should annotate the referrer as binned so the refusal is actionable", msg)
	}
}

// ── CAL-22: the no-drift guarantee ─────────────────────────────────────────

// A schedule with NO calendar bindings must hash byte-identically to what it
// hashed before the feature existed. If this breaks, the first sync after deploy
// reports the entire catalogue as modified.
func TestContentHashUnchangedWithoutBindings(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()

	// The digest a plain cron schedule produced BEFORE this feature existed,
	// pinned as a literal rather than recomputed: a recomputation would drift
	// along with the code it is supposed to fence, which is exactly the failure
	// mode (a silently-changed hash reports every pre-existing row as modified on
	// the first sync after deploy).
	const wantHash = "sha256:08e61c0459c6b42a23caf850960bc536f2df917f281c38615fea55f30f1089ee"

	if code, _ := api.do(http.MethodPost, "/api/v1/schedule-defs", map[string]any{
		"name": "plain", "cron": "0 0 2 * * *",
	}); code != http.StatusCreated {
		t.Fatal("create plain schedule failed")
	}
	var plainHash string
	if err := pool.QueryRowContext(ctx, `SELECT content_hash FROM schedules WHERE name='plain'`).Scan(&plainHash); err != nil {
		t.Fatalf("read hash: %v", err)
	}
	if plainHash != wantHash {
		t.Fatalf("unbound schedule hash = %q, want the pre-feature digest %q — every existing row's stored hash just changed", plainHash, wantHash)
	}

	// Binding a calendar MUST change the digest (it is part of the firing rule)...
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "holidays", "days": []map[string]string{{"day": "2026-07-03"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if code, _ := api.do(http.MethodPost, "/api/v1/schedule-defs", map[string]any{
		"name": "bound", "cron": "0 0 2 * * *", "skipCalendars": []string{"holidays"},
	}); code != http.StatusCreated {
		t.Fatal("create bound schedule failed")
	}
	var boundHash string
	_ = pool.QueryRowContext(ctx, `SELECT content_hash FROM schedules WHERE name='bound'`).Scan(&boundHash)
	if boundHash == plainHash {
		t.Error("binding a calendar did not change the content hash — the binding is part of the firing rule")
	}

	// ...and removing it must return the digest to exactly the unbound value.
	if code, _ := api.do(http.MethodPut, "/api/v1/schedule-defs/bound", map[string]any{
		"cron": "0 0 2 * * *",
	}); code != http.StatusOK {
		t.Fatal("unbind failed")
	}
	var rebound string
	_ = pool.QueryRowContext(ctx, `SELECT content_hash FROM schedules WHERE name='bound'`).Scan(&rebound)
	if rebound != plainHash {
		t.Errorf("unbound hash = %q, want it back to the plain digest %q — the segment must be conditional", rebound, plainHash)
	}
}

// ── CAL-10: the projection annotates rather than drops ─────────────────────

func TestUpcomingAnnotatesSuppressedInstants(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()

	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "holidays",
		// Both days, not just today: the per-minute cron's next instants straddle
		// UTC midnight for the last minute of every day, and this test asserts on
		// whichever instants the projection returns.
		"days": []map[string]string{{"day": today(), "label": "Test Holiday"}, {"day": tomorrow(), "label": "Test Holiday"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs(name, run_type, command, content_hash, enabled, synced_at)
		VALUES('patch','bash','echo hi','sha256:x',1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// Fires every minute, so the 24h window certainly contains instants today.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, skip_calendars)
		VALUES('git','job','patch','everyminute','0 * * * * *',0,'["holidays"]')`); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	var body struct {
		Items []struct {
			ScheduleName    string `json:"scheduleName"`
			At              string `json:"at"`
			Suppressed      bool   `json:"suppressed"`
			SuppressedBy    string `json:"suppressedBy"`
			SuppressedLabel string `json:"suppressedLabel"`
		} `json:"items"`
	}
	getJSON(t, api.client, api.ts+"/api/v1/schedules/upcoming", &body)
	if len(body.Items) == 0 {
		t.Fatal("upcoming returned no items — suppressed instants must be ANNOTATED, not dropped")
	}
	var annotated int
	for _, it := range body.Items {
		if it.Suppressed {
			annotated++
			if it.SuppressedBy != "holidays" {
				t.Errorf("suppressedBy = %q, want holidays", it.SuppressedBy)
			}
			if it.SuppressedLabel != "Test Holiday" {
				t.Errorf("suppressedLabel = %q, want the day's label", it.SuppressedLabel)
			}
		}
	}
	if annotated == 0 {
		t.Error("no instant was annotated as suppressed — the projection and the engine would disagree")
	}
}

// ── CAL-30: the audit trail is retrievable ─────────────────────────────────

// The point of CAL-8/CAL-32's rows is that somebody can find them. The filter
// matches the structured column, so a reword of the message cannot break it —
// this test deliberately stores a reason text that shares NO words with the
// query.
func TestHistoryFilterByCalendarUsesStructuredProvenance(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()

	seedRun := func(id, job, cal, reason string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO runs(id, job_name, job_source, run_type, status, queued_reason,
			                 triggered_by, trigger_kind, schedule_name, suppressed_by_calendar, created_at)
			VALUES(?,?,'git','bash','skipped',?,'scheduler','scheduled','nightly',?,?)`,
			id, job, reason, nullOrEmpty(cal), time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	seedRun("r1", "patch", "federal-holidays", "totally reworded message")
	seedRun("r2", "patch", "change-freeze", "another wording entirely")
	seedRun("r3", "patch", "", "Skipped: a run for this job is already active (Forbid policy)")

	count := func(query string) int {
		var body struct {
			Items []struct {
				TraceID              string `json:"traceId"`
				SuppressedByCalendar string `json:"suppressedByCalendar"`
			} `json:"items"`
		}
		getJSON(t, api.client, api.ts+"/api/v1/runs"+query, &body)
		return len(body.Items)
	}

	if n := count("?calendar=federal-holidays"); n != 1 {
		t.Errorf("?calendar=federal-holidays returned %d, want 1 — matched on the column, not the reason text", n)
	}
	if n := count("?calendar=*"); n != 2 {
		t.Errorf("?calendar=* returned %d, want 2 (both calendar suppressions, not the Forbid skip)", n)
	}
	if n := count(""); n != 3 {
		t.Errorf("unfiltered returned %d, want 3", n)
	}

	var body struct {
		Items []struct {
			TraceID              string `json:"traceId"`
			SuppressedByCalendar string `json:"suppressedByCalendar"`
		} `json:"items"`
	}
	getJSON(t, api.client, api.ts+"/api/v1/runs?calendar=federal-holidays", &body)
	if body.Items[0].SuppressedByCalendar != "federal-holidays" {
		t.Errorf("item suppressedByCalendar = %q, want it surfaced so the result explains itself", body.Items[0].SuppressedByCalendar)
	}
}

// The workflow half — new rows entirely, since a suppressed workflow fire
// previously left nothing behind.
func TestWorkflowHistoryFilterByCalendar(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()

	for i, c := range []string{"federal-holidays", ""} {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, queued_reason,
			                          triggered_by, trigger_kind, schedule_name, suppressed_by_calendar, created_at)
			VALUES(?,?,'git',?,?,'scheduler','scheduled','nightly',?,?)`,
			fmt.Sprintf("wr%d", i), "release",
			map[bool]string{true: "skipped", false: "success"}[c != ""],
			"reason text", nullOrEmpty(c), time.Now().UTC().Format(time.RFC3339)); err != nil {
			t.Fatalf("seed workflow run: %v", err)
		}
	}

	var body struct {
		Items []struct {
			TraceID              string `json:"traceId"`
			Status               string `json:"status"`
			SuppressedByCalendar string `json:"suppressedByCalendar"`
			StatusReason         string `json:"statusReason"`
		} `json:"items"`
	}
	getJSON(t, api.client, api.ts+"/api/v1/workflow-runs?calendar=federal-holidays", &body)
	if len(body.Items) != 1 {
		t.Fatalf("workflow calendar filter returned %d, want 1", len(body.Items))
	}
	if body.Items[0].SuppressedByCalendar != "federal-holidays" {
		t.Errorf("suppressedByCalendar = %q", body.Items[0].SuppressedByCalendar)
	}
	if body.Items[0].StatusReason == "" {
		t.Error("statusReason empty — a suppressed workflow fire carries nothing else to explain itself")
	}
}

// ── CAL-12: the roll-up ────────────────────────────────────────────────────

// "Does job X run on holidays?" must have a single-place answer, and the
// DISAGREEMENT case is the one worth surfacing loudly.
func TestInventoryCalendarRollup(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs(name, run_type, command, content_hash, enabled, synced_at)
		VALUES('patch','bash','echo hi','sha256:x',1,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// Two of three entries skip holidays — the mixed case §2.5 argues is the real
	// government shape (patching must not run on the holiday, the sweep must).
	for _, e := range []struct{ name, skip string }{
		{"nightly-patch", `["holidays"]`},
		{"weekly-patch", `["holidays"]`},
		{"compliance-sweep", ""},
	} {
		if _, err := pool.ExecContext(ctx, `
			INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, skip_calendars)
			VALUES('git','job','patch',?,'0 0 2 * * *',0,?)`, e.name, nullOrEmpty(e.skip)); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}

	var body struct {
		CalendarRollup map[string]string `json:"calendarRollup"`
	}
	getJSON(t, api.client, api.ts+"/api/v1/schedules", &body)
	got := body.CalendarRollup["job:git:patch"]
	if got != "holidays (2 of 3 entries)" {
		t.Errorf("rollup = %q, want \"holidays (2 of 3 entries)\" — disagreement must be visible, not averaged away", got)
	}
}

// CAL-27's first refusal must hold on CREATE too: force-deleting a calendar
// leaves its only-bindings dangling by design, so re-creating that name as
// global is a reachable path to the state the refusal exists to prevent.
func TestGlobalRefusedOnCreateWhenNameStillBoundAsOnly(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, only_calendars)
		VALUES('git','job','sweep','nightly','0 0 2 * * *',0,'["fiscal-close"]')`); err != nil {
		t.Fatalf("seed dangling binding: %v", err)
	}
	code, body := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "fiscal-close", "global": true,
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("create global over an only-bound name = %d, want 422", code)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "fiscal-close") {
		t.Errorf("422 message %q should name the calendar", msg)
	}
}

// A first-class schedule that binds a calendar is a real binding even before any
// job references it — the delete guard and the global guard must both see it.
func TestFirstClassScheduleBindingBlocksDeleteAndGlobal(t *testing.T) {
	api, pool := newCalAPI(t)
	ctx := context.Background()
	if code, _ := api.do(http.MethodPost, "/api/v1/calendars", map[string]any{
		"name": "fiscal-close", "days": []map[string]string{{"day": "2026-09-30"}},
	}); code != http.StatusCreated {
		t.Fatal("seed calendar failed")
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO schedules(name, source, cron, content_hash, only_calendars, created_at)
		VALUES('fiscal-only','cronomicon','0 0 2 * * *','sha256:x','["fiscal-close"]','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	if code, _ := api.do(http.MethodPut, "/api/v1/calendars/fiscal-close", map[string]any{"global": true}); code != http.StatusUnprocessableEntity {
		t.Errorf("promote to global = %d, want 422 — a catalog-only binding is still a binding", code)
	}
	if code, _ := api.do(http.MethodDelete, "/api/v1/calendars/fiscal-close", nil); code != http.StatusConflict {
		t.Errorf("delete = %d, want 409 — a catalog-only binding must block it", code)
	}
}

func nullOrEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
