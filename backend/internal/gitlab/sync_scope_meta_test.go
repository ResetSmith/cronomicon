package gitlab

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// TestSyncLegacyTypesPragma is the ST-Q1 regression. Supported run types were
// removed in 2.1.0 (migration 1170), but the inventories already in users'
// repositories still say `# cronomicon:v1 types=…`. The directive must be
// ignored, not rejected:
//
//   - parseInventories returns NO error for it. Its error list is what sets
//     scopesOK in the sync, and a false scopesOK both marks the sync `partial`
//     and suppresses scope pruning — so a rejection would have frozen pruning on
//     every repository until someone edited the file.
//   - the scope still syncs, and what is stored is the Git metadata alone:
//     owner and sidecar path, no run types.
//   - a sidecar that still carries spec.types loads, and its owner wins.
//
// It also pins ST-D4: an owner declared WITHOUT types reaches the API. Before
// 2.1.0 it rode on the capability and was dropped when no types were declared.
func TestSyncLegacyTypesPragma(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopemeta.db"))
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
	// legacy: the pre-2.1.0 shape, types + owner in the pragma.
	writeInvFile(t, filepath.Join(invDir, "legacy.ini"),
		"# cronomicon:v1 types=bash,ansible\n# cronomicon:v1 owner=infra\n# cronomicon:v1 description=Legacy fleet\n[web]\nweb1\n")
	// bare: an owner and nothing else — the case that used to lose its owner.
	writeInvFile(t, filepath.Join(invDir, "bare.ini"), "# cronomicon:v1 owner=plat-eng\n[db]\ndb1\n")
	// sidecar: a sidecar that still declares types beside an owner.
	writeInvFile(t, filepath.Join(invDir, "sidecar.ini"), "# cronomicon:v1 owner=from-pragma\n[app]\napp1\n")
	writeInvFile(t, filepath.Join(invDir, "sidecar.cronomicon.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: InventoryMeta\nmetadata:\n  name: sidecar\nspec:\n  types: [bash, terraform]\n  owner: from-sidecar\n")

	svc := &Service{db: pool, cloneDir: clone}
	scopes, errs := svc.parseInventories()
	if len(errs) != 0 {
		t.Fatalf("a retired types directive must not be a parse error (it would suppress pruning): %v", errs)
	}
	if len(scopes) != 3 {
		t.Fatalf("want 3 scopes parsed, got %d", len(scopes))
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

	// What is stored is owner / sidecarPath / errors — and nothing about types.
	var raw string
	if err := pool.QueryRow(`SELECT git_meta_json FROM scopes WHERE name='legacy'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatalf("git_meta_json is not an object: %v (%s)", err, raw)
	}
	for _, gone := range []string{"types", "origin"} {
		if _, ok := stored[gone]; ok {
			t.Errorf("git_meta_json still carries %q: %s", gone, raw)
		}
	}

	list, err := settings.ListScopes(context.Background(), pool, "git")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]settings.Scope{}
	for _, sc := range list {
		byName[sc.Scope] = sc
	}
	owner := func(name string) string {
		t.Helper()
		sc, ok := byName[name]
		if !ok {
			t.Fatalf("scope %q was not synced", name)
		}
		if sc.Owner == nil {
			return ""
		}
		return *sc.Owner
	}
	if got := owner("legacy"); got != "infra" {
		t.Errorf("legacy owner = %q, want infra", got)
	}
	if d := byName["legacy"].Description; d == nil || *d != "Legacy fleet" {
		t.Errorf("legacy description = %v, want the pragma description", d)
	}
	if got := owner("bare"); got != "plat-eng" {
		t.Errorf("an owner declared without types = %q, want plat-eng (ST-D4)", got)
	}
	if got := owner("sidecar"); got != "from-sidecar" {
		t.Errorf("sidecar owner = %q, want from-sidecar (sidecar over pragma)", got)
	}
	if p := byName["sidecar"].SidecarPath; p == nil || *p != "inventory/sidecar.cronomicon.yaml" {
		t.Errorf("sidecar path = %v, want inventory/sidecar.cronomicon.yaml", p)
	}
	if p := byName["legacy"].SidecarPath; p != nil {
		t.Errorf("a scope with no sidecar reports one: %q", *p)
	}
	for name, sc := range byName {
		if len(sc.PragmaErrors) != 0 {
			t.Errorf("%s: unexpected pragma errors %+v", name, sc.PragmaErrors)
		}
	}
}
