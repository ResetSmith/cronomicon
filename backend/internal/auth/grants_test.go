package auth

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

func grantsDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "grants.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func exec(t *testing.T, pool *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// seedTwoAgencies builds: agency Tax {tax, tax-audit}, agency Finance {finance}.
func seedTwoAgencies(t *testing.T, pool *sql.DB) {
	t.Helper()
	exec(t, pool, `INSERT INTO scopes (id,name,source,created_at) VALUES
		('s-tax','tax','cronomicon','2026-01-01T00:00:00Z'),
		('s-aud','tax-audit','cronomicon','2026-01-01T00:00:00Z'),
		('s-fin','finance','cronomicon','2026-01-01T00:00:00Z')`)
	exec(t, pool, `INSERT INTO agencies (id,name,created_at) VALUES
		('a-tax','Tax','2026-01-01T00:00:00Z'),
		('a-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(t, pool, `INSERT INTO scope_agencies (scope_id,agency_id) VALUES
		('s-tax','a-tax'),('s-aud','a-tax'),('s-fin','a-fin')`)
}

// TestResolveGrantsExpandsAgencies — RB-13. A grant is authored on an agency and
// evaluated on scopes; the expansion is what makes "add a scope to Tax next month"
// require no grant edit.
func TestResolveGrantsExpandsAgencies(t *testing.T) {
	pool := grantsDB(t)
	seedTwoAgencies(t, pool)
	exec(t, pool, `INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('g1','sg-tax','operator','a-tax',0,'2026-01-01T00:00:00Z')`)

	grants, err := ResolveGrants(context.Background(), pool, []string{"sg-tax"})
	if err != nil {
		t.Fatalf("ResolveGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("got %d grants, want 1: %+v", len(grants), grants)
	}
	got := grants[0]
	if got.Role != "operator" || got.Agency != "a-tax" {
		t.Errorf("grant = %+v, want operator @ a-tax", got)
	}
	sort.Strings(got.Scopes)
	if !reflect.DeepEqual(got.Scopes, []string{"tax", "tax-audit"}) {
		t.Errorf("expanded scopes = %v, want [tax tax-audit]", got.Scopes)
	}

	// Add a scope to the agency: the SAME grant now covers it, with no grant edit.
	exec(t, pool, `INSERT INTO scopes (id,name,source,created_at) VALUES ('s-new','tax-filing','cronomicon','2026-01-01T00:00:00Z')`)
	exec(t, pool, `INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s-new','a-tax')`)
	grants, _ = ResolveGrants(context.Background(), pool, []string{"sg-tax"})
	if len(grants[0].Scopes) != 3 {
		t.Errorf("after adding a scope to the agency, grant covers %v — expansion is the point of authoring on an agency",
			grants[0].Scopes)
	}
}

// TestResolveGrantsShapes covers the "*" grant, dedup across groups, an agency
// with no scopes, and exact-case group matching.
func TestResolveGrantsShapes(t *testing.T) {
	pool := grantsDB(t)
	seedTwoAgencies(t, pool)
	exec(t, pool, `INSERT INTO agencies (id,name,created_at) VALUES ('a-empty','Empty','2026-01-01T00:00:00Z')`)
	exec(t, pool, `INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('g1','sg-admins','admin',NULL,1,'2026-01-01T00:00:00Z'),
		('g2','sg-a','operator','a-tax',0,'2026-01-01T00:00:00Z'),
		('g3','sg-b','operator','a-tax',0,'2026-01-01T00:00:00Z'),
		('g4','sg-empty','viewer','a-empty',0,'2026-01-01T00:00:00Z')`)

	// The "*" shape.
	g, _ := ResolveGrants(context.Background(), pool, []string{"sg-admins"})
	if len(g) != 1 || !g[0].Unrestricted() {
		t.Fatalf("all_scopes grant = %+v, want one unrestricted grant", g)
	}

	// Two groups conferring the SAME (role, where) is one grant, not two.
	g, _ = ResolveGrants(context.Background(), pool, []string{"sg-a", "sg-b"})
	if len(g) != 1 {
		t.Errorf("duplicate (role, where) across groups produced %d grants, want 1: %+v", len(g), g)
	}

	// An agency with no scopes grants NOTHING. Empty membership on an entity means
	// "no restriction" (AG-Q1(b)); an empty grant means zero access (A5). The two
	// conventions look alike and mean opposite things.
	g, _ = ResolveGrants(context.Background(), pool, []string{"sg-empty"})
	if len(g) != 1 {
		t.Fatalf("expected one grant for the empty agency, got %+v", g)
	}
	if g[0].Unrestricted() || len(g[0].Scopes) != 0 {
		t.Errorf("empty-agency grant = %+v, want zero scopes and NOT unrestricted", g[0])
	}

	// Group matching is exact — mirroring rolesForGroups, which has no COLLATE.
	g, _ = ResolveGrants(context.Background(), pool, []string{"SG-ADMINS"})
	if len(g) != 0 {
		t.Errorf("case-mismatched group resolved %d grants; login matches exactly, so this must be 0", len(g))
	}

	// No groups, no grants.
	if g, _ := ResolveGrants(context.Background(), pool, nil); len(g) != 0 {
		t.Errorf("no groups resolved %d grants", len(g))
	}
}

// TestCrossProductLeakStaysClosed is the security property the RB-15 switch
// existed to establish, restated to survive RB-19.
//
// It was TestBackfillEquivalence: it seeded ad_group_mappings + scope_restrictions,
// re-ran migration 810's conversion, and compared grants against a legacy oracle.
// v0.57.8 dropped both tables, so that comparison is not merely unnecessary — it is
// unrunnable. What it PROVED is still worth proving, and does not need the old
// model to state it: two grants must not compose into a permission neither confers.
//
// Alice is operator on Tax and viewer on Finance. Under the two-axis model she held
// {triggerJobs} over {tax, finance} — the strongest verb applied to the widest
// scope set — so she could trigger FINANCE jobs. Under grants each half must come
// from the SAME grant.
func TestCrossProductLeakStaysClosed(t *testing.T) {
	pool := grantsDB(t)
	seedTwoAgencies(t, pool)
	exec(t, pool, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at) VALUES
		('g-ops',  'sg-ops',  'operator', 'a-tax', 0, 'test', '2026-01-01T00:00:00Z'),
		('g-view', 'sg-view', 'viewer',   'a-fin', 0, 'test', '2026-01-01T00:00:00Z')`)

	ctx := context.Background()

	// ── Single grant: the operator keeps what the operator was given ──
	opGrants, err := ResolveGrants(ctx, pool, []string{"sg-ops"})
	if err != nil {
		t.Fatalf("ResolveGrants: %v", err)
	}
	if !canViaGrants(opGrants, PermTriggerJobs, "tax") {
		t.Error("the operator grant lost triggerJobs on its own agency's scope")
	}
	// Agency Tax also holds tax-audit, and a grant authored on the agency reaches
	// every scope in it — by design (RB-Q1), and the reason the pre-flight used to
	// report widening before the conversion ran.
	if !canViaGrants(opGrants, PermTriggerJobs, "tax-audit") {
		t.Error("an agency-shaped grant must reach every scope in that agency")
	}
	if canViaGrants(opGrants, PermTriggerJobs, "finance") {
		t.Error("the operator grant reached another agency's scope")
	}

	// ── Two grants: the leak ──
	aliceGrants, err := ResolveGrants(ctx, pool, []string{"sg-ops", "sg-view"})
	if err != nil {
		t.Fatalf("ResolveGrants: %v", err)
	}
	if canViaGrants(aliceGrants, PermTriggerJobs, "finance") {
		t.Error("THE LEAK IS STILL OPEN: triggerJobs on finance came from a viewer grant " +
			"combined with an operator's verb")
	}
	if !canViaGrants(aliceGrants, PermTriggerJobs, "tax") {
		t.Error("holding a second grant revoked the first one's verb — grants must narrow " +
			"the cross-product, never the grant itself")
	}
	// Visibility stays unioned: Alice SHOULD still see Finance, she just may not
	// act on it (§2.2).
	if !hasScope(UnionGrantScopes(aliceGrants), "finance") {
		t.Error("visibility must stay unioned across grants — Alice should still SEE finance")
	}
}

// canViaGrants evaluates a grant set the way RB-15 will: a permission is held on a
// scope only when ONE grant carries both.
func canViaGrants(grants []RoleGrant, perm, scope string) bool {
	for _, g := range grants {
		if PermsForRoles([]string{g.Role}).Has(perm) && g.Covers(scope) {
			return true
		}
	}
	return false
}

func hasScope(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}

// TestGrantsAreAuthoritative — 🔴 RB-15, the moment the leak closes.
//
// This test replaces TestGrantsAreInert, which asserted the exact opposite and was
// correct for three releases: Can synthesized its grants from Roles × AllowedScopes,
// so every outcome matched the two-axis model by construction. Now Can reads the
// resolved grants, and a permission and a scope must come from the SAME grant.
//
// The fixture is the headline case from the plan: Alice is a viewer on Tax and an
// operator on Finance. Under the old model the two axes unioned independently and
// handed her {trigger} over {tax, finance} — so she could trigger TAX jobs, which
// no one ever granted her.
func TestGrantsAreAuthoritative(t *testing.T) {
	alice := Identity{
		// The legacy fields still carry the union — they are DERIVED from the grants
		// now, and visibility deliberately stays unioned (§2.2). Their presence here
		// is the point: if Can still consulted them, the leak would still be open.
		Roles:         []string{"viewer", "operator"},
		AllowedScopes: []string{"finance", "tax"},
		Grants: []RoleGrant{
			{Role: "viewer", Agency: "a-tax", Scopes: []string{"tax"}},
			{Role: "operator", Agency: "a-fin", Scopes: []string{"finance"}},
		},
	}

	if alice.Can(PermTriggerJobs, "tax") {
		t.Error("🔴 THE LEAK IS OPEN: triggerJobs on tax came from the operator grant's " +
			"verb combined with the viewer grant's scope. Can must require both from ONE grant.")
	}
	if !alice.Can(PermTriggerJobs, "finance") {
		t.Error("Alice lost triggerJobs on finance, where she IS an operator — RB-15 must " +
			"narrow, not revoke")
	}
	// Visibility is unchanged and still unioned: she should SEE Tax, just not act on it.
	if !ScopeReadable(alice, "tax") {
		t.Error("visibility narrowed — §2.2 keeps AllowedScopes a union across grants")
	}

	// No grants means no authority. This is what makes populating the field on every
	// login path (and for the bootstrap floor and the dev identity) load-bearing
	// rather than tidy.
	orphan := Identity{Roles: []string{"admin"}, AllowedScopes: []string{AllScopes}}
	if orphan.Can(PermConfigureApp, AllScopes) || orphan.CanAnywhere(PermTriggerJobs) {
		t.Error("an identity with no grants was granted authority from its legacy fields")
	}
}

// TestUnionsFeedTheDerivedFields pins that the derived Roles/AllowedScopes a login
// path writes are exactly what the grants say, so the display and the decision
// cannot disagree.
func TestUnionsFeedTheDerivedFields(t *testing.T) {
	grants := []RoleGrant{
		{Role: "viewer", Agency: "a-tax", Scopes: []string{"tax"}},
		{Role: "operator", Agency: "a-fin", Scopes: []string{"finance", "finance-ops"}},
	}
	roles := UnionGrantRoles(grants)
	if len(roles) != 2 || roles[0] != "operator" || roles[1] != "viewer" {
		t.Errorf("UnionGrantRoles = %v, want [operator viewer] (sorted)", roles)
	}
	scopes := UnionGrantScopes(grants)
	if len(scopes) != 3 {
		t.Errorf("UnionGrantScopes = %v, want all three scopes — visibility stays unioned", scopes)
	}
	// An unrestricted grant collapses the union to the exclusive "*" sentinel.
	withStar := append(grants, RoleGrant{Role: "admin", Scopes: []string{AllScopes}})
	if got := UnionGrantScopes(withStar); len(got) != 1 || got[0] != AllScopes {
		t.Errorf("UnionGrantScopes with a '*' grant = %v, want exactly [*] — the sentinel is "+
			"EXCLUSIVE in the A5 model and mixing it with named scopes is contradictory", got)
	}
}
