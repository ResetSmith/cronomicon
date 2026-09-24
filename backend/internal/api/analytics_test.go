package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// SL-E — run analytics.
//
// The number most worth defending is the success rate, because the obvious
// implementation of it is wrong: counting skipped runs as failures makes a job
// correctly suppressed by a working calendar look broken, which is the exact
// opposite of what the calendar feature exists to express.

func seedAnalyticsRun(t *testing.T, pool *sql.DB, id, job, status string, durationMs int64, ago time.Duration, reason string) {
	t.Helper()
	at := time.Now().UTC().Add(-ago).Format(time.RFC3339)
	var r any
	if reason != "" {
		r = reason
	}
	if _, err := pool.Exec(`
		INSERT INTO runs (id, job_name, job_source, run_type, scope, kind, status, duration_ms,
		                  queued_reason, triggered_by, trigger_kind, created_at)
		VALUES (?, ?, 'git', 'bash', 'prod', 'job', ?, ?, ?, 'scheduler', 'scheduled', ?)`,
		id, job, status, durationMs, r, at); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
}

func TestAnalyticsSuccessRateExcludesSkippedRuns(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	// 3 succeeded, 1 failed, 5 suppressed by a calendar.
	seedAnalyticsRun(t, pool, "s1", "billing", "success", 1000, time.Hour, "")
	seedAnalyticsRun(t, pool, "s2", "billing", "success", 2000, 2*time.Hour, "")
	seedAnalyticsRun(t, pool, "s3", "billing", "success", 3000, 3*time.Hour, "")
	seedAnalyticsRun(t, pool, "f1", "billing", "failure", 500, 4*time.Hour, "exit status 1")
	for i := range 5 {
		seedAnalyticsRun(t, pool, "k"+string(rune('a'+i)), "billing", "skipped", 0,
			time.Duration(5+i)*time.Hour, "Skipped: Independence Day")
	}

	resp, err := client.Get(ts.URL + "/api/v1/analytics/runs?job=billing&window=30d")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Total       int      `json:"total"`
		Success     int      `json:"success"`
		Failure     int      `json:"failure"`
		Skipped     int      `json:"skipped"`
		SuccessRate *float64 `json:"successRate"`
		P50Ms       *int64   `json:"p50Ms"`
		Failures    []struct {
			Reason string `json:"reason"`
			Count  int    `json:"count"`
		} `json:"failures"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// FX-D2 — total is what RAN (4), not every row (9). The suppressions keep
	// their own count, so the two buckets do not overlap and "fires observed" is
	// still total+skipped. This number is rendered to the operator as "N runs in
	// the last 30 days", so counting five calendar vetoes in it made a job that
	// was stopped every day read as fully active.
	if out.Total != 4 || out.Success != 3 || out.Failure != 1 || out.Skipped != 5 {
		t.Fatalf("counts = total %d / success %d / failure %d / skipped %d, want 4/3/1/5",
			out.Total, out.Success, out.Failure, out.Skipped)
	}
	if out.SuccessRate == nil {
		t.Fatal("successRate is null with terminal runs present")
	}
	// 3 of 4 TERMINAL outcomes, not 3 of 9. Counting the calendar skips would
	// give 0.33 and make a correctly-suppressed job look broken.
	if got := *out.SuccessRate; got < 0.74 || got > 0.76 {
		t.Errorf("successRate = %.3f, want ~0.75 — skipped runs must not count against a job", got)
	}
	// Durations across ALL timed runs are [500, 1000, 2000, 3000] — the failed
	// run's 500ms counts, because "how long does this take" is not a question
	// only about the successes. Nearest-rank median of four values is the second.
	if out.P50Ms == nil {
		t.Error("p50Ms is null with timed runs present")
	} else if *out.P50Ms != 1000 {
		t.Errorf("p50Ms = %d, want 1000", *out.P50Ms)
	}
	if len(out.Failures) != 1 || out.Failures[0].Reason != "exit status 1" || out.Failures[0].Count != 1 {
		t.Errorf("failure clustering = %+v, want one 'exit status 1'", out.Failures)
	}
}

// An empty window is not an error, and a rate over nothing is null rather than
// zero — "no runs" and "everything failed" must not render the same.
func TestAnalyticsEmptyWindowIsNullNotZero(t *testing.T) {
	ts, _ := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	resp, err := client.Get(ts.URL + "/api/v1/analytics/runs?job=nothing-here")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Total       int      `json:"total"`
		SuccessRate *float64 `json:"successRate"`
		Buckets     []any    `json:"buckets"`
		Failures    []any    `json:"failures"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Total != 0 {
		t.Errorf("total = %d, want 0", out.Total)
	}
	if out.SuccessRate != nil {
		t.Errorf("successRate = %v, want null — a rate over nothing is unknown, not 0%%", *out.SuccessRate)
	}
	if out.Buckets == nil || out.Failures == nil {
		t.Error("buckets/failures serialized as null rather than []; the UI maps over them")
	}
}

// The window bounds the scan and is clamped, so an unbounded ?window= cannot
// turn a range scan into a full-table scan.
func TestAnalyticsWindowIsBoundedAndFilters(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	seedAnalyticsRun(t, pool, "recent", "billing", "success", 100, 2*24*time.Hour, "")
	seedAnalyticsRun(t, pool, "old", "billing", "success", 100, 60*24*time.Hour, "")

	read := func(q string) int {
		resp, err := client.Get(ts.URL + "/api/v1/analytics/runs?job=billing&window=" + q)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var out struct {
			Total      int `json:"total"`
			WindowDays int `json:"windowDays"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.WindowDays > 365 {
			t.Errorf("windowDays = %d, want <= 365", out.WindowDays)
		}
		return out.Total
	}

	if n := read("7d"); n != 1 {
		t.Errorf("7d window = %d runs, want 1 (the 60-day-old run is outside it)", n)
	}
	if n := read("90d"); n != 2 {
		t.Errorf("90d window = %d runs, want 2", n)
	}
	if n := read("99999d"); n != 2 {
		t.Errorf("clamped window = %d runs, want 2", n)
	}
	if n := read("garbage"); n != 1 {
		t.Errorf("unparseable window = %d runs, want 1 (the 30-day default)", n)
	}
}

// Each bucket carries its OWN median, not the window's.
//
// The gap this closes survived because the existing coverage stopped at the
// top-level aggregate: every bucket serialized without p50Ms and the trend line
// the buckets exist to draw was flat by construction, which reads as "nothing
// changed" rather than as a missing number. The assertion that matters is
// therefore that the two days DISAGREE with each other and with the window —
// a per-day p50 that happened to equal the window's would prove nothing.
func TestAnalyticsBucketsCarryTheirOwnMedian(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	// Anchored at local noon: the buckets are keyed in the app zone (time.Local
	// here, since no timezone setting is stored), and midday keeps a run from
	// landing on the neighbouring day.
	day := func(daysAgo int) (string, time.Duration) {
		t.Helper()
		now := time.Now()
		at := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.Local).AddDate(0, 0, -daysAgo)
		return at.Format("2006-01-02"), time.Since(at)
	}
	slowDay, slowAgo := day(5)
	fastDay, fastAgo := day(2)
	quietDay, quietAgo := day(3)

	// A slow day and a fast day, ordered so neither day's median can be reached by
	// accident from the other's rows.
	for i, ms := range []int64{100, 900, 5000} {
		seedAnalyticsRun(t, pool, "slow"+string(rune('a'+i)), "billing", "success", ms, slowAgo+time.Duration(i)*time.Minute, "")
	}
	for i, ms := range []int64{10, 20, 30, 40} {
		seedAnalyticsRun(t, pool, "fast"+string(rune('a'+i)), "billing", "success", ms, fastAgo+time.Duration(i)*time.Minute, "")
	}
	// A day whose runs were all suppressed by a calendar: it still gets a bucket
	// (something was scheduled and something happened) but has no duration to take
	// a median of, and 0ms would read as "instant" rather than "never ran".
	seedAnalyticsRun(t, pool, "quiet1", "billing", "skipped", 0, quietAgo, "Skipped: Independence Day")

	resp, err := client.Get(ts.URL + "/api/v1/analytics/runs?job=billing&window=30d")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		P50Ms   *int64 `json:"p50Ms"`
		Buckets []struct {
			Day     string `json:"day"`
			Total   int    `json:"total"`
			Skipped int    `json:"skipped"`
			P50Ms   *int64 `json:"p50Ms"`
		} `json:"buckets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Nearest-rank over all eight timed runs [10,20,30,40,100,900,5000] is the
	// fourth: 40ms. No day's median is 40, which is the point.
	if out.P50Ms == nil || *out.P50Ms != 40 {
		t.Errorf("window p50Ms = %v, want 40", out.P50Ms)
	}

	got := map[string]*int64{}
	totals := map[string]int{}
	skips := map[string]int{}
	for _, b := range out.Buckets {
		got[b.Day] = b.P50Ms
		totals[b.Day] = b.Total
		skips[b.Day] = b.Skipped
	}
	if len(out.Buckets) != 3 {
		t.Fatalf("buckets = %d, want 3 (%s, %s, %s): %+v", len(out.Buckets), slowDay, quietDay, fastDay, out.Buckets)
	}
	// Buckets stay in day order — the sparkline reads left to right.
	if out.Buckets[0].Day != slowDay || out.Buckets[2].Day != fastDay {
		t.Errorf("bucket order = %s..%s, want %s..%s", out.Buckets[0].Day, out.Buckets[2].Day, slowDay, fastDay)
	}
	// [100,900,5000] → the second; [10,20,30,40] → the second.
	for _, tc := range []struct {
		day  string
		want int64
	}{{slowDay, 900}, {fastDay, 20}} {
		p50, ok := got[tc.day]
		if !ok {
			t.Errorf("no bucket for %s", tc.day)
			continue
		}
		if p50 == nil {
			t.Errorf("%s p50Ms is null with timed runs present", tc.day)
			continue
		}
		if *p50 != tc.want {
			t.Errorf("%s p50Ms = %d, want %d — that day's median, not the window's", tc.day, *p50, tc.want)
		}
	}
	if p50 := got[quietDay]; p50 != nil {
		t.Errorf("%s p50Ms = %d, want null — a day with nothing timed has no median, and 0 would read as instant", quietDay, *p50)
	}
	// The day still gets a bucket — that is the point above, and a gap in the
	// sparkline would read as missing data rather than a deliberate quiet day —
	// but FX-D2 makes its total the count of what RAN, which is nothing. The
	// suppression is carried in the bucket's own skipped count instead, so the
	// day is neither invisible nor pretending to be busy.
	if _, ok := totals[quietDay]; !ok {
		t.Errorf("no bucket for %s — the day still happened", quietDay)
	}
	if totals[quietDay] != 0 {
		t.Errorf("%s total = %d, want 0 — nothing ran that day, and this number draws the bar",
			quietDay, totals[quietDay])
	}
	if skips[quietDay] != 1 {
		t.Errorf("%s skipped = %d, want 1 — the suppression must still be visible per-day",
			quietDay, skips[quietDay])
	}
}

// Missed-run rows are surfaced as their own number rather than buried in the
// skipped count — the analytics view is the home of the SLA story.
func TestAnalyticsCountsMissedRunsSeparately(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	seedAnalyticsRun(t, pool, "m1", "billing", "skipped", 0, time.Hour,
		"Missed: the schedule expected a fire and no run appeared")
	seedAnalyticsRun(t, pool, "c1", "billing", "skipped", 0, 2*time.Hour, "Skipped: Independence Day")

	resp, err := client.Get(ts.URL + "/api/v1/analytics/runs?job=billing")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Skipped    int `json:"skipped"`
		MissedRuns int `json:"missedRuns"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", out.Skipped)
	}
	if out.MissedRuns != 1 {
		t.Errorf("missedRuns = %d, want 1 — a silent miss is not the same event as a holiday", out.MissedRuns)
	}
}
