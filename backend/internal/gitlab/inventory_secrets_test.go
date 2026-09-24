package gitlab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseInventories_RejectsSecret verifies the M1 Path-A wiring: a secret-
// bearing inventory file is SKIPPED (no scope produced, so no raw_inventory is
// ever persisted/shipped) and surfaces a line-numbered error — which also feeds
// scopeErrs → scopesOK, suppressing scope pruning for the sync (the intended safe
// failure mode, §5). A clean inventory in the same dir still produces its scope.
func TestParseInventories_RejectsSecret(t *testing.T) {
	clone := t.TempDir()
	invDir := filepath.Join(clone, "inventory")
	if err := os.MkdirAll(invDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeInvFile(t, filepath.Join(invDir, "prod.ini"), "[web]\nweb1 ansible_host=10.0.0.1\n")
	writeInvFile(t, filepath.Join(invDir, "bad.ini"), "[db:vars]\nansible_become_pass=hunter2\n")

	svc := &Service{cloneDir: clone}
	scopes, errs := svc.parseInventories()

	var prodOK bool
	for _, sc := range scopes {
		if sc.Name == "bad" {
			t.Errorf("secret-bearing scope %q must be SKIPPED, but was produced", sc.Name)
		}
		if sc.Name == "prod" {
			prodOK = true
			if sc.Content == "" || sc.Format != "ini" {
				t.Errorf("clean scope missing raw content/format: %+v", sc)
			}
		}
	}
	if !prodOK {
		t.Errorf("clean scope 'prod' should still be produced; got %d scopes", len(scopes))
	}

	var found bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "ansible_become_pass") && strings.Contains(e.Error(), "bad.ini") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a line-numbered secret rejection error mentioning bad.ini, got %v", errs)
	}
}

func writeInvFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
