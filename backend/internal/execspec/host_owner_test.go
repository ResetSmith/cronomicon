package execspec_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// LR-69, LR-70, LR-71 — who a scope's run may be pointed at.
//
// Until 2.3.0 a host record written by hand belonged to nobody and answered to
// every scope, and a job's fixed target_host was resolved by name alone. So a
// job in one agency's scope reached any host record at all by naming it, and a
// record could shadow another agency's host of the same name.

func hostOwnerDB(t *testing.T) (*sql.DB, func(q string, a ...any)) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	// fin-prod is Finance's, tax-prod is Tax's, shared is Global's (born there).
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-fin', 'fin-prod', 'cronomicon', 't'), ('s-tax', 'tax-prod', 'cronomicon', 't'), ('s-glob', 'shared', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-fin', 'ag-fin'), ('s-tax', 'ag-tax')`)
	for _, sc := range []string{"s-fin", "s-tax", "s-glob"} {
		for _, h := range []string{"db01", "web01", "only-global", "only-tax"} {
			exec(`INSERT INTO scope_hosts (scope_id, host) VALUES (?, ?)`, sc, h)
		}
	}
	return pool, exec
}

func manualHost(exec func(string, ...any), id, hostname, owner, modified string) {
	exec(`INSERT INTO ssh_hosts (id, source, hostname, address, port, created_at, last_modified_at, owner_agency)
	      VALUES (?, 'cronomicon', ?, ?, 22, 't', ?, ?)`, id, hostname, "10.0.0."+id[len(id)-1:], modified, owner)
}

func TestAScopeResolvesItsOwnAgencysHostRecordsAndGlobalsOnly(t *testing.T) {
	pool, exec := hostOwnerDB(t)
	ctx := context.Background()
	// db01: a record each from Finance, Tax and Global. Tax's is the newest, so
	// "most recent wins" would hand it to everyone if nothing filtered by owner.
	manualHost(exec, "h-fin1", "db01", "ag-fin", "2026-01-02T00:00:00Z")
	manualHost(exec, "h-tax2", "db01", "ag-tax", "2026-01-09T00:00:00Z")
	// web01: Finance's and Tax's only.
	manualHost(exec, "h-fin3", "web01", "ag-fin", "2026-01-02T00:00:00Z")
	manualHost(exec, "h-tax4", "web01", "ag-tax", "2026-01-09T00:00:00Z")
	manualHost(exec, "h-glb5", "only-global", "global", "2026-01-01T00:00:00Z")
	manualHost(exec, "h-tax6", "only-tax", "ag-tax", "2026-01-01T00:00:00Z")

	resolve := func(scope, host string) string {
		t.Helper()
		tgt, err := execspec.HostByName(ctx, pool, scope, host)
		if err != nil {
			t.Fatalf("HostByName(%q, %q): %v", scope, host, err)
		}
		if tgt == nil {
			return ""
		}
		return tgt.ID
	}
	for _, c := range []struct{ scope, host, want, why string }{
		{"fin-prod", "db01", "h-fin1", "Finance's scope takes Finance's record, not Tax's newer one"},
		{"tax-prod", "db01", "h-tax2", "Tax's scope takes Tax's"},
		{"fin-prod", "web01", "h-fin3", "each agency's own"},
		{"tax-prod", "web01", "h-tax4", "each agency's own"},
		{"fin-prod", "only-global", "h-glb5", "Global's record answers to every scope"},
		{"tax-prod", "only-global", "h-glb5", "Global's record answers to every scope"},
		{"fin-prod", "only-tax", "", "another agency's record does not exist for this scope"},
		{"shared", "only-tax", "", "a Global scope sees Global's records only"},
		{"shared", "db01", "", "a Global scope sees Global's records only"},
		{"", "only-global", "h-glb5", "a job with no scope resolves against Global's records"},
		{"", "web01", "", "a job with no scope sees no agency's record"},
		{"no-such-scope", "db01", "", "a scope the catalog does not hold is no agency's"},
	} {
		if got := resolve(c.scope, c.host); got != c.want {
			t.Errorf("scope %q, host %q resolved %q, want %q: %s", c.scope, c.host, got, c.want, c.why)
		}
	}

	// A global administrator's record still wins everywhere, deliberately: it is
	// how the installation corrects a host for every agency at once.
	manualHost(exec, "h-glb7", "db01", "global", "2025-01-01T00:00:00Z")
	for _, scope := range []string{"fin-prod", "tax-prod", "shared", ""} {
		if got := resolve(scope, "db01"); got != "h-glb7" {
			t.Errorf("scope %q resolved %q for db01 once Global wrote a record, want Global's", scope, got)
		}
	}

	// The record says whose bastions it may route through.
	tgt, _ := execspec.HostByName(ctx, pool, "fin-prod", "web01")
	if tgt == nil || len(tgt.Owners) != 1 || tgt.Owners[0] != "ag-fin" {
		t.Errorf("Finance's hand-written record answers to %v, want [ag-fin]", tgt)
	}
	// An imported record answers to its scope's agency, whatever its column says.
	exec(`INSERT INTO ssh_hosts (id, source, scope_id, hostname, address, port, created_at, last_modified_at)
	      VALUES ('h-imp8', 'cronomicon', 's-tax', 'imported01', '10.0.0.8', 22, 't', 't')`)
	tgt, _ = execspec.HostByName(ctx, pool, "tax-prod", "imported01")
	if tgt == nil || len(tgt.Owners) != 1 || tgt.Owners[0] != "ag-tax" {
		t.Errorf("a record imported for Tax's scope answers to %v, want [ag-tax]", tgt)
	}
	if got := resolve("fin-prod", "imported01"); got != "" {
		t.Errorf("Finance's scope resolved a record imported for Tax's scope: %q", got)
	}
}

// LR-71 — a job's fixed target_host must be one of its scope's hosts. This is
// the one function every producer's run resolves through.
func TestAFixedTargetHostMustBeAMemberOfTheScope(t *testing.T) {
	pool, exec := hostOwnerDB(t)
	ctx := context.Background()
	manualHost(exec, "h-glb1", "db01", "global", "t")
	manualHost(exec, "h-glb2", "elsewhere", "global", "t") // a real record, in no scope's inventory

	one := func(scope, host string) execspec.Target {
		t.Helper()
		ts, err := execspec.ResolveTargets(ctx, pool, scope, host, nil)
		if err != nil || len(ts) != 1 {
			t.Fatalf("ResolveTargets(%q, %q) = %v, %v; want one target", scope, host, ts, err)
		}
		return ts[0]
	}
	if got := one("fin-prod", "db01"); got.ResolveErr != "" || got.ID != "h-glb1" {
		t.Errorf("a member host did not resolve: %+v", got)
	}
	got := one("fin-prod", "elsewhere")
	if got.ID != "" || !strings.Contains(got.ResolveErr, "not a member of scope fin-prod") {
		t.Errorf("a host outside the scope resolved, or failed without saying why: %+v", got)
	}
	// It fails as a per-host error, never by widening to the scope.
	if ts, _ := execspec.ResolveTargets(ctx, pool, "fin-prod", "elsewhere", nil); len(ts) != 1 {
		t.Errorf("a refused target became %d targets", len(ts))
	}
	// A job with no scope, and one whose scope the catalog does not hold, have
	// no membership to check: they resolve against Global's records.
	if got := one("", "elsewhere"); got.ResolveErr != "" || got.ID != "h-glb2" {
		t.Errorf("an unscoped job's target did not resolve against Global's records: %+v", got)
	}
	if got := one("no-such-scope", "elsewhere"); got.ResolveErr != "" || got.ID != "h-glb2" {
		t.Errorf("a job in an unknown scope did not resolve against Global's records: %+v", got)
	}
	// A scope that lists no hosts at all — its hosts live in a runner's own
	// inventory, or it is only a label — has no membership to be outside of: its
	// pinned hosts resolve as an unscoped job's would, still by owner.
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('s-empty', 'runner-side', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-empty', 'ag-fin')`)
	manualHost(exec, "h-tax3", "tax-only", "ag-tax", "t")
	if got := one("runner-side", "elsewhere"); got.ResolveErr != "" || got.ID != "h-glb2" {
		t.Errorf("a pinned host of a scope with no host list did not resolve: %+v", got)
	}
	if got := one("runner-side", "tax-only"); got.ID != "" {
		t.Errorf("a scope with no host list resolved another agency's record: %+v", got)
	}
	for _, c := range []struct {
		scope, host string
		in, known   bool
	}{
		{"fin-prod", "db01", true, true}, {"fin-prod", "elsewhere", false, true},
		{"", "db01", false, false}, {"no-such-scope", "db01", false, false},
		{"runner-side", "db01", false, false},
	} {
		in, known, err := execspec.HostInScope(ctx, pool, c.scope, c.host)
		if err != nil || in != c.in || known != c.known {
			t.Errorf("HostInScope(%q, %q) = %v, %v, %v; want %v, %v", c.scope, c.host, in, known, err, c.in, c.known)
		}
	}
}
