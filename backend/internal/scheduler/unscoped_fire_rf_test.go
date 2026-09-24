package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// RF-12 — RB-Q11(c), asserted as behavior (the RBAC-fixes plan).
//
// RB-26 requires an interactive caller to BIND a scope before triggering an
// unscoped job. A cron fire has no caller — TriggerKind "scheduled", TriggeredBy
// "scheduler" — so RB-Q11(c) resolved that scheduled runs are SYSTEM runs: they
// stay unbound, reach only the general pool, and no scope check happens because
// there is no actor whose grants could be evaluated.
//
// This is deliberately the one execution path v0.56.4 did NOT tighten, and it
// looks exactly like the hole RB-26 closed everywhere else. RB-30 already fences
// the safety ARGUMENT (schedule authoring is admin-only, and
// TestScheduleDefsAdminOnly_GuardsUnscopedSchedulingBypass fails loudly if that
// changes). This test fences the BEHAVIOR, so that "fixing" the scheduler to
// demand a bound scope has to argue with a test that states the decision — the
// same treatment RB-Q12 gets in frozen_authorization_rf_test.go.
//
// If per-entry schedule scope (option b′, §10 of the parent plan) is ever built,
// THIS is the test that should change, and changing it should be a deliberate act.
func TestScheduledFireOfAnUnscopedJobStaysUnbound(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "unscoped_fire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	// An unscoped job, and an agency-bound scope that exists but has nothing to do
	// with it — present so a fire that wrongly picked up a scope would show it.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'restart-service', 'restart-service', 'git', 'bash', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}

	s := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	// The scheduler's own call shape for an unscoped job: scope is "" because the
	// job carries none and nothing else supplies one.
	s.fire("git", "restart-service", "", "bash", "", "Allow", "restart-service", "nightly", "")

	var scope, agencies, triggerKind, triggeredBy, status string
	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(scope,''), COALESCE(agencies_json,''), trigger_kind, triggered_by, status
		FROM runs WHERE job_name = 'restart-service'`).
		Scan(&scope, &agencies, &triggerKind, &triggeredBy, &status); err != nil {
		t.Fatalf("the scheduled fire did not enqueue a run at all — RB-Q11(c) says this path is "+
			"deliberately untouched, so a refusal here is a behavior change, not a fix: %v", err)
	}

	if scope != "" {
		t.Errorf("scheduled run scope = %q, want \"\" — a cron fire has no actor to bind one, "+
			"and inventing one would be fiction (RB-Q11(c))", scope)
	}
	// The general pool, spelled as the empty ARRAY: claimRun byte-compares this
	// column against '[]', so "" or NULL would silently change dispatch.
	if agencies != "[]" {
		t.Errorf("scheduled run agencies_json = %q, want [] (the general pool)", agencies)
	}
	if triggerKind != "scheduled" || triggeredBy != "scheduler" {
		t.Errorf("trigger provenance = %q/%q, want scheduled/scheduler — this is what makes "+
			"the run identifiable as a system run rather than someone's", triggerKind, triggeredBy)
	}
	if status != "queued" {
		t.Errorf("scheduled run status = %q, want queued", status)
	}

	// And it is claimable ONLY by a general-pool runner (AG-Q3a), which is the
	// practical consequence §10 of the parent plan documents: a department's
	// agency-bound fleet cannot pick this up.
	var idx int
	if err := pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM run_agencies rag JOIN runs r ON r.id = rag.run_id
		WHERE r.job_name = 'restart-service'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 0 {
		t.Errorf("run_agencies rows for an unbound scheduled run = %d, want 0", idx)
	}
}

// TestScheduledFireOfAScopedJobStillBindsItsScope — the control. The rule above is
// about jobs with NO scope; a scheduled fire of a SCOPED job must still carry that
// scope and its agencies, or "scheduled runs are system runs" would have quietly
// become "scheduled runs ignore scope", which is a different and much larger claim.
func TestScheduledFireOfAScopedJobStillBindsItsScope(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scoped_fire.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, synced_at)VALUES ('uid-'||'tax-report', 'tax-report', 'git', 'bash', 'tax', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-tax','tax','git','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-tax','ag-tax')`); err != nil {
		t.Fatal(err)
	}

	s := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s.fire("git", "tax-report", "", "bash", "tax", "Allow", "tax-report", "nightly", "")

	var scope, agencies string
	if err := pool.QueryRowContext(ctx, `
		SELECT COALESCE(scope,''), COALESCE(agencies_json,'') FROM runs WHERE job_name = 'tax-report'`).
		Scan(&scope, &agencies); err != nil {
		t.Fatalf("scoped scheduled fire did not enqueue: %v", err)
	}
	if scope != "tax" {
		t.Errorf("scheduled run scope = %q, want tax", scope)
	}
	if agencies != `["Tax"]` {
		t.Errorf("scheduled run agencies_json = %q, want [\"Tax\"] — the scope's agencies are "+
			"snapshotted at enqueue and are what claimRun intersects against", agencies)
	}
}
