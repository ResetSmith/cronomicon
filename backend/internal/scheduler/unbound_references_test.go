package scheduler

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runref"
)

// RA-24 on the SCHEDULED path (the runas-update plan §11.1b).
//
// This is the case that actually matters under the operations-team model (§13):
// admins wire jobs and SCHEDULE them; the ad-hoc trigger is the exception. A cron
// fire of an unscoped, credential-consuming job resolves nothing and is claimable
// by no departmental runner — with no human watching and no 422 to hand anyone.
//
// The plan's rev-6 fix specified only "the trigger boundary". It would have guarded
// the exception and missed the rule. That is why RA-Q20 widened it to every run
// path, and why this test exists at all.

func unboundFireDB(t *testing.T) (*sql.DB, func(string, ...any)) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "unbound_fire.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-tax','Tax','t')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'unscoped-job', 'unscoped-job','git','bash','Allow','t')`)
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-owned','DEPT_PASSWORD','stored','ag-tax','t')`)
	exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('s-owned','ag-tax')`)
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-global','SHARED_TOKEN','stored','','t')`)
	return pool, exec
}

func fireUnscoped(t *testing.T, pool *sql.DB) {
	t.Helper()
	s := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s.fire("git", "unscoped-job", "", "bash", "", "Allow", "unscoped-job", "nightly", "")
}

// TestScheduledUnboundFireWithOwnedCredentialIsRecordedNotEnqueued — the fire is
// refused, but VISIBLY: a terminal skipped run carrying the reason, not a log line.
// A fire that vanishes into the log is a fire nobody knows didn't happen, which is
// the same class of invisibility RA-24 exists to end.
func TestScheduledUnboundFireWithOwnedCredentialIsRecordedNotEnqueued(t *testing.T) {
	pool, exec := unboundFireDB(t)
	exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	      VALUES('job','git','unscoped-job','secret','DEPT_PASSWORD','t')`)

	fireUnscoped(t, pool)

	ctx := context.Background()
	var status, reason string
	if err := pool.QueryRowContext(ctx,
		`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name='unscoped-job'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("no run row recorded — the fire vanished silently: %v", err)
	}
	if status != "skipped" {
		t.Errorf("status = %q, want skipped — a doomed run must not be enqueued", status)
	}
	if reason != runref.QueuedReasonUnboundReferences {
		t.Errorf("queued_reason = %q, want %q", reason, runref.QueuedReasonUnboundReferences)
	}

	var queued int
	_ = pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE status='queued'`).Scan(&queued)
	if queued != 0 {
		t.Errorf("queued runs = %d, want 0 — the point is not to park a run nothing can claim", queued)
	}
}

// TestScheduledUnboundFireWithGlobalCredentialStillEnqueues — precision, same as
// the trigger side. A job binding only global rows has always fired unbound and
// must keep doing so; over-refusing here would silently stop working cron jobs,
// which is a worse outcome than the failure being prevented.
func TestScheduledUnboundFireWithGlobalCredentialStillEnqueues(t *testing.T) {
	pool, exec := unboundFireDB(t)
	exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	      VALUES('job','git','unscoped-job','secret','SHARED_TOKEN','t')`)

	fireUnscoped(t, pool)

	var status string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT status FROM runs WHERE job_name='unscoped-job'`).Scan(&status); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued — a global row resolves for everyone", status)
	}
}

// TestScheduledFireWithNoBindingsIsUntouched — the overwhelming majority of jobs.
// The guard must be invisible to them, or it is a tax on every fire in the system.
func TestScheduledFireWithNoBindingsIsUntouched(t *testing.T) {
	pool, _ := unboundFireDB(t)

	fireUnscoped(t, pool)

	var status string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT status FROM runs WHERE job_name='unscoped-job'`).Scan(&status); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued — a job with no bindings has nothing to resolve", status)
	}
}
