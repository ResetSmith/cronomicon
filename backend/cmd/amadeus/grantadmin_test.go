package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// RF-25 — the break-glass recovery, proven end to end: a database with NO grants
// at all (the lockout this subcommand exists for — under RB-15 an identity with
// no grants has no authority, so such an instance has nobody who can reach the
// Users & Access screen to fix it) is recovered by the subcommand, and the
// recovered group resolves to a working unrestricted admin THROUGH THE REAL
// RESOLVER, not by inspecting the row we just wrote.
func TestGrantAdminRecoversALockedOutDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lockout.db")
	pool, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	// The lockout: zero grants. (A fresh migrated DB has none — the 810 backfill
	// runs over empty legacy tables — which is itself the state a first OIDC
	// deployment with a miscased group lands in.)
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM access_grants`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fixture not locked out: %d grants", n)
	}
	// Sanity: before recovery, the group resolves to nothing.
	if grants, err := auth.ResolveGrants(ctx, pool, []string{"SG-Recovery"}); err != nil || len(grants) != 0 {
		t.Fatalf("pre-recovery grants = %v (err %v), want none", grants, err)
	}
	pool.Close()

	if code := runGrantAdmin([]string{"-db", dbPath, "SG-Recovery"}); code != 0 {
		t.Fatalf("grant-admin exit = %d, want 0", code)
	}

	pool, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	// The property that matters: the REAL login-path resolver now mints an
	// unrestricted admin for members of the recovered group.
	grants, err := auth.ResolveGrants(ctx, pool, []string{"SG-Recovery"})
	if err != nil {
		t.Fatalf("ResolveGrants after recovery: %v", err)
	}
	if len(grants) != 1 || grants[0].Role != "admin" || !grants[0].Unrestricted() {
		t.Fatalf("post-recovery grants = %+v, want exactly one unrestricted admin grant", grants)
	}

	// Idempotent: running it again neither errors nor stacks duplicate rows —
	// an operator mid-lockout WILL run it twice.
	if code := runGrantAdmin([]string{"-db", dbPath, "SG-Recovery"}); code != 0 {
		t.Fatalf("second grant-admin exit = %d, want 0", code)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM access_grants WHERE ad_group='SG-Recovery'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("grants after re-run = %d, want 1 (idempotent)", n)
	}

	// The audit trail exists — a break-glass grant that leaves no trace is
	// indistinguishable from an attacker's.
	if err := pool.QueryRow(
		`SELECT COUNT(*) FROM auth_events WHERE kind='bootstrap-admin' AND target='SG-Recovery'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("no auth_events row for the break-glass grant")
	}
}

// The email form is a LOOKUP, never a write: granting admin to a group grants it
// to everyone in the group, so picking which group must be a human act with the
// name in view.
func TestGrantAdminEmailFormWritesNothing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lookup.db")
	pool, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		VALUES ('alice@corp.example','Alice','["SG-Ops","SG-Staff"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	pool.Close()

	if code := runGrantAdmin([]string{"-db", dbPath, "alice@corp.example"}); code != 0 {
		t.Fatalf("email lookup exit = %d, want 0", code)
	}
	pool, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM access_grants`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("email form wrote %d grants, want 0 — it is a lookup only", n)
	}
	// An unknown email fails loudly rather than silently doing nothing.
	if code := runGrantAdmin([]string{"-db", dbPath, "nobody@corp.example"}); code == 0 {
		t.Error("unknown email exit = 0, want non-zero")
	}
}
