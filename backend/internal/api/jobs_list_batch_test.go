package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// --- query-counting driver ------------------------------------------------
//
// A thin wrapper over the mattn sqlite3 driver that counts every SELECT
// (QueryContext) reaching the database. It is used by the CC.8 regression to
// prove the jobs list issues a fixed number of queries per request rather than
// scaling with the number of rows returned (the old per-row N+1 shape).

var (
	jobsListQueryCount atomic.Int64
	jobsListDriverOnce sync.Once
)

type countingDriver struct{ base driver.Driver }

func (d countingDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.base.Open(dsn)
	if err != nil {
		return nil, err
	}
	return countingConn{c}, nil
}

type countingConn struct{ base driver.Conn }

func (c countingConn) Prepare(q string) (driver.Stmt, error) { return c.base.Prepare(q) }
func (c countingConn) Close() error                          { return c.base.Close() }
func (c countingConn) Begin() (driver.Tx, error)             { return c.base.Begin() }

func (c countingConn) BeginTx(ctx context.Context, o driver.TxOptions) (driver.Tx, error) {
	return c.base.(driver.ConnBeginTx).BeginTx(ctx, o)
}

func (c countingConn) QueryContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Rows, error) {
	jobsListQueryCount.Add(1)
	return c.base.(driver.QueryerContext).QueryContext(ctx, q, a)
}

func (c countingConn) ExecContext(ctx context.Context, q string, a []driver.NamedValue) (driver.Result, error) {
	return c.base.(driver.ExecerContext).ExecContext(ctx, q, a)
}

// countingPool opens a migrated database on the query-counting driver, using
// the same DSN pragmas as db.Open so migrations and semantics match production.
func countingPool(t *testing.T) *sql.DB {
	t.Helper()
	jobsListDriverOnce.Do(func() {
		sql.Register("sqlite3-jobcount", countingDriver{base: &sqlite3.SQLiteDriver{}})
	})
	path := filepath.Join(t.TempDir(), "cc8.db")
	dsn := "file:" + url.PathEscape(path) + "?" + (url.Values{
		"_journal_mode": {"WAL"},
		"_busy_timeout": {"5000"},
		"_foreign_keys": {"on"},
		"_synchronous":  {"NORMAL"},
		"_txlock":       {"immediate"},
	}).Encode()
	pool, err := sql.Open("sqlite3-jobcount", dsn)
	if err != nil {
		t.Fatalf("open counting db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	pool.SetMaxOpenConns(1) // funnel every query through one counted connection
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestListJobsBatchesPerRowQueries is the CC.8 regression: the jobs list must
// fold the latest-run lookup and the schedule-entry COUNT into the page query
// instead of issuing them per row. It asserts (a) the derived list-row fields
// are correct and (b) the query count does not grow with page size.
func TestListJobsBatchesPerRowQueries(t *testing.T) {
	pool := countingPool(t)
	s := &Server{
		cfg: &config.Config{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  pool,
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339) }

	const n = 60
	for i := range n {
		name := fmt.Sprintf("j-%03d", i)
		if _, err := pool.Exec(`
			INSERT INTO jobs(name, source, run_type, command, enabled, content_hash, synced_at)
			VALUES (?, 'git', 'bash', 'echo hi', 1, 'h', ?)`, name, stamp(0)); err != nil {
			t.Fatalf("seed job %s: %v", name, err)
		}
		// An older run, then the latest run: the list must report the latest
		// run's timestamp/duration/status, not the older one's.
		if _, err := pool.Exec(`
			INSERT INTO runs(id, job_name, job_source, run_type, status, duration_ms, triggered_by, trigger_kind, executor, created_at)
			VALUES (?, ?, 'git', 'bash', 'failure', 999, 'test', 'manual', 'ssh', ?)`,
			fmt.Sprintf("r-old-%03d", i), name, stamp(-time.Hour)); err != nil {
			t.Fatalf("seed old run %s: %v", name, err)
		}
		if _, err := pool.Exec(`
			INSERT INTO runs(id, job_name, job_source, run_type, status, duration_ms, triggered_by, trigger_kind, executor, created_at)
			VALUES (?, ?, 'git', 'bash', 'success', ?, 'test', 'manual', 'ssh', ?)`,
			fmt.Sprintf("r-%03d", i), name, int64(1000+i), stamp(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("seed run %s: %v", name, err)
		}
		// scheduleCount cycles 0,1,2 so the "+N" badge count is exercised.
		for k := 0; k < i%3; k++ {
			if _, err := pool.Exec(`
				INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
				VALUES ('git', 'job', ?, ?, '0 * * * *', ?)`,
				name, fmt.Sprintf("s-%d", k), k); err != nil {
				t.Fatalf("seed schedule %s/%d: %v", name, k, err)
			}
		}
	}

	type jobItem struct {
		Name           string  `json:"name"`
		Status         string  `json:"status"`
		LastRunAt      *string `json:"lastRunAt"`
		LastDurationMs *int64  `json:"lastDurationMs"`
		ScheduleCount  int     `json:"scheduleCount"`
	}
	type envelope struct {
		TotalItems int       `json:"totalItems"`
		Items      []jobItem `json:"items"`
	}

	call := func(pageSize int) (envelope, int64) {
		jobsListQueryCount.Store(0)
		req := httptest.NewRequest("GET", fmt.Sprintf("/api/v1/jobs?pageSize=%d&page=1", pageSize), nil)
		w := httptest.NewRecorder()
		s.listJobs(w, req)
		if w.Code != 200 {
			t.Fatalf("listJobs = %d, body: %s", w.Code, w.Body.String())
		}
		var env envelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return env, jobsListQueryCount.Load()
	}

	small, qSmall := call(1)
	full, qFull := call(50)

	if small.TotalItems != n || full.TotalItems != n {
		t.Fatalf("totalItems = %d/%d, want %d", small.TotalItems, full.TotalItems, n)
	}
	if len(full.Items) != 50 {
		t.Fatalf("full page len = %d, want 50", len(full.Items))
	}

	// (a) The query count must NOT scale with the number of rows returned. A
	// per-row N+1 would make qFull ≈ qSmall + 2*49; batched, they are equal.
	if qFull > qSmall {
		t.Errorf("query count grew with page size: pageSize=1 issued %d, pageSize=50 issued %d (per-row N+1 regressed)", qSmall, qFull)
	}
	if qFull > 4 {
		t.Errorf("jobs list issued %d queries for a 50-row page, want a small constant (batched)", qFull)
	}

	// (b) Derived list-row fields come from the batched query and match the
	// latest run + schedule-entry count for each job.
	for _, it := range full.Items {
		var i int
		if _, err := fmt.Sscanf(it.Name, "j-%03d", &i); err != nil {
			t.Fatalf("unexpected name %q", it.Name)
		}
		if it.Status != "success" {
			t.Errorf("%s status = %q, want success (latest run wins over older failure)", it.Name, it.Status)
		}
		if it.LastDurationMs == nil || *it.LastDurationMs != int64(1000+i) {
			t.Errorf("%s lastDurationMs = %v, want %d (latest run)", it.Name, it.LastDurationMs, 1000+i)
		}
		wantAt := stamp(time.Duration(i) * time.Minute)
		if it.LastRunAt == nil || *it.LastRunAt != wantAt {
			t.Errorf("%s lastRunAt = %v, want %s (latest run)", it.Name, it.LastRunAt, wantAt)
		}
		if it.ScheduleCount != i%3 {
			t.Errorf("%s scheduleCount = %d, want %d", it.Name, it.ScheduleCount, i%3)
		}
	}
}
