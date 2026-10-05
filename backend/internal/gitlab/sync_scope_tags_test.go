package gitlab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestSyncPreservesScopeTags is the ST-3 regression for migration 1160: a
// scope's tags are operator-owned, SQLite-only and never parsed from Git, so the
// scope upsert must leave them alone. It runs on every scope on every sync
// (ON CONFLICT on scopes(name)), which means a `tags=excluded.tags` slipped into
// that statement would silently reset every operator's labels on the next pull.
//
// Three syncs, because the upsert has two shapes worth pinning: the INSERT of a
// new scope (tags start at the column default, "[]") and the UPDATE of an
// existing one — once unchanged, and once after the inventory file itself
// changed, which is the sync that actually rewrites the row.
func TestSyncPreservesScopeTags(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopetagsync.db"))
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
	invPath := filepath.Join(invDir, "prod.ini")
	writeInvFile(t, invPath, "[web]\nweb1\n")

	svc := &Service{db: pool, cloneDir: clone}
	syncOnce := func() {
		t.Helper()
		scopes, errs := svc.parseInventories()
		if len(errs) != 0 {
			t.Fatalf("parse errors: %v", errs)
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
	}
	tags := func() string {
		t.Helper()
		var raw string
		if err := pool.QueryRow(`SELECT tags FROM scopes WHERE name='prod'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	syncOnce()
	if got := tags(); got != "[]" {
		t.Fatalf("a newly synced scope has tags %q, want the column default []", got)
	}

	const want = `["prod","linux"]`
	if _, err := pool.Exec(`UPDATE scopes SET tags=? WHERE name='prod'`, want); err != nil {
		t.Fatal(err)
	}

	syncOnce()
	if got := tags(); got != want {
		t.Fatalf("tags after an unchanged re-sync = %q, want %q — the upsert must not write the column", got, want)
	}

	writeInvFile(t, invPath, "# cronomicon:v1 description=Production web tier\n[web]\nweb1\nweb2\n")
	syncOnce()
	if got := tags(); got != want {
		t.Fatalf("tags after a re-sync of a changed inventory = %q, want %q", got, want)
	}
}
