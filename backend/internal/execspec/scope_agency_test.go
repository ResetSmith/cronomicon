package execspec

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestScopeAgency covers the M3 resolver: a scope bound to an agency resolves to
// that agency's NAME; an unbound scope, an unknown scope, and the empty scope all
// resolve to "" (general pool).
func TestScopeAgency(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopeagency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('a1','alpha','t')`)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('a2','beta','t')`)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('s1','prod','cronomicon','t')`)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('s2','staging','cronomicon','t')`)
	// T3.6 — membership lives in scope_agencies now; scopes.agency_id was dropped in
	// migration 700. A scope may belong to SEVERAL agencies, which is the whole
	// reason the scalar had to go.
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('s1','a1')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('s1','a2')`)

	ctx := context.Background()
	cases := []struct {
		scope string
		want  []string
	}{
		{"prod", []string{"alpha", "beta"}}, // bound to BOTH, sorted by name
		{"staging", nil},                    // exists, no agency
		{"ghost", nil},                      // unknown scope
		{"", nil},                           // unscoped
	}
	for _, c := range cases {
		got, err := ScopeAgencies(ctx, pool, c.scope)
		if err != nil {
			t.Fatalf("ScopeAgencies(%q): %v", c.scope, err)
		}
		if len(got) != len(c.want) {
			t.Errorf("ScopeAgencies(%q) = %v, want %v", c.scope, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("ScopeAgencies(%q) = %v, want %v", c.scope, got, c.want)
				break
			}
		}
		// The snapshot written at enqueue must be deterministic — two enqueues of the
		// same scope have to produce byte-identical JSON.
		first, second := MarshalAgencies(got), MarshalAgencies(got)
		if first != second {
			t.Errorf("MarshalAgencies is not deterministic for %v: %q vs %q", got, first, second)
		}
	}
}
