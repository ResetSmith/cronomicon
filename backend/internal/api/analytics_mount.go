package api

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// Run analytics (SL-E, the prod-features plan §3).
//
// The Score plots individual marks and the job detail draws a sparkline of the
// last twenty runs; neither answers "is this getting worse?". This does, over a
// selectable window, from the runs table.
//
// # Computed on demand (PF-Q7)
//
// No rollup tables. The window is bounded, the new (job_name, job_source,
// created_at) index covers the range scan, and every number here is a single
// aggregate pass — so a rollup would be a second copy of the truth to keep in
// sync in exchange for latency nobody has measured as a problem. If it ever
// becomes slow the fix is a materialized daily rollup behind this same
// endpoint, which is why the response shape says nothing about how it was
// derived.
//
//	GET /api/v1/analytics/runs?job=&source=&window=30d
//
// Omitting `job` gives the fleet-wide view, which is what the History tab uses.
func (s *Server) mountAnalytics(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/analytics/runs", s.auth.RequireSession(http.HandlerFunc(s.runAnalytics)))
}

type analyticsBucket struct {
	Day string `json:"day"`
	// Total is that day's EXECUTED runs; Skipped is its suppressions (FX-D2).
	// The bucket is created for either, so a day on which everything was
	// suppressed still appears — the day did happen, and a gap in the sparkline
	// would read as missing data rather than as a deliberate quiet day. What
	// changed is that its bar no longer claims those suppressions were runs.
	Total   int `json:"total"`
	Skipped int `json:"skipped"`
	Success int `json:"success"`
	Failure int `json:"failure"`
	// P50Ms is the median duration of that day's terminal runs, or null when the
	// day had none that recorded one.
	P50Ms *int64 `json:"p50Ms,omitempty"`
}

type analyticsFailure struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type analyticsResponse struct {
	Job        string `json:"job,omitempty"`
	Source     string `json:"source,omitempty"`
	WindowDays int    `json:"windowDays"`
	From       string `json:"from"`
	// Total is EXECUTED runs in the window — everything except suppressions.
	// Skipped counts those separately, so the two never overlap and the number of
	// fires the window saw is Total + Skipped (FX-D2).
	Total       int                `json:"total"`
	Success     int                `json:"success"`
	Failure     int                `json:"failure"`
	Warning     int                `json:"warning"`
	Killed      int                `json:"killed"`
	Skipped     int                `json:"skipped"`
	Running     int                `json:"running"`
	SuccessRate *float64           `json:"successRate"` // null when nothing terminal ran
	P50Ms       *int64             `json:"p50Ms,omitempty"`
	P95Ms       *int64             `json:"p95Ms,omitempty"`
	MaxMs       *int64             `json:"maxMs,omitempty"`
	Buckets     []analyticsBucket  `json:"buckets"`
	Failures    []analyticsFailure `json:"failures"`
	SLABreaches int                `json:"slaBreaches"`
	MissedRuns  int                `json:"missedRuns"`
}

// parseWindowDays reads ?window= as a day count ("7d", "30", "90d"). Bounded at
// a year: the point of the window is to be a window, and an unbounded one turns
// a range scan into a full-table scan on a busy install.
func parseWindowDays(v string) int {
	v = strings.TrimSpace(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), "d"))
	if v == "" {
		return 30
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 30
	}
	if n > 365 {
		return 365
	}
	return n
}

func (s *Server) runAnalytics(w http.ResponseWriter, r *http.Request) {
	job := strings.TrimSpace(r.URL.Query().Get("job"))
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	days := parseWindowDays(r.URL.Query().Get("window"))
	loc := s.appLocation()
	from := time.Now().UTC().AddDate(0, 0, -days)

	where := " WHERE created_at >= ? AND kind = 'job'"
	args := []any{from.Format(time.RFC3339)}
	if job != "" {
		where += " AND job_name = ?"
		args = append(args, job)
		if source != "" {
			where += " AND COALESCE(job_source,'git') = ?"
			args = append(args, source)
		}
	}
	// RB — the caller only ever sees runs in scopes they can read. Reusing the
	// same fragment every other list endpoint uses, so analytics cannot become a
	// side channel that reveals the existence of work in a scope the caller has
	// no grant for.
	scopeFrag, scopeArgs := s.scopeWhereFragment(r, "scope")
	if scopeFrag != "" {
		where += " AND " + scopeFrag
		args = append(args, scopeArgs...)
	}

	out := analyticsResponse{
		Job: job, Source: source, WindowDays: days,
		From:     from.Format(time.RFC3339),
		Buckets:  []analyticsBucket{},
		Failures: []analyticsFailure{},
	}

	// One pass over the window; the aggregates below are all derived from it.
	// Deliberately NOT five separate COUNT queries: at this row count the scan is
	// the cost, and doing it once keeps every number describing the same instant.
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT status, COALESCE(duration_ms,0), created_at, COALESCE(queued_reason,'') FROM runs`+where, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	var durations []int64
	byDay := map[string]*analyticsBucket{}
	durationsByDay := map[string][]int64{}
	failureReasons := map[string]int{}
	for rows.Next() {
		var status, createdAt, reason string
		var durationMs int64
		if err := rows.Scan(&status, &durationMs, &createdAt, &reason); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// FX-D2 — Total counts what RAN. A suppression is a real row here (it is
		// the proof a fire was expected and deliberately did not happen) but it is
		// not a run, and the UI renders this number as "N runs in the last 30
		// days": a job suppressed every day by a working calendar read as fully
		// active. Skipped keeps its own count, so the two buckets no longer
		// overlap and "fires observed" is still derivable as Total + Skipped.
		if status != "skipped" {
			out.Total++
		}
		switch status {
		case "success":
			out.Success++
		case "failure":
			out.Failure++
		case "warning":
			out.Warning++
		case "killed":
			out.Killed++
		case "skipped":
			out.Skipped++
		case "running", "queued":
			out.Running++
		}
		// SL rows are recorded as skipped runs carrying a known reason; surfacing
		// them here is what makes the analytics view the home of the SLA story
		// rather than a second place to go looking.
		if strings.HasPrefix(reason, "Missed:") {
			out.MissedRuns++
		}
		if durationMs > 0 {
			durations = append(durations, durationMs)
		}
		if status == "failure" && reason != "" {
			failureReasons[reason]++
		}

		day := ""
		if t := parseRFC3339(createdAt); !t.IsZero() {
			day = t.In(loc).Format("2006-01-02")
		}
		if day == "" {
			continue
		}
		b := byDay[day]
		if b == nil {
			b = &analyticsBucket{Day: day}
			byDay[day] = b
		}
		// Same rule per day: the bar is runs, not fires. A day of suppressions
		// otherwise drew a full-height bar with nothing in it.
		if status == "skipped" {
			b.Skipped++
		} else {
			b.Total++
		}
		switch status {
		case "success":
			b.Success++
		case "failure":
			b.Failure++
		}
		// The per-day median needs that day's durations kept separately from the
		// window's — the window aggregate below cannot answer "was Tuesday slow".
		if durationMs > 0 {
			durationsByDay[day] = append(durationsByDay[day], durationMs)
		}
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// The rate is over TERMINAL OUTCOMES only. Counting skipped runs as failures
	// would make a job suppressed by a working calendar look broken, which is the
	// opposite of what the calendar feature exists to express; counting them as
	// successes would inflate the number of a job that never ran.
	terminal := out.Success + out.Failure + out.Warning + out.Killed
	if terminal > 0 {
		rate := float64(out.Success) / float64(terminal)
		out.SuccessRate = &rate
	}
	if len(durations) > 0 {
		slices.Sort(durations)
		p50 := percentile(durations, 0.50)
		p95 := percentile(durations, 0.95)
		max := durations[len(durations)-1]
		out.P50Ms, out.P95Ms, out.MaxMs = &p50, &p95, &max
	}

	// SLA breaches are stamped on the run rather than recorded as their own row,
	// so they need their own count.
	slaWhere := where + " AND sla_warned_at IS NOT NULL"
	_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM runs`+slaWhere, args...).Scan(&out.SLABreaches)

	dayKeys := make([]string, 0, len(byDay))
	for d := range byDay {
		dayKeys = append(dayKeys, d)
	}
	sort.Strings(dayKeys)
	for _, d := range dayKeys {
		if ds := durationsByDay[d]; len(ds) > 0 {
			slices.Sort(ds)
			p50 := percentile(ds, 0.50)
			byDay[d].P50Ms = &p50
		}
		out.Buckets = append(out.Buckets, *byDay[d])
	}

	for reason, n := range failureReasons {
		out.Failures = append(out.Failures, analyticsFailure{Reason: reason, Count: n})
	}
	sort.Slice(out.Failures, func(i, j int) bool {
		if out.Failures[i].Count != out.Failures[j].Count {
			return out.Failures[i].Count > out.Failures[j].Count
		}
		return out.Failures[i].Reason < out.Failures[j].Reason
	})
	if len(out.Failures) > 8 {
		out.Failures = out.Failures[:8] // the clustering, not the catalogue
	}

	httpx.JSON(w, http.StatusOK, out)
}

// percentile returns the p-th percentile of a SORTED slice, nearest-rank.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := max(int(math.Ceil(p*float64(len(sorted))))-1, 0)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func parseRFC3339(v string) time.Time {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
