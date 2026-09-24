package runref

import (
	"context"
	"testing"
)

// TestAuditReleaseOnWriteFailure (M6): when the change_log write fails, the
// one-time slot must be RELEASED (injection_audited back to 0) so a later fetch
// re-attempts — never left flagged-audited with no row. The release runs on a
// cancellation-proof context so a disconnected request cannot wedge it.
func TestAuditReleaseOnWriteFailure(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES('run-rel','j','bash','running','ops@x','manual','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Force the audit write to fail: drop the table WriteChangeLog inserts into.
	if _, err := pool.Exec(`DROP TABLE change_log`); err != nil {
		t.Fatalf("drop change_log: %v", err)
	}
	resolved := &Resolved{Refs: []ResolvedRef{{Kind: KindSecret, Name: "DB_PASS", Source: "stored"}}}

	err := AuditInjectionOnce(ctx, pool, nilLog(), "run-rel", "ops@x", "prod", resolved)
	if err == nil {
		t.Fatal("expected the audit write to fail (change_log dropped)")
	}
	// The slot must have been released despite the write failure.
	var audited bool
	if qerr := pool.QueryRow(`SELECT injection_audited FROM runs WHERE id='run-rel'`).Scan(&audited); qerr != nil {
		t.Fatalf("read slot: %v", qerr)
	}
	if audited {
		t.Error("injection_audited still set, want released (0) after a failed write")
	}
}

// TestAuditCompareAndRepair (M6 belt-and-suspenders): a slot wedged at 1 with NO
// change_log row (a prior release that itself failed) must SELF-HEAL — the next
// AuditInjectionOnce sees the missing row and (re)writes it rather than trusting
// the flag and shipping secrets untraced forever.
func TestAuditCompareAndRepair(t *testing.T) {
	pool := openDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
		VALUES('run-wedge','j','bash','running','ops@x','manual','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	// Simulate the wedge: slot claimed, but no audit row was ever written.
	if _, err := pool.Exec(`UPDATE runs SET injection_audited=1 WHERE id='run-wedge'`); err != nil {
		t.Fatalf("wedge slot: %v", err)
	}
	resolved := &Resolved{Refs: []ResolvedRef{{Kind: KindSecret, Name: "DB_PASS", Source: "stored"}}}

	if err := AuditInjectionOnce(ctx, pool, nilLog(), "run-wedge", "ops@x", "prod", resolved); err != nil {
		t.Fatalf("compare-and-repair should succeed: %v", err)
	}
	// The missing audit row is now present.
	var n int
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-wedge'`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit row count = %d, want 1 (wedged slot self-healed)", n)
	}

	// A subsequent call is now a genuine no-op (row present, slot stays 1) — no dup.
	if err := AuditInjectionOnce(ctx, pool, nilLog(), "run-wedge", "ops@x", "prod", resolved); err != nil {
		t.Fatalf("second call: %v", err)
	}
	_ = pool.QueryRow(
		`SELECT COUNT(*) FROM change_log WHERE category='Secrets' AND action='injected' AND target='run-wedge'`).Scan(&n)
	if n != 1 {
		t.Errorf("audit row count after no-op re-call = %d, want 1 (no duplicate)", n)
	}
}
