package gitlab

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// NOTE: the former TestScopeAgencySurvivesSync (which asserted the same rule for
// the scalar scopes.agency_id) was removed with that column in migration 700. The
// rule it guarded is unchanged and is now guarded below, over the join table that
// replaced it — including the multi-agency case the scalar could not express.

// TestSyncPreservesScopeAgencies is the AG-Q6/T2.9 regression for the migration-670
// join table that supersedes agency_id in Phase 3. It is a SEPARATE test from the
// one above, deliberately: the two are governed by the same rule but by different
// code, and a future "reconcile membership during sync" change would leave the
// agency_id test passing while quietly wiping every operator assignment.
//
// The assertion is byte-for-byte identity of the membership rows across a full
// re-sync, and it covers the case the column version cannot — a scope belonging to
// MORE THAN ONE agency, which is the whole point of the join table.
func TestSyncPreservesScopeAgencies(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "agencymembersync.db"))
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
	writeInvFile(t, filepath.Join(invDir, "prod.ini"), "[web]\nweb1\n")

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
	syncOnce()

	var scopeID string
	if err := pool.QueryRow(`SELECT id FROM scopes WHERE name='prod'`).Scan(&scopeID); err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ id, name string }{{"ag-dss", "DSS"}, {"ag-nwd", "NWD"}} {
		if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES(?,?,'t')`, a.id, a.name); err != nil {
			t.Fatalf("seed agency %s: %v", a.name, err)
		}
		if _, err := pool.Exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES(?,?)`, scopeID, a.id); err != nil {
			t.Fatalf("assign %s: %v", a.name, err)
		}
	}

	read := func() []string {
		t.Helper()
		rows, err := pool.Query(`SELECT agency_id FROM scope_agencies WHERE scope_id=? ORDER BY agency_id`, scopeID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	before := read()

	// Two full syncs — a reconciliation bug that only fires on the second pull is
	// exactly the kind that survives a single-sync test.
	syncOnce()
	syncOnce()

	after := read()
	if len(after) != len(before) {
		t.Fatalf("scope_agencies changed across re-sync: %v → %v (sync must neither insert nor delete membership)", before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("scope_agencies changed across re-sync: %v → %v", before, after)
		}
	}
	if len(after) != 2 {
		t.Fatalf("expected the scope to keep BOTH agencies, got %v", after)
	}
}
