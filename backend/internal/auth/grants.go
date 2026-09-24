package auth

import (
	"context"
	"database/sql"
	"sort"
)

// Grant resolution (RB-13, the rbac-update plan Phase 2).
//
// AUTHORITATIVE since the RB-15 switch (v0.56.5): ResolveGrants runs at login,
// the result rides Identity.Grants, and every authorization decision —
// Identity.Can/CanAnywhere/CanUnbound/CanAgency — reads those grants. Roles and
// AllowedScopes are DERIVED unions kept for display and visibility. This is the
// single resolver (the legacy pair rolesForGroups + ResolveAllowedScopes is
// retired from the login path; the scope half is deleted); V2-5, if built,
// extends this function rather than adding a second.

// AgencyScopes returns the scope NAMES belonging to an agency. It is the inverse
// of execspec.ScopeAgencies, and it is the expansion that lets a grant be authored
// on an agency and evaluated on a scope: add a scope to Tax next month and every
// Tax grant covers it with no grant edit.
//
// Written here rather than as a second join in the resolver so there is one
// definition of "what is in this agency" on the read side.
func AgencyScopes(ctx context.Context, db *sql.DB, agencyIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(agencyIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(agencyIDs))
	ph := make([]byte, 0, len(agencyIDs)*2)
	for i, a := range agencyIDs {
		args[i] = a
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
	}
	rows, err := db.QueryContext(ctx, `
		SELECT sa.agency_id, s.name
		FROM scope_agencies sa
		JOIN scopes s ON s.id = sa.scope_id
		WHERE sa.agency_id IN (`+string(ph)+`)
		ORDER BY s.name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agencyID, scope string
		if err := rows.Scan(&agencyID, &scope); err != nil {
			return nil, err
		}
		out[agencyID] = append(out[agencyID], scope)
	}
	return out, rows.Err()
}

// ResolveGrants resolves a user's AD groups to their grants, expanding each
// agency-shaped grant to the scopes in that agency.
//
// Group matching is EXACT, mirroring rolesForGroups: ad_group is plain TEXT with
// no COLLATE NOCASE and the claim is trimmed but not case-folded, so a mapping
// stored in a different case grants nothing at login. Folding case here would make
// grants resolve differently from roles on the same request, which is worse than
// the existing inconsistency.
//
// Expansion happens HERE, at login, not per request (RB-Q10): a per-request
// expansion puts a scope_agencies join on every authorized call, and login-time
// expansion is what ResolveAllowedScopes already does. The cost is that adding a
// scope to an agency does not reach live sessions — paid for by bumping the
// session epoch on scope_agencies writes, which is the same machinery every other
// RBAC change uses.
func ResolveGrants(ctx context.Context, db *sql.DB, groups []string) ([]RoleGrant, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	args := make([]any, len(groups))
	ph := make([]byte, 0, len(groups)*2)
	for i, g := range groups {
		args[i] = g
		if i > 0 {
			ph = append(ph, ',')
		}
		ph = append(ph, '?')
	}
	rows, err := db.QueryContext(ctx, `
		SELECT role, COALESCE(agency_id,''), all_scopes
		FROM access_grants
		WHERE ad_group IN (`+string(ph)+`)`, args...)
	if err != nil {
		return nil, err
	}
	type raw struct {
		role, agencyID string
		all            bool
	}
	var rawGrants []raw
	agencySet := map[string]bool{}
	for rows.Next() {
		var r raw
		var all int
		if err := rows.Scan(&r.role, &r.agencyID, &all); err != nil {
			rows.Close()
			return nil, err
		}
		r.role = CanonRole(r.role)
		r.all = all != 0
		rawGrants = append(rawGrants, r)
		if r.agencyID != "" {
			agencySet[r.agencyID] = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // close BEFORE the expansion query — the pool deadlocks on a
	// query issued inside an open cursor (db.maxOpenConns).

	agencyIDs := make([]string, 0, len(agencySet))
	for a := range agencySet {
		agencyIDs = append(agencyIDs, a)
	}
	sort.Strings(agencyIDs)
	expanded, err := AgencyScopes(ctx, db, agencyIDs)
	if err != nil {
		return nil, err
	}

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
			g.Scopes = expanded[r.agencyID]
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
	return out, nil
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
