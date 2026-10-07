package execspec

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestScopeAgency covers the M3 resolver: a scope bound to an agency resolves to
// that agency's NAME; a scope nobody assigned, an unknown scope, and the empty
// scope all resolve to Global (migration 1220 — they resolved to nothing, which
// meant "the general pool", before Global was a row).
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
		{"staging", []string{"Global"}},     // exists, never assigned: born in Global
		{"ghost", []string{"Global"}},       // unknown scope (a job's scope is free text)
		{"", []string{"Global"}},            // unscoped
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

// An unreadable membership is not an empty one. ScopeAgencies used to answer
// "no agencies" when its query failed, and every producer stamped the run with
// that: claimable by general-pool runners, resolving none of its own agency's
// secrets. It returns the error, and the producers refuse to enqueue on it.
func TestScopeAgenciesReturnsAReadFailure(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopeagencyerr.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	if got, err := ScopeAgencies(ctx, pool, ""); err != nil || len(got) != 1 || got[0] != "Global" {
		t.Fatalf("the empty scope = %v, %v; want [Global] and no error", got, err)
	}
	_ = pool.Close()
	got, err := ScopeAgencies(ctx, pool, "prod")
	if err == nil {
		t.Fatalf("a failed read answered %v with no error — it would be taken for \"no agency\"", got)
	}
	if got != nil {
		t.Errorf("a failed read returned a set (%v) alongside its error", got)
	}
}

// A scope that exists and belongs to NO agency is not "global": the database
// gives every scope a Global row at birth and the setters refuse to leave one
// with none, so the state is corruption. It is an error the producers refuse
// on — reading it as Global would bring back the convention migration 1220
// retired, and reading it as "nobody's" would hide the scope.
func TestAScopeWithNoAgencyIsAnErrorNotGlobal(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopenoagency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO scopes(id, name, source, created_at) VALUES('s1','orphan','cronomicon','t')`,
		`DELETE FROM scope_agencies WHERE scope_id = 's1'`, // what no route can do
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	got, err := ScopeAgencies(context.Background(), pool, "orphan")
	if !errors.Is(err, ErrScopeHasNoAgency) {
		t.Fatalf("ScopeAgencies(orphan) = %v, %v; want ErrScopeHasNoAgency", got, err)
	}
}
