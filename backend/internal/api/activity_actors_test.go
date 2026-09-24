package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// AA-2 — the feed's runner filter and the facet endpoint that populates its
// picker. What is pinned here is the WIRE: which rows `?runner=` returns, and
// how `/activity/actors` classifies an actor. Classification lives on the
// server precisely so there is one implementation of "does this look like a
// person", and a test on the client could not hold that.

type actorFacets struct {
	Users   []string `json:"users"`
	Runners []string `json:"runners"`
	System  []string `json:"system"`
}

type activityPage struct {
	TotalItems int `json:"totalItems"`
	Items      []struct {
		Kind       string  `json:"kind"`
		Actor      string  `json:"actor"`
		RunnerName *string `json:"runnerName"`
		JobName    *string `json:"jobName"`
	} `json:"items"`
}

// seedActorActivity lays down one row of each shape the classifier must sort:
// two people, two runners, two system writers, and a stopped run — the row the
// AA-1 gap left unstamped and migration 1110 + the kill handler now cover.
func seedActorActivity(t *testing.T, db *sql.DB) {
	t.Helper()
	at := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	rows := []struct{ kind, actor, runner, jobName, at string }{
		{"run-end", "runner:rn-1", "runner-east", "nightly", at},
		{"run-start", "runner:rn-2", "runner-west", "patch", at},
		{"config", "alice@corp.example", "runner-east", "", at},
		{"run-end", "bob@corp.example", "runner-east", "stopped-job", at}, // a STOPPED run
		{"run-end", "ssh-executor", "", "sshjob", at},
		{"run-end", "system", "runner-west", "reaped", at},
		// Outside a 72h window: must not appear in a windowed facet list.
		{"config", "carol@corp.example", "runner-ancient", "", old},
	}
	for _, r := range rows {
		var runner, job any
		if r.runner != "" {
			runner = r.runner
		}
		if r.jobName != "" {
			job = r.jobName
		}
		if _, err := db.Exec(`
			INSERT INTO activity(kind, actor, runner_name, job_name, at, created_at)
			VALUES (?,?,?,?,?,?)`, r.kind, r.actor, runner, job, r.at, r.at); err != nil {
			t.Fatalf("seed activity: %v", err)
		}
	}
}

func TestActivityRunnerFilter(t *testing.T) {
	ts, db := newTestServer(t)
	seedActorActivity(t, db)
	client, _ := devLoginWithCSRF(t, ts)

	var page activityPage
	getJSON(t, client, ts.URL+"/api/v1/activity?runner=runner-east&pageSize=50", &page)

	if page.TotalItems != 3 {
		t.Fatalf("?runner=runner-east: totalItems = %d, want 3", page.TotalItems)
	}
	for _, it := range page.Items {
		if it.RunnerName == nil || *it.RunnerName != "runner-east" {
			t.Errorf("row %+v leaked into the runner-east filter", it)
		}
	}

	// The gap AA-1 recorded and AA-2 closes: a run an operator STOPPED is still
	// a run that executed on a runner. Its activity row's actor is the
	// operator's email, so an actor-based filter can never find it — which is
	// exactly why runner_name is a separate column.
	var found bool
	for _, it := range page.Items {
		if it.JobName != nil && *it.JobName == "stopped-job" {
			found = true
			if it.Actor != "bob@corp.example" {
				t.Errorf("stopped run actor = %q, want the operator's email", it.Actor)
			}
		}
	}
	if !found {
		t.Error("?runner= excluded a STOPPED run — 'everything runner X ran' must include runs an operator intervened in")
	}
}

func TestActivityRunnerFilterComposesWithKind(t *testing.T) {
	ts, db := newTestServer(t)
	seedActorActivity(t, db)
	client, _ := devLoginWithCSRF(t, ts)

	var page activityPage
	getJSON(t, client, ts.URL+"/api/v1/activity?runner=runner-east&kind=config&pageSize=50", &page)
	if page.TotalItems != 1 {
		t.Fatalf("runner+kind: totalItems = %d, want 1 (the filters must AND)", page.TotalItems)
	}
}

func TestActivityActorsClassifies(t *testing.T) {
	ts, db := newTestServer(t)
	seedActorActivity(t, db)
	client, _ := devLoginWithCSRF(t, ts)

	var f actorFacets
	getJSON(t, client, ts.URL+"/api/v1/activity/actors", &f)

	has := func(hay []string, needle string) bool {
		for _, h := range hay {
			if h == needle {
				return true
			}
		}
		return false
	}
	for _, u := range []string{"alice@corp.example", "bob@corp.example"} {
		if !has(f.Users, u) {
			t.Errorf("users missing %q (an '@' actor is a person)", u)
		}
	}
	for _, rn := range []string{"runner-east", "runner-west"} {
		if !has(f.Runners, rn) {
			t.Errorf("runners missing %q", rn)
		}
	}
	for _, sysActor := range []string{"ssh-executor", "system"} {
		if !has(f.System, sysActor) {
			t.Errorf("system missing %q", sysActor)
		}
	}
	// A runner must never appear in `system` via its `runner:<id>` actor — the
	// picker would show a UUID, which is the unreadable thing AA-1 exists to fix.
	for _, s := range f.System {
		if len(s) > 7 && s[:7] == "runner:" {
			t.Errorf("system contains %q — a raw runner id must not reach the picker", s)
		}
	}
	// Distinct, not one entry per row: runner-east wrote three rows above.
	seen := map[string]int{}
	for _, rn := range f.Runners {
		seen[rn]++
	}
	if seen["runner-east"] != 1 {
		t.Errorf("runner-east appears %d times, want 1 (SELECT DISTINCT)", seen["runner-east"])
	}
}

// AA-Q5 — the picker offers the population the FEED is showing, so the same
// from/to that scope the feed scope the options.
func TestActivityActorsIsWindowScoped(t *testing.T) {
	ts, db := newTestServer(t)
	seedActorActivity(t, db)
	client, _ := devLoginWithCSRF(t, ts)

	from := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	var windowed actorFacets
	getJSON(t, client, ts.URL+"/api/v1/activity/actors?from="+from, &windowed)
	for _, u := range windowed.Users {
		if u == "carol@corp.example" {
			t.Error("a 30-day-old actor leaked into the 72h window")
		}
	}
	for _, rn := range windowed.Runners {
		if rn == "runner-ancient" {
			t.Error("a 30-day-old runner leaked into the 72h window")
		}
	}

	// Unwindowed, the same actor IS offered — the filter is the window, not a
	// permanent exclusion.
	var all actorFacets
	getJSON(t, client, ts.URL+"/api/v1/activity/actors", &all)
	var foundOld bool
	for _, u := range all.Users {
		if u == "carol@corp.example" {
			foundOld = true
		}
	}
	if !foundOld {
		t.Error("unwindowed actors omitted a 30-day-old actor that is still within retention")
	}
}

func TestActivityActorsRequiresASession(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/api/v1/activity/actors")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET /activity/actors = %d, want 401", resp.StatusCode)
	}
}

// The response must be arrays, never nulls — the client iterates them.
func TestActivityActorsEmptyGroupsAreArrays(t *testing.T) {
	ts, _ := newTestServer(t)
	client, _ := devLoginWithCSRF(t, ts)

	resp, err := client.Get(ts.URL + "/api/v1/activity/actors?from=" +
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"users", "runners", "system"} {
		if got := string(raw[k]); got != "[]" {
			t.Errorf("%s = %s, want [] for an empty group (never null)", k, got)
		}
	}
}
