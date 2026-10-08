package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TS-20/TS-22 (the sorting-update plan): the paged list endpoints accept
// allowlisted ?sort=&order= — ordering happens in SQL before pagination, an
// unknown key or direction is a 400 (never interpolated), and empty cells sort
// last in both directions, matching the client-side comparator.

// seedRun inserts a minimal CHECK-valid runs row (shape mirrors the ssh-test
// mirror insert). durationMs < 0 ⇒ NULL duration (a still-running run).
func seedRun(t *testing.T, exec func(q string, args ...any), id, job, status string, durationMs int, createdAt string) {
	t.Helper()
	var dur any
	if durationMs >= 0 {
		dur = durationMs
	}
	exec(`INSERT INTO runs(id, kind, job_name, run_type, executor, status,
	                       triggered_by, trigger_kind, started_at, completed_at,
	                       duration_ms, created_at, entity_code)
	      VALUES (?, 'job', ?, 'bash', 'ssh', ?, 'tester', 'manual', ?, ?, ?, ?, '_system')`,
		id, job, status, createdAt, createdAt, dur, createdAt)
}

func getItems(t *testing.T, client *http.Client, url string) []map[string]any {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body.Items
}

func TestListRunsSortParam(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLogin(t, ts.URL)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}

	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	at := func(i int) string { return base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339) }
	seedRun(t, exec, "r1", "charlie", "success", 500, at(0))
	seedRun(t, exec, "r2", "alpha", "failure", 100, at(1))
	seedRun(t, exec, "r3", "bravo", "warning", -1, at(2)) // NULL duration (running)

	jobs := func(items []map[string]any) []string {
		var out []string
		for _, it := range items {
			out = append(out, it["jobName"].(string))
		}
		return out
	}

	// Default (no sort): created_at DESC — newest first, unchanged contract.
	got := jobs(getItems(t, client, ts.URL+"/api/v1/runs"))
	if fmt.Sprint(got) != "[bravo alpha charlie]" {
		t.Fatalf("default order = %v, want [bravo alpha charlie]", got)
	}

	// duration asc: 100, 500, then the NULL-duration run LAST (not first, which
	// is where naive SQLite NULL ordering would put it).
	got = jobs(getItems(t, client, ts.URL+"/api/v1/runs?sort=duration&order=asc"))
	if fmt.Sprint(got) != "[alpha charlie bravo]" {
		t.Fatalf("duration asc = %v, want [alpha charlie bravo]", got)
	}
	// duration desc: NULL still last.
	got = jobs(getItems(t, client, ts.URL+"/api/v1/runs?sort=duration&order=desc"))
	if fmt.Sprint(got) != "[charlie alpha bravo]" {
		t.Fatalf("duration desc = %v, want [charlie alpha bravo]", got)
	}

	// status asc: severity rank (worst first), not alphabetical — failure,
	// warning, success. Alphabetical would give failure, success, warning.
	got = jobs(getItems(t, client, ts.URL+"/api/v1/runs?sort=status&order=asc"))
	if fmt.Sprint(got) != "[alpha bravo charlie]" {
		t.Fatalf("status asc = %v, want [alpha bravo charlie] (rank order)", got)
	}

	// job asc: plain text column.
	got = jobs(getItems(t, client, ts.URL+"/api/v1/runs?sort=job&order=asc"))
	if fmt.Sprint(got) != "[alpha bravo charlie]" {
		t.Fatalf("job asc = %v", got)
	}

	// Pagination happens AFTER the sort: page 2 of pageSize 1 under job asc is
	// the middle row of the sorted set, not of the default order.
	got = jobs(getItems(t, client, ts.URL+"/api/v1/runs?sort=job&order=asc&page=2&pageSize=1"))
	if fmt.Sprint(got) != "[bravo]" {
		t.Fatalf("job asc page2/size1 = %v, want [bravo]", got)
	}

	// Unknown sort key and bad order: 400, never interpolated.
	for _, u := range []string{"/api/v1/runs?sort=rowid", "/api/v1/runs?sort=job&order=sideways"} {
		resp, err := client.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s = %d, want 400", u, resp.StatusCode)
		}
	}
}

// TestRunRowsNameTheRunnerThatTookThem: History shows a Runner where it showed
// an Executor (2.3.0, LR-50). Every run is the runner executor's now, so the
// row carries the NAME of the runner that took it — the run's own copy
// (migration 1280), which outlives the runner. `sort=runner` orders by what
// the column shows, runs nobody took last, and the old `sort=executor` key — a
// column that says the same thing on every new row — is an unknown key.
func TestRunRowsNameTheRunnerThatTookThem(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLogin(t, ts.URL)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	at := func(i int) string { return base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339) }
	seedRun(t, exec, "r1", "charlie", "success", 500, at(0))
	seedRun(t, exec, "r2", "alpha", "success", 100, at(1))
	seedRun(t, exec, "r3", "bravo", "success", 50, at(2)) // an old SSH-executor run: no runner
	seedRun(t, exec, "r4", "delta", "queued", -1, at(3))  // queued for a runner, not yet claimed
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('rn-z','zulu-agent','online','t','t')`)
	// What the claim writes: the id and the run's own copy of the name. r2's
	// runner is already gone (runner_id is ON DELETE SET NULL); the name is not.
	exec(`UPDATE runs SET runner_id = 'rn-z', runner_name = 'zulu-agent', executor = 'runner' WHERE id = 'r1'`)
	exec(`UPDATE runs SET runner_id = NULL, runner_name = 'alpha-agent', executor = 'runner' WHERE id = 'r2'`)
	exec(`UPDATE runs SET executor = 'runner' WHERE id = 'r4'`)

	names := func() map[string]any {
		out := map[string]any{}
		for _, it := range getItems(t, client, ts.URL+"/api/v1/runs") {
			out[it["jobName"].(string)] = it["runnerName"]
		}
		return out
	}
	if got := names(); got["charlie"] != "zulu-agent" || got["alpha"] != "alpha-agent" || got["bravo"] != nil || got["delta"] != nil {
		t.Fatalf("runnerName by job = %v, want charlie=zulu-agent, alpha=alpha-agent, bravo and delta nil", got)
	}

	order := func(u string) string {
		var out []string
		for _, it := range getItems(t, client, ts.URL+u) {
			out = append(out, it["jobName"].(string))
		}
		return fmt.Sprint(out)
	}
	// By what the column shows, in the database's byte order: "Server (SSH)"
	// for the old SSH run (so those rows still sort together), alpha-agent,
	// zulu-agent — and the run no runner has taken last in both directions.
	if got := order("/api/v1/runs?sort=runner&order=asc"); got != "[bravo alpha charlie delta]" {
		t.Errorf("runner asc = %s, want [bravo alpha charlie delta]", got)
	}
	if got := order("/api/v1/runs?sort=runner&order=desc"); got != "[charlie alpha bravo delta]" {
		t.Errorf("runner desc = %s, want [charlie alpha bravo delta]", got)
	}

	// Deregistering the runner takes its row and the run's id, not the name.
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'rn-z'`)
	exec(`DELETE FROM runners WHERE id = 'rn-z'`)
	if got := names(); got["charlie"] != "zulu-agent" {
		t.Errorf("after its runner was deregistered the run names %v, want zulu-agent still", got["charlie"])
	}

	resp, err := client.Get(ts.URL + "/api/v1/runs?sort=executor")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("GET /runs?sort=executor = %d, want 400: the key is retired", resp.StatusCode)
	}
}

func TestListChangeLogSortParam(t *testing.T) {
	ts, pool := newTestServer(t)
	client, _ := devLogin(t, ts.URL)

	base := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	for i, row := range []struct{ actor, action string }{
		{"zoe@example.com", "created"},
		{"amy@example.com", "deleted"},
	} {
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if _, err := pool.Exec(`INSERT INTO change_log(at, actor, category, action, target, created_at)
			VALUES (?, ?, 'Settings', ?, 't', ?)`, at, row.actor, row.action, at); err != nil {
			t.Fatal(err)
		}
	}

	users := func(items []map[string]any) []string {
		var out []string
		for _, it := range items {
			out = append(out, it["user"].(string))
		}
		return out
	}
	got := users(getItems(t, client, ts.URL+"/api/v1/change-log?sort=user&order=asc"))
	if fmt.Sprint(got) != "[amy@example.com zoe@example.com]" {
		t.Fatalf("user asc = %v", got)
	}
	got = users(getItems(t, client, ts.URL+"/api/v1/change-log"))
	if fmt.Sprint(got) != "[amy@example.com zoe@example.com]" {
		// Default at DESC: amy is the newer row (i=1).
		t.Fatalf("default = %v, want newest first [amy zoe]", got)
	}

	resp, err := client.Get(ts.URL + "/api/v1/change-log?sort=details")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("sort=details (excluded free-text column) = %d, want 400", resp.StatusCode)
	}
}

func TestListSchedulePushesSortRejection(t *testing.T) {
	ts, _ := newTestServer(t)
	client, _ := devLogin(t, ts.URL)
	resp, err := client.Get(ts.URL + "/api/v1/schedule-pushes?sort=details")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown sort = %d, want 400", resp.StatusCode)
	}
}
