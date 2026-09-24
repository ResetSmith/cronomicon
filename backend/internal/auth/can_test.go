package auth

import (
	"reflect"
	"testing"
)

// wide builds an identity whose grants carry the FULL scope list on every role —
// the shape the pre-RB-15 synthesis produced. Tests that are about Can's SEMANTICS
// (the per-grant conjunction, fail-closed on a typo, CanAnywhere vs Can) use it so
// they keep measuring what they were written to measure; the tests about the SWITCH
// itself build narrow grants by hand (see TestGrantsAreAuthoritative).
func wide(roles []string, scopes []string) Identity {
	id := Identity{Roles: roles, AllowedScopes: scopes}
	for _, r := range roles {
		id.Grants = append(id.Grants, RoleGrant{Role: CanonRole(r), Scopes: scopes})
	}
	return id
}

// TestCanMatchesLegacyAuthorization pins that Can DEGENERATES to the old union
// model when every grant is wide — over a generated matrix of role and scope
// combinations it returns exactly PermsForRoles(roles).Has(perm) &&
// ScopeReadable(id, scope).
//
// It was the safety property of the behavior-neutral releases (RB-1). Post-RB-15 it
// still earns its place, inverted in meaning: it proves Can narrows ONLY because
// grants are narrow, never on its own. A regression that made Can stricter than its
// grants — denying an actor whose single grant genuinely covers both halves — would
// fail here and nowhere else.
func TestCanMatchesLegacyAuthorization(t *testing.T) {
	roleSets := [][]string{
		nil,
		{},
		{"viewer"},
		{"operator"},
		{"approver"},
		{"admin"},
		{"viewer", "operator"},
		{"operator", "approver"},
		{"viewer", "admin"},
		{"viewer", "operator", "approver", "admin"},
		{"nonexistent"},
		{"nonexistent", "operator"},
		{"Operator"}, // non-canonical casing — canonRole must normalize it
	}
	scopeSets := [][]string{
		nil,
		{},
		{AllScopes},
		{"tax"},
		{"tax", "finance"},
		{"finance"},
	}
	perms := PermissionNames
	// The empty scope is the unscoped/global object (Q-F7); AllScopes is the
	// "no object scope exists here" sentinel that requirePerm evaluates.
	targets := []string{"", "tax", "finance", "unlisted", AllScopes}

	checked := 0
	for _, roles := range roleSets {
		for _, scopes := range scopeSets {
			id := wide(roles, scopes)
			legacyPerms := PermsForRoles(roles)
			for _, perm := range perms {
				for _, target := range targets {
					// What the code did before the seam existed: an unconditional
					// OR-union permission check, and a SEPARATE scope check.
					var want bool
					if target == AllScopes {
						want = legacyPerms.Has(perm) && id.Unrestricted()
					} else {
						want = legacyPerms.Has(perm) && ScopeReadable(id, target)
					}
					got := id.Can(perm, target)
					if got != want {
						t.Errorf("Can(%q, %q) = %v, want %v (roles=%v scopes=%v)",
							perm, target, got, want, roles, scopes)
					}
					checked++
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("matrix generated no cases")
	}
	t.Logf("verified %d role×scope×permission×target combinations", checked)
}

// TestCanFailsClosedOnUnknownPermission guards the silent-typo failure mode: Can
// takes a string, so a misspelled permission must deny rather than grant. Even an
// admin with AllScopes gets false.
func TestCanFailsClosedOnUnknownPermission(t *testing.T) {
	admin := wide([]string{"admin"}, []string{AllScopes})
	for _, bogus := range []string{"", "triggerjobs", "TriggerJobs", "trigger_jobs", "deleteEverything"} {
		if admin.Can(bogus, AllScopes) {
			t.Errorf("Can(%q) granted for an unknown permission name", bogus)
		}
		if admin.CanAgency(bogus, "") {
			t.Errorf("CanAgency(%q) granted for an unknown permission name", bogus)
		}
	}
	// Sanity: the correctly spelled constant does grant, so the test above is
	// measuring the typo and not a broken fixture.
	if !admin.Can(PermTriggerJobs, AllScopes) {
		t.Fatal("admin cannot trigger — fixture is wrong, the negative cases above prove nothing")
	}
}

// TestCanAllScopesIsStrongerThanRequirePerm pins the difference that makes Can and
// requirePerm NON-interchangeable, so nobody "simplifies" one into the other.
//
// requirePerm evaluates PermsForRoles(roles).Has(perm) with no scope component. A
// scope-restricted admin therefore passes it today — and MUST keep passing it, or
// an install that restricted its admin to one scope can never reach Users & Access
// again to undo that. Can(perm, AllScopes) demands an unrestricted grant and so
// denies the same actor. CanAnywhere is the primitive that matches requirePerm.
func TestCanAllScopesIsStrongerThanRequirePerm(t *testing.T) {
	restricted := wide([]string{"admin"}, []string{"tax"})

	// The legacy predicate, spelled out: this is what requirePerm does today.
	if !PermsForRoles(restricted.Roles).Has(PermManageRoles) {
		t.Fatal("fixture is wrong: a scope-restricted admin must still hold manageRoles")
	}
	if !restricted.CanAnywhere(PermManageRoles) {
		t.Error("CanAnywhere must match requirePerm — it is the safe substitution for a " +
			"route with no object scope; denying here would lock out a scope-restricted admin")
	}
	if restricted.Can(PermManageRoles, AllScopes) {
		t.Error("Can(perm, AllScopes) must remain STRICTER than requirePerm; if this starts " +
			"passing, the documented distinction in identity.go is no longer true")
	}
	if !restricted.Can(PermManageRoles, "tax") {
		t.Error("a scope-restricted admin lost manageRoles on the scope it holds")
	}

	unrestricted := wide([]string{"admin"}, []string{AllScopes})
	if !unrestricted.Can(PermManageRoles, AllScopes) || !unrestricted.CanAnywhere(PermManageRoles) {
		t.Error("an unrestricted admin failed a global permission check")
	}
}

// TestCanUnboundRequiresUnrestricted pins the predicate RB-2/RB-26 need: an object
// with no scope carries no authority of its own, so only an unrestricted actor may
// act on it unbound. This is deliberately the OPPOSITE of Can's Q-F7 behavior,
// which v0.56.0 preserves — the two must not be confused.
func TestCanUnboundRequiresUnrestricted(t *testing.T) {
	restricted := wide([]string{"operator"}, []string{"tax"})
	// Today's rule, preserved: the empty scope is allowed for everyone.
	if !restricted.Can(PermTriggerJobs, "") {
		t.Error("v0.56.0 must preserve Q-F7 — an empty effective scope is allowed for all")
	}
	// The rule RB-26 will apply instead.
	if restricted.CanUnbound(PermTriggerJobs) {
		t.Error("CanUnbound must deny a scope-restricted actor — that is the whole point of RB-26")
	}

	unrestricted := wide([]string{"operator"}, []string{AllScopes})
	if !unrestricted.CanUnbound(PermTriggerJobs) {
		t.Error("an unrestricted operator must still run unscoped jobs unbound")
	}
	viewer := wide([]string{"viewer"}, []string{AllScopes})
	if viewer.CanUnbound(PermTriggerJobs) {
		t.Error("CanUnbound must still require the verb, not just unrestricted scope")
	}
}

// TestCanAgencyMatchesAgencyShapedGrants — CanAgency for the entities isolated by
// AGENCY rather than by scope: SSH keys and runners have no scope column at all, and
// secrets/variables carry membership too.
//
// Post-RB-15 an agency-shaped grant matches its own agency. An EMPTY membership set
// (agencyID "") stays unrestricted-only per RB-Q14: absence of membership means the
// entity is global infrastructure every department consumes, so one department's
// admin must not rewrite or reveal it.
func TestCanAgencyMatchesAgencyShapedGrants(t *testing.T) {
	cases := []struct {
		name     string
		id       Identity
		agencyID string
		want     bool
	}{
		{"unrestricted admin, named agency", wide([]string{"admin"}, []string{AllScopes}), "ag-tax", true},
		{"agency-shaped grant matches its agency", Identity{Grants: []RoleGrant{
			{Role: "admin", Agency: "ag-tax", Scopes: []string{"tax"}}}}, "ag-tax", true},
		{"agency-shaped grant does NOT match another agency", Identity{Grants: []RoleGrant{
			{Role: "admin", Agency: "ag-tax", Scopes: []string{"tax"}}}}, "ag-fin", false},
		{"agency-shaped grant is not enough for empty membership (RB-Q14)", Identity{Grants: []RoleGrant{
			{Role: "admin", Agency: "ag-tax", Scopes: []string{"tax"}}}}, "", false},
		{"unrestricted admin, empty membership", wide([]string{"admin"}, []string{AllScopes}), "", true},
		{"restricted admin, named agency", wide([]string{"admin"}, []string{"tax"}), "ag-tax", false},
		{"restricted admin, empty membership (RB-Q14)", wide([]string{"admin"}, []string{"tax"}), "", false},
		{"unrestricted viewer lacks the verb", wide([]string{"viewer"}, []string{AllScopes}), "ag-tax", false},
		{"no roles", Identity{}, "ag-tax", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.CanAgency(PermManageEnvVars, tc.agencyID); got != tc.want {
				t.Errorf("CanAgency(manageEnvVars, %q) = %v, want %v", tc.agencyID, got, tc.want)
			}
		})
	}
}

// TestPermissionsOrIsCompleteOverEveryField guards the union against a field added
// to Permissions without a corresponding line in Or — the failure mode that would
// silently drop a permission during the OR-union. It works by setting each field
// via Has/the constant list rather than by hand.
func TestPermissionsOrIsCompleteOverEveryField(t *testing.T) {
	// Built by folding With over the name list, not hand-written — which is what
	// the comment above always claimed and what AF-2 made true. A hand-written
	// literal silently stops covering the struct the moment a permission is added:
	// `all` simply lacks the new field, so Or and Has are both asked an
	// all-false question about it and answer correctly. Folding closes that:
	//   · missing from With → the field stays false → Has catches it
	//   · missing from Has  → With set it, Has says false → caught
	//   · missing from Or   → zero.Or(all) differs from all → caught
	var all Permissions
	for _, n := range PermissionNames {
		all = all.With(n, true)
	}
	// A field added to the struct but never named in PermissionNames is invisible
	// to every loop above, so compare against the struct itself.
	if got, want := reflect.TypeFor[Permissions]().NumField(), len(PermissionNames); got != want {
		t.Fatalf("Permissions has %d fields but PermissionNames lists %d — a permission "+
			"was added to the struct without naming it", got, want)
	}
	var zero Permissions
	if got := zero.Or(all); got != all {
		t.Fatalf("Or(zero, all) = %+v, want all-true — a field is missing from Or", got)
	}
	if got := all.Or(zero); got != all {
		t.Fatalf("Or(all, zero) = %+v, want all-true — a field is missing from Or", got)
	}
	// Every named permission must be reachable through Has, or Can can never
	// grant it. A field added without a Has case fails here.
	names := PermissionNames
	for _, n := range names {
		if !all.Has(n) {
			t.Errorf("Has(%q) = false on an all-true Permissions — missing switch case", n)
		}
	}
	if len(names) != 7 {
		t.Fatalf("permission list has %d entries; update the OpenAPI Role schema, the roles "+
			"table columns (migration 800 + 990) and this test together", len(names))
	}
}

// TestPermsForRolesMatchesMatrix pins the built-in matrix values themselves, so the
// Phase-1 move to keyed literals (and the Phase-1 field deletions) cannot silently
// permute a role's grants — the exact hazard the old positional literals carried.
func TestPermsForRolesMatchesMatrix(t *testing.T) {
	want := map[string]Permissions{
		"admin": {TriggerJobs: true, KillJobs: true, ManageEnvVars: true,
			PublishSchedule: true, ConfigureApp: true, ManageRoles: true},
		"approver": {TriggerJobs: true, KillJobs: true, PublishSchedule: true},
		"operator": {TriggerJobs: true, KillJobs: true},
		// viewer holds NOTHING after v0.56.1: its only permission was
		// viewDashboard, which RB-Q9 deletes. Visibility is not a verb.
		"viewer": {},
	}
	for _, r := range builtinRoles() {
		w, ok := want[r.Name]
		if !ok {
			t.Errorf("unexpected built-in role %q", r.Name)
			continue
		}
		if r.Permissions != w {
			t.Errorf("role %q permissions = %+v, want %+v", r.Name, r.Permissions, w)
		}
	}
	if len(builtinRoles()) != len(want) {
		t.Errorf("built-in role count = %d, want %d", len(builtinRoles()), len(want))
	}
	// The union really unions: operator ∪ approver picks up publishSchedule.
	if got := PermsForRoles([]string{"operator", "approver"}); !got.PublishSchedule {
		t.Error("PermsForRoles did not union publishSchedule from approver")
	}
}
