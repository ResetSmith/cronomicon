package execspec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// TestResolveTargetsSubset covers the F2 host-subset mode of ResolveTargets
// (architecture-update.md §5): full fan-out with no subset, a member subset
// resolving exactly those hosts, a non-member surfacing as a per-host ResolveErr
// (never a silent skip or a widen-to-full-scope), and targetHost still winning.
func TestResolveTargetsSubset(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "targets.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	scopeID := db.NewID()
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES (?, 'Prod', 'amadeus', 't')`, scopeID)
	for _, h := range []string{"web1", "web2", "web3"} {
		exec(`INSERT INTO scope_hosts(scope_id, host) VALUES (?, ?)`, scopeID, h)
		exec(`INSERT INTO ssh_hosts(id, hostname, port, username, auth_key_env_var, created_at)
		      VALUES (?, ?, 22, 'deploy', '', 't')`, db.NewID(), h)
	}

	names := func(ts []execspec.Target) []string {
		out := make([]string, len(ts))
		for i, tg := range ts {
			out[i] = tg.Name
		}
		return out
	}

	// Full fan-out (no subset).
	all, err := execspec.ResolveTargets(ctx, pool, "Prod", "", nil)
	if err != nil {
		t.Fatalf("fan-out: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("full fan-out = %v, want 3 hosts", names(all))
	}

	// Subset of two members → exactly those two, no ResolveErr.
	sub, err := execspec.ResolveTargets(ctx, pool, "Prod", "", []string{"web1", "web3"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	if len(sub) != 2 || sub[0].Name != "web1" || sub[1].Name != "web3" {
		t.Fatalf("subset = %v, want [web1 web3]", names(sub))
	}
	for _, tg := range sub {
		if tg.ResolveErr != "" {
			t.Errorf("member %q unexpectedly has ResolveErr %q", tg.Name, tg.ResolveErr)
		}
	}

	// A non-member in the subset surfaces as a per-host ResolveErr — not a silent
	// skip, and the run does NOT widen to the full scope.
	mixed, err := execspec.ResolveTargets(ctx, pool, "Prod", "", []string{"web1", "rogue"})
	if err != nil {
		t.Fatalf("mixed subset: %v", err)
	}
	if len(mixed) != 2 {
		t.Fatalf("mixed subset = %v, want 2 entries", names(mixed))
	}
	var sawRogueErr bool
	for _, tg := range mixed {
		if tg.Name == "rogue" && tg.ResolveErr != "" {
			sawRogueErr = true
		}
	}
	if !sawRogueErr {
		t.Errorf("non-member host should carry a ResolveErr, got %+v", mixed)
	}

	// targetHost (single) still wins over scope/subset.
	single, err := execspec.ResolveTargets(ctx, pool, "Prod", "web2", []string{"web1"})
	if err != nil {
		t.Fatalf("single: %v", err)
	}
	if len(single) != 1 || single[0].Name != "web2" {
		t.Errorf("single-host override = %v, want [web2]", names(single))
	}

	// ScopeHosts returns the membership set; OverrideHosts parses the envelope.
	if members, err := execspec.ScopeHosts(ctx, pool, "Prod"); err != nil || len(members) != 3 {
		t.Errorf("ScopeHosts = %v (err %v), want 3", members, err)
	}
	if got := execspec.OverrideHosts(`{"hosts":["web1","web2"]}`); len(got) != 2 {
		t.Errorf("OverrideHosts = %v, want 2", got)
	}
	if got := execspec.OverrideHosts(""); got != nil {
		t.Errorf("OverrideHosts(empty) = %v, want nil", got)
	}
}
