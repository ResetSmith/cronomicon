package auth

import "sort"

// Grant resolution (RB-13, the rbac-update plan Phase 2).
//
// AUTHORITATIVE since the RB-15 switch (v0.56.5): every authorization decision —
// Identity.Can/CanAnywhere/CanUnbound/CanAgency — reads the grants an Identity
// carries, and Roles and AllowedScopes are DERIVED unions kept for display and
// visibility. Until v2.3.0 those grants were resolved once, at login, and rode
// the session cookie; they are resolved per request now (LR-78, snapshot.go).
// expandGrants below is the single definition of what a grant row grants; V2-5,
// if built, extends it rather than adding a second. The loader and ResolveGrants
// are in snapshot.go.

// expandGrants turns access_grants rows into the grants an Identity carries:
// deduplicated, each agency-shaped grant expanded to that agency's scopes, in a
// stable order. It is the ONE definition of that step: the per-request snapshot
// and ResolveGrants both end here (snapshot.go).
func expandGrants(rawGrants []grantRow, scopesOf func(agencyID string) []string) []RoleGrant {
	// Deduplicate: two groups mapped to the same (role, where) is one grant.
	seen := map[string]bool{}
	var out []RoleGrant
	for _, r := range rawGrants {
		key := r.role + "\x00" + r.agencyID
		if r.all {
			key = r.role + "\x00*"
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		g := RoleGrant{Role: r.role}
		if r.all {
			g.Scopes = []string{AllScopes}
		} else {
			g.Agency = r.agencyID
			g.Scopes = scopesOf(r.agencyID)
			// An agency with no scopes yet grants nothing. That is correct rather
			// than "everything": an empty MEMBERSHIP set on an entity means "no
			// restriction" (AG-Q1(b)), but an empty grant means zero access (A5).
			// The two conventions look alike and mean opposite things.
			if g.Scopes == nil {
				g.Scopes = []string{}
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Agency < out[j].Agency
	})
	return out
}

// WithBootstrapGrant adds the break-glass grant when the bootstrap admin floor
// has fired, and is the grant-side counterpart of applyBootstrapAdmin.
//
// 🔴 THIS IS THE BREAK-GLASS PATH. CRONOMICON_BOOTSTRAP_ADMIN_GROUP grants admin
// OUTSIDE ad_group_mappings (decision #6 / A.6) precisely so an instance with no
// working role mapping can still be repaired. Grants are resolved from
// access_grants BY AD GROUP, and the bootstrap group has no grant row by
// definition — it is the mechanism for when the tables are wrong. Without this,
// a bootstrapped login resolves to Roles ["admin"] and Grants [] the moment
// RB-15 makes grants authoritative: the role name with zero authority, and no
// way back in. There is no CLI recovery path; the alternative is hand-editing
// SQLite.
//
// It is a "*"-shaped grant, matching what applyBootstrapAdmin's role injection
// resolves to through the scope matrix today — the floor is unscoped admin, so
// the grant must be unscoped too.
//
// Called on every path that applies the role floor. If a future login path
// applies applyBootstrapAdmin without calling this, the escape hatch silently
// stops working in the release where it matters most — which is what
// TestBootstrapGrantSurvivesGrantEvaluation exists to catch.
func WithBootstrapGrant(grants []RoleGrant, bootstrapped bool) []RoleGrant {
	if !bootstrapped {
		return grants
	}
	for _, g := range grants {
		if g.Role == AdminRole && g.Unrestricted() {
			return grants // already covered by a real grant; nothing to add
		}
	}
	return append(grants, RoleGrant{Role: AdminRole, Scopes: []string{AllScopes}})
}

// UnionGrantScopes flattens grants to the visibility list Identity.AllowedScopes
// carries. Visibility stays UNIONED across grants on purpose (§2.2): Alice should
// SEE both Tax and Finance; she just may not act on Tax. Splitting visibility too
// would be a far larger and much riskier change than the one being made, and it is
// not what was asked for.
func UnionGrantScopes(grants []RoleGrant) []string {
	set := map[string]bool{}
	unrestricted := false
	for _, g := range grants {
		for _, s := range g.Scopes {
			if s == AllScopes {
				unrestricted = true
			}
			set[s] = true
		}
	}
	if unrestricted {
		// "*" is EXCLUSIVE in the A5 model — never mixed with named scopes.
		return []string{AllScopes}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// UnionGrantRoles flattens grants to the role list Identity.Roles carries, used
// for display and for the bootstrap-admin floor.
func UnionGrantRoles(grants []RoleGrant) []string {
	set := map[string]bool{}
	for _, g := range grants {
		set[g.Role] = true
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}
