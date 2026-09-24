package scheduler

import (
	"context"
	"database/sql"
	"testing"
)

// SL-A — pause and cap suppressions must leave an audit row.
//
// CAL-6 gave calendar vetoes a durable record; the two OTHER reasons a
// scheduled fire silently doesn't happen kept returning with only a log line.
// That is the gap the missed-run detector (PF-3/SL) trips over: at T+grace it
// asks "the schedule expected a fire — is there a row?", and a paused or capped
// job answered exactly like a scheduler that had died. These tests pin that the
// row exists, says which reason, and does not storm.
//
// The de-dupe is the load-bearing half. dedupeEpisode keys on concurrency_key,
// which PP-L8 assigns only to Forbid runs, so these key-less suppressions need
// dedupeEpisodeReason — and it must stay independent of the Forbid and calendar
// rules in both directions (the CAL-8 lesson).

func pauseDef(t *testing.T, pool *sql.DB, source, kind, name string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at) VALUES (?, ?, ?, 'operator', '2026-08-11T00:00:00Z')`,
		source, kind, name); err != nil {
		t.Fatalf("pause %s/%s: %v", kind, name, err)
	}
}

func setMaxConcurrent(t *testing.T, pool *sql.DB, n string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, settingsKeyMax, n); err != nil {
		t.Fatalf("set maxConcurrent: %v", err)
	}
}

// seedActiveRun occupies a concurrency slot so the cap gate trips.
func seedActiveRun(t *testing.T, pool *sql.DB, id, job string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
		 VALUES (?, ?, 'git', 'bash', 'running', 'scheduler', 'scheduled', '2026-08-11T00:00:00Z')`, id, job); err != nil {
		t.Fatalf("seed active run: %v", err)
	}
}

func skippedRows(t *testing.T, pool *sql.DB, job string) []string {
	t.Helper()
	rows, err := pool.QueryContext(context.Background(),
		`SELECT COALESCE(queued_reason,'') FROM runs WHERE job_name = ? AND status = 'skipped'
		 ORDER BY created_at, rowid`, job)
	if err != nil {
		t.Fatalf("read skipped rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// A paused job's fire records one skip — and does not record a second one on the
// next tick, or a `* * * * *` entry would write ~1,440 rows a day.
func TestFirePausedRecordsOncePerEpisode(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	pauseDef(t, pool, "git", "job", "patch")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")
	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")
	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Errorf("paused job enqueued %d runs, want 0", n)
	}
	got := skippedRows(t, pool, "patch")
	if len(got) != 1 {
		t.Fatalf("recorded %d skips across three fires, want 1: %v", len(got), got)
	}
	if got[0] != reasonJobPaused {
		t.Errorf("queued_reason = %q, want %q", got[0], reasonJobPaused)
	}

	// The row must carry the entry that didn't fire — the detector joins on it.
	var schedName sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT schedule_name FROM runs WHERE job_name='patch' AND status='skipped'`).Scan(&schedName); err != nil {
		t.Fatalf("fetch skipped run: %v", err)
	}
	if schedName.String != "nightly" {
		t.Errorf("schedule_name = %q, want nightly", schedName.String)
	}
}

// The cap gate records too, and its reason is distinguishable from pause — the
// detector reports "expected, capped" differently from "expected, paused".
func TestFireCappedRecordsWithItsOwnReason(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	setMaxConcurrent(t, pool, "1")
	seedActiveRun(t, pool, "run-holding-the-slot", "other")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")
	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	got := skippedRows(t, pool, "patch")
	if len(got) != 1 {
		t.Fatalf("recorded %d skips across two capped fires, want 1: %v", len(got), got)
	}
	if got[0] != reasonConcurrencyCap {
		t.Errorf("queued_reason = %q, want %q", got[0], reasonConcurrencyCap)
	}
}

// Two DIFFERENT reasons must not collapse into each other: a pause skip standing
// in for a cap skip is exactly the CAL-8 leak, one level down.
func TestPauseAndCapSkipsDoNotSwallowEachOther(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	setMaxConcurrent(t, pool, "1")
	seedActiveRun(t, pool, "run-holding-the-slot", "other")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "") // capped
	pauseDef(t, pool, "git", "job", "patch")
	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "") // paused

	got := skippedRows(t, pool, "patch")
	if len(got) != 2 {
		t.Fatalf("recorded %d skips, want 2 (one per reason): %v", len(got), got)
	}
	if got[0] != reasonConcurrencyCap || got[1] != reasonJobPaused {
		t.Errorf("reasons = %v, want [%q %q]", got, reasonConcurrencyCap, reasonJobPaused)
	}
}

// A cap skip must not be visible to the Forbid de-dupe. Forbid keys on
// concurrency_key and these rows deliberately carry none, so a Forbid
// suppression following a cap suppression still records.
func TestCapSkipDoesNotSwallowAForbidSkip(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	setMaxConcurrent(t, pool, "1")
	seedActiveRun(t, pool, "run-holding-the-slot", "other")

	// Capped first: records a key-less skip.
	s.fire("git", "patch", "", "bash", "prod", "Forbid", "git/patch", "nightly", "")

	// Now raise the cap so the Forbid gate is what suppresses, with an active run
	// under the job's own key.
	setMaxConcurrent(t, pool, "50")
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, job_source, run_type, status, concurrency_key, triggered_by, trigger_kind, created_at)
		 VALUES ('active-forbid', 'patch', 'git', 'bash', 'running', 'git/patch', 'scheduler', 'scheduled', '2026-08-11T00:01:00Z')`); err != nil {
		t.Fatalf("seed forbid holder: %v", err)
	}
	s.fire("git", "patch", "", "bash", "prod", "Forbid", "git/patch", "nightly", "")

	got := skippedRows(t, pool, "patch")
	if len(got) != 2 {
		t.Fatalf("recorded %d skips, want 2 — a key-less cap skip must not stand in for the Forbid episode: %v", len(got), got)
	}
	if got[1] == reasonConcurrencyCap {
		t.Errorf("second skip = %q, want the Forbid reason", got[1])
	}
}

// The key-less cap/pause rows must not carry a concurrency key, which is what
// keeps them invisible to the Forbid rule.
func TestPauseSkipCarriesNoConcurrencyKey(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	pauseDef(t, pool, "git", "job", "patch")
	s.fire("git", "patch", "", "bash", "prod", "Forbid", "git/patch", "nightly", "")

	var key sql.NullString
	if err := pool.QueryRowContext(context.Background(),
		`SELECT concurrency_key FROM runs WHERE job_name='patch' AND status='skipped'`).Scan(&key); err != nil {
		t.Fatalf("fetch skipped run: %v", err)
	}
	if key.Valid {
		t.Errorf("concurrency_key = %q, want NULL — PP-L8 assigns it only to Forbid runs", key.String)
	}
}

// Once a real run lands, the episode is over: the next suppression records again
// rather than being collapsed into the previous one forever.
func TestCapSkipRecordsAgainAfterASuccessfulRun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "patch", 1)
	setMaxConcurrent(t, pool, "1")
	seedActiveRun(t, pool, "holder-1", "other")

	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	// The cap clears and the job runs to completion.
	if _, err := pool.ExecContext(context.Background(),
		`UPDATE runs SET status='success' WHERE id='holder-1'`); err != nil {
		t.Fatalf("clear holder: %v", err)
	}
	// created_at must sort AFTER the skip row above, which recordSkippedFire
	// stamped at wall-clock now — the episode rule reads the most recent row.
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO runs (id, job_name, job_source, run_type, status, schedule_name, triggered_by, trigger_kind, created_at)
		 VALUES ('real-run', 'patch', 'git', 'bash', 'success', 'nightly', 'scheduler', 'scheduled',
		         strftime('%Y-%m-%dT%H:%M:%SZ','now','+1 minute'))`); err != nil {
		t.Fatalf("seed real run: %v", err)
	}

	// Cap trips again — a NEW episode.
	seedActiveRun(t, pool, "holder-2", "other")
	s.fire("git", "patch", "", "bash", "prod", "Allow", "", "nightly", "")

	if got := skippedRows(t, pool, "patch"); len(got) != 2 {
		t.Fatalf("recorded %d skips, want 2 — a run in between ends the episode: %v", len(got), got)
	}
}

// ─── The workflow half ────────────────────────────────────────────────────────

func workflowSkippedRows(t *testing.T, pool *sql.DB, wf string) []string {
	t.Helper()
	rows, err := pool.QueryContext(context.Background(),
		`SELECT COALESCE(queued_reason,'') FROM workflow_runs WHERE workflow_name = ? AND status = 'skipped'
		 ORDER BY created_at, rowid`, wf)
	if err != nil {
		t.Fatalf("read skipped workflow rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// CAL-32's header noted fireWorkflow "dropped even its concurrency-cap skips
// with a log line". Both workflow gates now record.
func TestFireWorkflowPausedAndCappedRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wf         string
		setup      func(t *testing.T, pool *sql.DB)
		wantReason string
	}{
		{
			name: "paused", wf: "nightly-wf",
			setup:      func(t *testing.T, pool *sql.DB) { pauseDef(t, pool, "git", "workflow", "nightly-wf") },
			wantReason: reasonWorkflowPaused,
		},
		{
			name: "capped", wf: "capped-wf",
			setup: func(t *testing.T, pool *sql.DB) {
				setMaxConcurrent(t, pool, "1")
				seedActiveRun(t, pool, "holder", "other")
			},
			wantReason: reasonConcurrencyCap,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := mustPool(t)
			s := New(pool, quietLog(), nil)
			fired := 0
			s.SetWorkflowFirer(func(context.Context, string, string, string, string) { fired++ })
			tc.setup(t, pool)

			s.fireWorkflow("git", tc.wf, "nightly", "")
			s.fireWorkflow("git", tc.wf, "nightly", "")

			if fired != 0 {
				t.Errorf("suppressed workflow fired %d times, want 0", fired)
			}
			got := workflowSkippedRows(t, pool, tc.wf)
			if len(got) != 1 {
				t.Fatalf("recorded %d workflow skips across two fires, want 1: %v", len(got), got)
			}
			if got[0] != tc.wantReason {
				t.Errorf("queued_reason = %q, want %q", got[0], tc.wantReason)
			}
		})
	}
}

// ─── RH: a binned definition must not fire ───────────────────────────────────
//
// This is the sharpest edge in the recycle-bin design. The soft delete is an
// UPDATE, so the AFTER DELETE triggers never fire and the definition KEEPS its
// definition_schedules rows — deliberately, because that is what makes an
// undelete lossless. Nothing about the data therefore stops the scheduler from
// registering the entry; only reloadJobs' deleted_at filter does. Drop it and a
// deleted job keeps running at 2am with nothing in the UI to explain why.

func TestReloadSkipsSoftDeletedDefinitions(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)

	seedJobRow(t, pool, "live-job", 1)
	seedJobRow(t, pool, "binned-job", 1)
	seedSchedule(t, pool, "job", "live-job", "default", "0 2 * * *", 0)
	seedSchedule(t, pool, "job", "binned-job", "default", "0 3 * * *", 0)

	if _, err := pool.ExecContext(ctx,
		`INSERT INTO workflows(name, source, steps, enabled, synced_at) VALUES('binned-wf','git','[]',1,'t')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}
	seedSchedule(t, pool, "workflow", "binned-wf", "default", "0 4 * * *", 0)

	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := len(s.cr.Entries()); n != 3 {
		t.Fatalf("registered %d entries before the delete, want 3", n)
	}

	// Bin one job and one workflow — WITHOUT touching their bindings, exactly as
	// the soft delete does.
	if _, err := pool.ExecContext(ctx,
		`UPDATE jobs SET deleted_at='2026-08-11T00:00:00Z' WHERE name='binned-job'`); err != nil {
		t.Fatalf("bin job: %v", err)
	}
	if _, err := pool.ExecContext(ctx,
		`UPDATE workflows SET deleted_at='2026-08-11T00:00:00Z' WHERE name='binned-wf'`); err != nil {
		t.Fatalf("bin workflow: %v", err)
	}
	var stillBound int
	_ = pool.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM definition_schedules WHERE owner_name IN ('binned-job','binned-wf')`).Scan(&stillBound)
	if stillBound != 2 {
		t.Fatalf("the test's premise is gone: bindings = %d, want 2 (a soft delete must not cascade)", stillBound)
	}

	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload after bin: %v", err)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Errorf("registered %d entries after binning two definitions, want 1 — "+
			"their bindings still exist, so only the deleted_at filter can retire them", n)
	}

	// Restoring puts it straight back, with no binding rebuild needed.
	if _, err := pool.ExecContext(ctx, `UPDATE jobs SET deleted_at=NULL WHERE name='binned-job'`); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload after restore: %v", err)
	}
	if n := len(s.cr.Entries()); n != 2 {
		t.Errorf("registered %d entries after restoring one job, want 2", n)
	}
}
