package execspec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
)

func TestAnsibleLimit(t *testing.T) {
	cases := []struct {
		name   string
		hosts  []string
		groups []string
		want   string
	}{
		{"empty", nil, nil, ""},
		{"hosts only", []string{"web1", "web2"}, nil, "web1,web2"},
		{"groups only", nil, []string{"web", "db"}, "web,db"},
		{"host union group", []string{"web1"}, []string{"db"}, "web1,db"},
		{"metachar names refused", []string{"web,bad", "ok1"}, []string{"all*", "prod"}, "ok1,prod"},
		{"all metachar -> empty", []string{"a:b"}, []string{"x!y"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execspec.AnsibleLimit(tc.hosts, tc.groups); got != tc.want {
				t.Errorf("AnsibleLimit(%v,%v) = %q, want %q", tc.hosts, tc.groups, got, tc.want)
			}
		})
	}
}

// TestPinnedAnsibleLimit exercises the TG-3 pin half of RunLimit. The precedence
// under test is the PARITY rule: a pin wins outright and the hosts/groups subset
// is ignored, mirroring ResolveTargets, which returns the single pinned Target and
// ignores its hosts argument. Unioning them instead would make --limit BROADER
// than the host set SSH dials (an ansible comma is a union, not an intersection) —
// executor drift in the dangerous direction, which is what this asserts against.
//
// It also documents the TG-4 hazard AnsibleLimit's silent refusal creates: a
// metachar pin yields an EMPTY limit, i.e. no --limit, i.e. the full inventory —
// exactly why the trigger 422 / compose 422 / manifest 409 guards exist upstream.
func TestPinnedAnsibleLimit(t *testing.T) {
	cases := []struct {
		name       string
		hosts      []string
		groups     []string
		targetHost string
		want       string
	}{
		{"pin alone", nil, nil, "web1", "web1"},
		{"pin wins outright over a host subset and groups (ResolveTargets parity)", []string{"db1"}, []string{"web"}, "web1", "web1"},
		{"pin already in subset does not duplicate", []string{"web1"}, nil, "web1", "web1"},
		{"empty pin behaves exactly as AnsibleLimit (regression guard)", []string{"web1", "web2"}, []string{"db"}, "", "web1,web2,db"},
		{"metachar pin yields empty limit (the TG-4 hazard)", nil, nil, "web[01:50]", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execspec.PinnedAnsibleLimit(tc.hosts, tc.groups, tc.targetHost); got != tc.want {
				t.Errorf("PinnedAnsibleLimit(%v,%v,%q) = %q, want %q", tc.hosts, tc.groups, tc.targetHost, got, tc.want)
			}
		})
	}
}

// TestRunLimit covers RunLimit's precedence: a raw ansibleLimit passthrough in
// override_json wins outright over the pin (the operator has taken manual control,
// so a metachar pin is harmless on that branch — which is why the 422/409 guards
// are scoped to skip it), and absent that it falls through to PinnedAnsibleLimit.
func TestRunLimit(t *testing.T) {
	cases := []struct {
		name         string
		overrideJSON string
		targetHost   string
		want         string
	}{
		{"raw ansibleLimit wins over the pin", `{"ansibleLimit":"web:&staged"}`, "web1", "web:&staged"},
		{"no override, pin alone", "", "web1", "web1"},
		{"pin wins over override hosts (ResolveTargets parity)", `{"hosts":["db1"]}`, "web1", "web1"},
		{"no override, no pin -> empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execspec.RunLimit(tc.overrideJSON, tc.targetHost); got != tc.want {
				t.Errorf("RunLimit(%q,%q) = %q, want %q", tc.overrideJSON, tc.targetHost, got, tc.want)
			}
		})
	}
}

func TestOverrideGroupsAndLimit(t *testing.T) {
	if got := execspec.OverrideGroups(`{"groups":["web","db"]}`); len(got) != 2 || got[0] != "web" {
		t.Errorf("OverrideGroups = %v, want [web db]", got)
	}
	if got := execspec.OverrideGroups(`{"hosts":["x"]}`); got != nil {
		t.Errorf("OverrideGroups (absent) = %v, want nil", got)
	}
	if got := execspec.OverrideAnsibleLimit(`{"ansibleLimit":"web:&staged"}`); got != "web:&staged" {
		t.Errorf("OverrideAnsibleLimit = %q", got)
	}
	if got := execspec.OverrideAnsibleLimit(`{}`); got != "" {
		t.Errorf("OverrideAnsibleLimit (absent) = %q, want empty", got)
	}
}

func TestResolveRun_GroupExpansion(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "run.db"))
	if err != nil {
		t.Fatal(err)
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
	exec(`INSERT INTO scopes(id,name,source,created_at,projection_status) VALUES (?,'Prod','git','t','ok')`, scopeID)
	for _, h := range []string{"web1", "web2", "db1"} {
		exec(`INSERT INTO scope_hosts(scope_id,host) VALUES (?,?)`, scopeID, h)
		exec(`INSERT INTO ssh_hosts(id,hostname,port,username,auth_key_env_var,created_at) VALUES (?,?,22,'deploy','','t')`, db.NewID(), h)
	}
	exec(`INSERT INTO scope_groups(scope_id,name) VALUES (?,'web')`, scopeID)
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'web','web1')`, scopeID)
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'web','web2')`, scopeID)

	names := func(ts []execspec.Target) []string {
		out := make([]string, len(ts))
		for i, tg := range ts {
			out[i] = tg.Name
		}
		return out
	}

	// ScopeGroups + GroupHosts readers.
	if g, err := execspec.ScopeGroups(ctx, pool, "Prod"); err != nil || len(g) != 1 || g[0] != "web" {
		t.Errorf("ScopeGroups = %v (err %v), want [web]", g, err)
	}
	if h, err := execspec.GroupHosts(ctx, pool, "Prod", "web"); err != nil || len(h) != 2 {
		t.Errorf("GroupHosts(web) = %v (err %v), want 2", h, err)
	}

	// Group expansion → its member hosts as Targets, plus the --limit by NAME.
	targets, limit, err := execspec.ResolveRun(ctx, pool, "Prod", "", nil, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(targets); len(got) != 2 || got[0] != "web1" || got[1] != "web2" {
		t.Errorf("group expansion targets = %v, want [web1 web2]", got)
	}
	if limit != "web" {
		t.Errorf("limit = %q, want web", limit)
	}

	// Union of explicit host + group members; limit = host UNION group NAMES.
	targets, limit, err = execspec.ResolveRun(ctx, pool, "Prod", "", []string{"db1"}, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(targets); len(got) != 3 {
		t.Errorf("union targets = %v, want 3 (db1,web1,web2)", got)
	}
	if limit != "db1,web" {
		t.Errorf("limit = %q, want db1,web", limit)
	}

	// No hosts/groups → full fan-out + no limit (legacy default).
	targets, limit, err = execspec.ResolveRun(ctx, pool, "Prod", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 || limit != "" {
		t.Errorf("full fan-out = %v limit=%q, want 3 hosts and empty limit", names(targets), limit)
	}
}

// TestResolveRun_NestedGroups exercises the CRITICAL parity fix: a [group:children]
// group expands TRANSITIVELY (mirroring ansible --limit), and a group that resolves
// to ZERO hosts must run NOTHING — never silently fan out to the whole scope.
func TestResolveRun_NestedGroups(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "nested.db"))
	if err != nil {
		t.Fatal(err)
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
	exec(`INSERT INTO scopes(id,name,source,created_at,projection_status) VALUES (?,'Prod','git','t','ok')`, scopeID)
	for _, h := range []string{"web1", "web2", "db1", "mon1"} { // mon1 is ungrouped
		exec(`INSERT INTO scope_hosts(scope_id,host) VALUES (?,?)`, scopeID, h)
		exec(`INSERT INTO ssh_hosts(id,hostname,port,username,auth_key_env_var,created_at) VALUES (?,?,22,'deploy','','t')`, db.NewID(), h)
	}
	for _, g := range []string{"web", "db", "allservers", "empty"} {
		exec(`INSERT INTO scope_groups(scope_id,name) VALUES (?,?)`, scopeID, g)
	}
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'web','web1')`, scopeID)
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'web','web2')`, scopeID)
	exec(`INSERT INTO scope_group_hosts(scope_id,group_name,host) VALUES (?,'db','db1')`, scopeID)
	// allservers has ONLY children (no direct members) — the rollup case.
	exec(`INSERT INTO scope_group_children(scope_id,parent,child) VALUES (?,'allservers','web')`, scopeID)
	exec(`INSERT INTO scope_group_children(scope_id,parent,child) VALUES (?,'allservers','db')`, scopeID)

	names := func(ts []execspec.Target) map[string]bool {
		out := map[string]bool{}
		for _, tg := range ts {
			out[tg.Name] = true
		}
		return out
	}

	// Child-only group expands transitively to web1,web2,db1 — NOT mon1, and NOT a
	// full-scope fan-out.
	targets, limit, err := execspec.ResolveRun(ctx, pool, "Prod", "", nil, []string{"allservers"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(targets)
	if len(got) != 3 || !got["web1"] || !got["web2"] || !got["db1"] || got["mon1"] {
		t.Errorf("allservers expansion = %v, want {web1,web2,db1} (no mon1, no fan-out)", got)
	}
	if limit != "allservers" {
		t.Errorf("limit = %q, want allservers", limit)
	}

	// An EMPTY group (no hosts, no children) must run NOTHING — never the full scope.
	targets, _, err = execspec.ResolveRun(ctx, pool, "Prod", "", nil, []string{"empty"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 {
		t.Errorf("empty group = %v, want NO targets (must not fan out to the scope)", names(targets))
	}
}
