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

	// An unscoped job of an ordinary agency (Ops), and an agency-bound scope
	// that exists but has nothing to do with it — present so a fire that wrongly
	// picked up a scope would show it.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z'), (?,'Ops','2026-01-01T00:00:00Z')`, opsAgency); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, synced_at, owner_agency)VALUES ('uid-'||'restart-service', 'restart-service', 'git', 'bash', 'Allow', '2026-01-01T00:00:00Z', ?)`, opsAgency); err != nil {
		t.Fatalf("seed job: %v", err)
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
	// An unbound run is its job's agency's (2.5.3, CA-5). From migration 1220
	// until then it was Global's, whatever the job's agency — what "the general
	// pool" became when Global was given a row.
	if agencies != `["Ops"]` {
		t.Errorf("scheduled run agencies_json = %q, want [\"Ops\"] — the job's agency (CA-5)", agencies)
	}
	if triggerKind != "scheduled" || triggeredBy != "scheduler" {
		t.Errorf("trigger provenance = %q/%q, want scheduled/scheduler — this is what makes "+
			"the run identifiable as a system run rather than someone's", triggerKind, triggeredBy)
	}
	if status != "queued" {
		t.Errorf("scheduled run status = %q, want queued", status)
	}

	// And it is claimable ONLY by a runner that serves the job's agency (until
	// 2.5.3, Global), which is the practical consequence §10 of the parent plan
	// documents: another department's agency-bound fleet cannot pick this up.
	var idx int
	if err := pool.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM run_agencies rag JOIN runs r ON r.id = rag.run_id
		WHERE r.job_name = 'restart-service'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	var only string
	_ = pool.QueryRowContext(ctx, `
		SELECT rag.agency FROM run_agencies rag JOIN runs r ON r.id = rag.run_id
		WHERE r.job_name = 'restart-service'`).Scan(&only)
	if idx != 1 || only != "Ops" {
		t.Errorf("run_agencies for an unbound scheduled run = %d row(s) (%q), want the one Ops row", idx, only)
	}

	// The same unscoped job of the built-in agency (as one written with no owner
	// is born) is not a run at all since 2.5.3 (CA-1): the fire is recorded as
	// skipped, with the reason, and nothing is queued.
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, synced_at)VALUES ('uid-'||'central-restart', 'central-restart', 'git', 'bash', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	s.fire("git", "central-restart", "", "bash", "", "Allow", "central-restart", "nightly", "")
	var reason string
	if err := pool.QueryRowContext(ctx, `
		SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name = 'central-restart'`).Scan(&status, &reason); err != nil {
		t.Fatalf("the built-in agency's fire left no record: %v", err)
	}
	if status != "skipped" || reason != reasonCentralRunsNothing {
		t.Errorf("the built-in agency's unscoped fire: status/reason = %q/%q, want skipped/%q", status, reason, reasonCentralRunsNothing)
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
		INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// Tax's job, from Tax's repository: a scope's name is looked for in the job's
	// agency (I2), and a job of Global's repository on Tax's scope is stopped
	// since §0a (I1).
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-tax', 'ag-tax', 'u', 'main')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, source, run_type, scope, concurrency_policy, synced_at, repo_id)VALUES ('uid-'||'tax-report', 'tax-report', 'git', 'bash', 'tax', 'Allow', '2026-01-01T00:00:00Z', 'repo-tax')`); err != nil {
		t.Fatalf("seed job: %v", err)
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
