package execspec_test

import (
	"context"
	"database/sql"
	"errors"
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
		// §0a (IN-6; question 33): Global's record is Global's runs' only. Until
		// then it answered to every scope.
		{"fin-prod", "only-global", "", "Global's record is not Finance's"},
		{"tax-prod", "only-global", "", "Global's record is not Tax's"},
		{"shared", "only-global", "h-glb5", "a Global scope sees Global's records"},
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

	// A record Global writes for a host is Global's runs': each agency keeps its
	// own (§0a, question 33). Until then a global administrator's record won
	// everywhere, as the way to correct a host for every agency at once.
	manualHost(exec, "h-glb7", "db01", "global", "2025-01-01T00:00:00Z")
	for scope, want := range map[string]string{"fin-prod": "h-fin1", "tax-prod": "h-tax2", "shared": "h-glb7", "": "h-glb7"} {
		if got := resolve(scope, "db01"); got != want {
			t.Errorf("scope %q resolved %q for db01 once Global wrote a record, want %q", scope, got, want)
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

// LR-70 — a host record routes only through its own agency's bastion. Since
// §0a (question 33) that holds for Global's too: until then every record could
// route through Global's, after its agency's own.
func TestARecordRoutesThroughItsOwnAgencysBastionOnly(t *testing.T) {
	pool, exec := hostOwnerDB(t)
	ctx := context.Background()
	exec(`INSERT INTO bastions (id, name, hostname, address, port, created_at, owner_agency) VALUES
	      ('b-glob', 'jump', 'jump', '10.9.0.1', 22, 't', 'global'),
	      ('b-glob2', 'global-jump', 'global-jump', '10.9.0.2', 22, 't', 'global'),
	      ('b-fin', 'fin-jump', 'fin-jump', '10.9.0.3', 22, 't', 'ag-fin')`)
	for _, c := range []struct {
		ref    string
		owners []string
		want   string
	}{
		{"fin-jump", []string{"ag-fin"}, "b-fin"},
		{"jump", []string{"ag-fin"}, ""},        // Global's: not Finance's records' (until §0a it was)
		{"global-jump", []string{"ag-tax"}, ""}, // the same for Tax
		{"fin-jump", []string{"ag-tax"}, ""},    // another agency's, as always
		{"jump", []string{"global"}, "b-glob"},  // Global's record, Global's bastion
		{"global-jump", nil, "b-glob2"},         // a record with no owner to name is Global's
		{"fin-jump", nil, ""},
	} {
		b, err := execspec.BastionByRef(ctx, pool, c.ref, c.owners)
		got := ""
		if b != nil {
			got = b.ID
		}
		if got != c.want || (c.want == "" && !errors.Is(err, execspec.ErrNoBastion)) || (c.want != "" && err != nil) {
			t.Errorf("BastionByRef(%q, %v) = %q, %v; want %q", c.ref, c.owners, got, err, c.want)
		}
	}
}

// LR-71 — a job's fixed target_host must be one of its scope's hosts. This is
// the one function every producer's run resolves through.
func TestAFixedTargetHostMustBeAMemberOfTheScope(t *testing.T) {
	pool, exec := hostOwnerDB(t)
	ctx := context.Background()
	manualHost(exec, "h-fin1", "db01", "ag-fin", "t")
	manualHost(exec, "h-glb2", "elsewhere", "global", "t") // a real record, in no scope's inventory
	manualHost(exec, "h-fin4", "fin-side", "ag-fin", "t")  // Finance's, in no scope's inventory

	one := func(scope, host string) execspec.Target {
		t.Helper()
		ts, err := execspec.ResolveTargets(ctx, pool, scope, host, nil)
		if err != nil || len(ts) != 1 {
			t.Fatalf("ResolveTargets(%q, %q) = %v, %v; want one target", scope, host, ts, err)
		}
		return ts[0]
	}
	if got := one("fin-prod", "db01"); got.ResolveErr != "" || got.ID != "h-fin1" {
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
	if got := one("runner-side", "fin-side"); got.ResolveErr != "" || got.ID != "h-fin4" {
		t.Errorf("a pinned host of a scope with no host list did not resolve: %+v", got)
	}
	// Global's record is not Finance's scope's (§0a, question 33).
	if got := one("runner-side", "elsewhere"); got.ID != "" {
		t.Errorf("a scope of Finance's with no host list resolved Global's record: %+v", got)
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
