package gitlab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestUpsertScopes_WritesProjection verifies the M2 sync wiring: parseInventories
// builds the advisory projection and upsertScopes writes the scope_* projection
// tables + projection_status.
func TestUpsertScopes_WritesProjection(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "proj.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	clone := t.TempDir()
	invDir := filepath.Join(clone, "inventory")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeInvFile(t, filepath.Join(invDir, "prod.ini"),
		"[web]\nweb1 ansible_host=10.0.0.1\nweb2\n[web:vars]\nansible_port=22\n")
	writeInvFile(t, filepath.Join(invDir, "ranged.ini"),
		"[web]\nweb[01:50]\n") // degrades → projection_status unavailable

	svc := &Service{db: pool, cloneDir: clone}
	scopes, errs := svc.parseInventories()
	if len(errs) != 0 {
		t.Fatalf("unexpected parse errors: %v", errs)
	}

	tx, err := pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.upsertScopes(context.Background(), tx, scopes, "now", "sha"); err != nil {
		t.Fatalf("upsertScopes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	count := func(q string, args ...any) int {
		var n int
		if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}

	// prod.ini → a usable projection.
	if n := count(`SELECT COUNT(*) FROM scope_groups`); n != 1 {
		t.Errorf("scope_groups = %d, want 1 (web)", n)
	}
	if n := count(`SELECT COUNT(*) FROM scope_group_hosts`); n != 2 {
		t.Errorf("scope_group_hosts = %d, want 2 (web1, web2)", n)
	}
	if n := count(`SELECT COUNT(*) FROM scope_group_vars`); n != 1 {
		t.Errorf("scope_group_vars = %d, want 1 (ansible_port)", n)
	}
	if n := count(`SELECT COUNT(*) FROM scope_host_vars`); n != 1 {
		t.Errorf("scope_host_vars = %d, want 1 (web1.ansible_host)", n)
	}
	var prodStatus string
	if err := pool.QueryRow(`SELECT projection_status FROM scopes WHERE name='prod'`).Scan(&prodStatus); err != nil {
		t.Fatal(err)
	}
	if prodStatus != "ok" {
		t.Errorf("prod projection_status = %q, want ok", prodStatus)
	}

	// ranged.ini → degraded: status unavailable, no tree persisted for it, but raw
	// still stored.
	var rangedStatus, rangedRaw string
	if err := pool.QueryRow(`SELECT projection_status, raw_inventory FROM scopes WHERE name='ranged'`).
		Scan(&rangedStatus, &rangedRaw); err != nil {
		t.Fatal(err)
	}
	if rangedStatus != "unavailable" {
		t.Errorf("ranged projection_status = %q, want unavailable", rangedStatus)
	}
	if rangedRaw == "" {
		t.Errorf("ranged raw_inventory must still be stored even when the projection degrades")
	}
}
