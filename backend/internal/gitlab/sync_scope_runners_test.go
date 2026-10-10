package gitlab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestSyncPreservesScopeRunners is the SB-1 regression for migration 1180: a
// scope's runner binding is an operator-owned overlay, never parsed from Git,
// so a sync must leave it alone. The binding hangs off the scope's ID, which
// makes the thing to pin the upsert's SHAPE rather than a column list: it must
// stay an `ON CONFLICT … DO UPDATE` that keeps the row. Rewritten as a
// delete-and-insert (or INSERT OR REPLACE) it would mint a new id, the cascade
// would take the binding, and every restricted scope would silently reopen to
// its whole agency on the next pull.
//
// Three syncs, as for the tags: the INSERT of a new scope, the UPDATE of an
// unchanged one, and the UPDATE after the inventory file itself changed.
func TestSyncPreservesScopeRunners(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scoperunnersync.db"))
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
		if _, err := svc.upsertScopes(context.Background(), tx, scopes, "now", "sha"); err != nil {
			t.Fatalf("upsertScopes: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	bound := func() []string {
		t.Helper()
		rows, err := pool.Query(`
			SELECT sr.runner_id FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id
			 WHERE sc.name = 'prod' ORDER BY sr.runner_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out = append(out, id)
		}
		return out
	}

	syncOnce()
	if got := bound(); len(got) != 0 {
		t.Fatalf("a newly synced scope is bound to %v, want nothing (unrestricted)", got)
	}

	// The operator binds the scope. No runners row is needed: a binding names an
	// id and deliberately has no foreign key to it.
	if _, err := pool.Exec(`
		INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
		SELECT id, 'r-dmz', 'runner-dmz-01', 'ops@example', 'now' FROM scopes WHERE name = 'prod'`); err != nil {
		t.Fatalf("bind: %v", err)
	}

	syncOnce()
	if got := bound(); len(got) != 1 || got[0] != "r-dmz" {
		t.Fatalf("after an unchanged re-sync the scope is bound to %v, want [r-dmz]", got)
	}

	writeInvFile(t, invPath, "[web]\nweb1\nweb2\n")
	syncOnce()
	if got := bound(); len(got) != 1 || got[0] != "r-dmz" {
		t.Fatalf("after a re-sync that changed the inventory the scope is bound to %v, want [r-dmz]", got)
	}
}
