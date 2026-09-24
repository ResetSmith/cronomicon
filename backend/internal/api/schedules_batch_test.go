package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// TestListWorkflowsBatchesPerRowQueries is the CC.9 regression for the workflow
// list: Disabled, Last Run (timestamp + status) and Next Run must be derived
// from the batched page query + one schedules query, not three lookups per row.
func TestListWorkflowsBatchesPerRowQueries(t *testing.T) {
	pool := countingPool(t)
	s := &Server{
		cfg: &config.Config{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  pool,
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }

	const n = 40
	for i := range n {
		name := fmt.Sprintf("wf-%03d", i)
		if _, err := pool.Exec(`INSERT INTO workflows(name, source, steps, enabled, synced_at, created_at)
			VALUES (?, 'git', '[]', 1, ?, ?)`, name, stamp(0), stamp(0)); err != nil {
			t.Fatalf("seed workflow %s: %v", name, err)
		}
		// An older failure then the latest success: the list must report the
		// latest run's timestamp and status.
		if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at)
			VALUES (?, ?, 'git', 'failure', 'test', 'manual', ?)`, fmt.Sprintf("wr-old-%03d", i), name, stamp(-time.Hour)); err != nil {
			t.Fatalf("seed old workflow_run %s: %v", name, err)
		}
		if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at)
			VALUES (?, ?, 'git', 'success', 'test', 'manual', ?)`, fmt.Sprintf("wr-%03d", i), name, stamp(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed workflow_run %s: %v", name, err)
		}
		if i%5 == 0 { // every 5th paused
			if _, err := pool.Exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at)
				VALUES ('git', 'workflow', ?, 'test', ?)`, name, stamp(0)); err != nil {
				t.Fatalf("seed paused %s: %v", name, err)
			}
		}
		if i%3 == 0 { // every 3rd scheduled
			if _, err := pool.Exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
				VALUES ('git', 'workflow', ?, 'default', '0 * * * *', 0)`, name); err != nil {
				t.Fatalf("seed schedule %s: %v", name, err)
			}
		}
	}

	type wfItem struct {
		Name      string  `json:"name"`
		Status    string  `json:"status"`
		Disabled  bool    `json:"disabled"`
		LastRunAt *string `json:"lastRunAt"`
		NextRunAt *string `json:"nextRunAt"`
	}
	type envelope struct {
		TotalItems int      `json:"totalItems"`
		Items      []wfItem `json:"items"`
	}

	call := func(pageSize int) (envelope, int64) {
		jobsListQueryCount.Store(0)
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/workflows?pageSize=%d&page=1", pageSize), nil)
		w := httptest.NewRecorder()
		s.listWorkflows(w, req)
		if w.Code != 200 {
			t.Fatalf("listWorkflows = %d, body: %s", w.Code, w.Body.String())
		}
		var env envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env, jobsListQueryCount.Load()
	}

	small, qSmall := call(1)
	full, qFull := call(30)
	if small.TotalItems != n || full.TotalItems != n {
		t.Fatalf("totalItems = %d/%d, want %d", small.TotalItems, full.TotalItems, n)
	}
	if qFull > qSmall {
		t.Errorf("workflow list query count grew with page size: %d → %d (per-row N+1 regressed)", qSmall, qFull)
	}
	if qFull > 4 {
		t.Errorf("workflow list issued %d queries for a full page, want a small constant (batched)", qFull)
	}

	for _, it := range full.Items {
		var i int
		if _, err := fmt.Sscanf(it.Name, "wf-%03d", &i); err != nil {
			t.Fatalf("unexpected name %q", it.Name)
		}
		wantDisabled := i%5 == 0
		if it.Disabled != wantDisabled {
			t.Errorf("%s disabled = %v, want %v", it.Name, it.Disabled, wantDisabled)
		}
		if it.Status != "success" {
			t.Errorf("%s status = %q, want success (latest run wins over older failure)", it.Name, it.Status)
		}
		wantAt := stamp(time.Duration(i) * time.Minute)
		if it.LastRunAt == nil || *it.LastRunAt != wantAt {
			t.Errorf("%s lastRunAt = %v, want %s (latest run)", it.Name, it.LastRunAt, wantAt)
		}
		// Scheduled AND active ⇒ a projected next run; paused ⇒ nil even when scheduled.
		if scheduled := i%3 == 0; scheduled && !wantDisabled {
			if it.NextRunAt == nil {
				t.Errorf("%s nextRunAt = nil, want a projected time (scheduled, active)", it.Name)
			}
		} else if it.NextRunAt != nil {
			t.Errorf("%s nextRunAt = %v, want nil (unscheduled or paused)", it.Name, *it.NextRunAt)
		}
	}
}

// TestListSchedulesBatchesPerEntryQueries is the CC.9 regression for the
// schedule inventory: enabled/paused/lastRunAt must come from a fixed set of
// preload queries, not three lookups per entry. It also pins the batched
// last-run rule that the 'default' entry absorbs legacy NULL-schedule runs.
func TestListSchedulesBatchesPerEntryQueries(t *testing.T) {
	pool := countingPool(t)
	s := &Server{
		cfg: &config.Config{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  pool,
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }

	// job-a: enabled, active, two schedules (default + nightly).
	mustExec(t, pool, `INSERT INTO jobs(name, source, run_type, command, enabled, content_hash, synced_at)
		VALUES ('job-a', 'git', 'bash', 'echo hi', 1, 'h', ?)`, stamp(0))
	mustExec(t, pool, `INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
		VALUES ('git','job','job-a','default','0 * * * *',0)`)
	mustExec(t, pool, `INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
		VALUES ('git','job','job-a','nightly','0 0 * * *',1)`)
	// Runs: nightly@+10, NULL-schedule@+5, explicit default@+1. The default entry
	// must resolve to the NEWEST of {default, NULL} = +5; nightly to +10.
	mustExec(t, pool, `INSERT INTO runs(id, job_name, job_source, run_type, status, schedule_name, triggered_by, trigger_kind, executor, created_at)
		VALUES ('ra1','job-a','git','bash','success','nightly','test','scheduled','ssh',?)`, stamp(10*time.Minute))
	mustExec(t, pool, `INSERT INTO runs(id, job_name, job_source, run_type, status, schedule_name, triggered_by, trigger_kind, executor, created_at)
		VALUES ('ra2','job-a','git','bash','success',NULL,'test','manual','ssh',?)`, stamp(5*time.Minute))
	mustExec(t, pool, `INSERT INTO runs(id, job_name, job_source, run_type, status, schedule_name, triggered_by, trigger_kind, executor, created_at)
		VALUES ('ra3','job-a','git','bash','success','default','test','scheduled','ssh',?)`, stamp(time.Minute))

	// job-b: disabled, one schedule, no runs.
	mustExec(t, pool, `INSERT INTO jobs(name, source, run_type, command, enabled, content_hash, synced_at)
		VALUES ('job-b', 'git', 'bash', 'echo hi', 0, 'h', ?)`, stamp(0))
	mustExec(t, pool, `INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
		VALUES ('git','job','job-b','default','0 * * * *',0)`)

	// wf-x: enabled but paused, one schedule, one workflow run.
	mustExec(t, pool, `INSERT INTO workflows(name, source, steps, enabled, synced_at) VALUES ('wf-x','git','[]',1,?)`, stamp(0))
	mustExec(t, pool, `INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at) VALUES ('git','workflow','wf-x','test',?)`, stamp(0))
	mustExec(t, pool, `INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
		VALUES ('git','workflow','wf-x','default','0 * * * *',0)`)
	mustExec(t, pool, `INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, schedule_name, triggered_by, trigger_kind, created_at)
		VALUES ('wx1','wf-x','git','success','default','test','scheduled',?)`, stamp(3*time.Minute))

	type schedItem struct {
		OwnerKind    string  `json:"ownerKind"`
		OwnerName    string  `json:"ownerName"`
		ScheduleName string  `json:"scheduleName"`
		Enabled      bool    `json:"enabled"`
		Paused       bool    `json:"paused"`
		NextRunAt    *string `json:"nextRunAt"`
		LastRunAt    *string `json:"lastRunAt"`
	}
	var env struct {
		Items []schedItem `json:"items"`
	}

	jobsListQueryCount.Store(0)
	req := httptest.NewRequest("GET", "/api/v1/schedules", nil)
	w := httptest.NewRecorder()
	s.listSchedules(w, req)
	if w.Code != 200 {
		t.Fatalf("listSchedules = %d, body: %s", w.Code, w.Body.String())
	}
	q := jobsListQueryCount.Load()
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// 4 entries but a fixed query budget: drain + 2 enabled + 1 paused + 2 last-run.
	// The old shape issued 3 per entry (would be 13 here).
	if q > 8 {
		t.Errorf("listSchedules issued %d queries for 4 entries, want a small constant (batched)", q)
	}
	if len(env.Items) != 4 {
		t.Fatalf("got %d schedule items, want 4", len(env.Items))
	}

	get := func(owner, sched string) schedItem {
		for _, it := range env.Items {
			if it.OwnerName == owner && it.ScheduleName == sched {
				return it
			}
		}
		t.Fatalf("no schedule item for %s/%s", owner, sched)
		return schedItem{}
	}

	// job-a/default: enabled+active ⇒ nextRunAt set; lastRunAt = newest of the
	// explicit-default and NULL-schedule runs (+5, not the older +1).
	if it := get("job-a", "default"); !it.Enabled || it.Paused || it.NextRunAt == nil ||
		it.LastRunAt == nil || *it.LastRunAt != stamp(5*time.Minute) {
		t.Errorf("job-a/default = %+v; want enabled, active, nextRunAt set, lastRunAt %s", it, stamp(5*time.Minute))
	}
	// job-a/nightly: lastRunAt = the nightly run (+10).
	if it := get("job-a", "nightly"); it.LastRunAt == nil || *it.LastRunAt != stamp(10*time.Minute) {
		t.Errorf("job-a/nightly lastRunAt = %v, want %s", it.LastRunAt, stamp(10*time.Minute))
	}
	// job-b/default: disabled ⇒ no next run, no last run.
	if it := get("job-b", "default"); it.Enabled || it.NextRunAt != nil || it.LastRunAt != nil {
		t.Errorf("job-b/default = %+v; want disabled, no next/last run", it)
	}
	// wf-x/default: enabled but paused ⇒ no next run; last run from workflow_runs (+3).
	if it := get("wf-x", "default"); !it.Enabled || !it.Paused || it.NextRunAt != nil ||
		it.LastRunAt == nil || *it.LastRunAt != stamp(3*time.Minute) {
		t.Errorf("wf-x/default = %+v; want enabled, paused, no next run, lastRunAt %s", it, stamp(3*time.Minute))
	}
}

func mustExec(t *testing.T, pool *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
