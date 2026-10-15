package auth

import (
	"slices"
)

// AllScopes is the sentinel grant meaning "this actor may reach EVERY scope"
// (A5 fix). It resolves the historical overload where an EMPTY allowed-scopes set
// stood for BOTH "unrestricted" and "restricted to zero scopes" — the trap that let
// clearing a role's grants silently UN-restrict it. Under the new model the tri-state
// is explicit:
//
//	grants contain "*"  ⇒ UNRESTRICTED (reaches every scope)
//	grants are a set     ⇒ RESTRICTED to exactly those scopes (incl. a scoped admin)
//	grants are EMPTY     ⇒ ZERO scope access (deny; a GLOBAL "" row stays readable)
//
// "Unrestricted" is thus a role-agnostic, data-derived signal (see Identity.Unrestricted),
// not a property of the role name — a scope-restricted admin stays restricted, which
// is what the injection RBAC controls (H3/M1/M3/M4) depend on. See
// the scoping-fix plan.
const AllScopes = "*"

// ScopeReadableIn reports whether the actor may READ a row in the scope named
// `scope` of the agency `agency`. An unrestricted actor (holds "*") sees
// everything; a GLOBAL row (scope "", the NULL/” convention) is always readable;
// otherwise a grant OF THAT AGENCY must cover the scope. An actor with no grants
// can read only GLOBAL rows. This and ScopeWritableIn are the single source of
// truth for A5 scope access — guards route through them and never re-derive
// "empty ⇒ all" inline.
//
// A scope's name is unique within an agency only (I2, IN-5): until then the
// question was asked of the name alone (ScopeReadable), against the union of
// every grant's scopes, and a grant of Tax on `web` would have read Finance's
// `web` once both existed. Every caller says whose scope it asks of.
func ScopeReadableIn(id Identity, agency, scope string) bool {
	if id.Unrestricted() || scope == "" {
		return true
	}
	return grantCoversIn(id, agency, scope)
}

// ScopeWritableIn reports whether the actor may WRITE (create / move) a row INTO
// the scope `targetScope` of the agency `agency`. Unlike ScopeReadableIn, a
// RESTRICTED actor may NOT write to the GLOBAL scope (""), because a global row is
// visible in — and injected into — every scope, which is beyond a restricted
// actor's reach. An unrestricted actor (holds "*") may write anywhere; an actor
// with no grants may write nowhere.
func ScopeWritableIn(id Identity, agency, targetScope string) bool {
	if id.Unrestricted() {
		return true
	}
	return targetScope != "" && grantCoversIn(id, agency, targetScope)
}

// grantCoversIn: one of the actor's grants is of the agency and covers the scope.
//
// An empty agency is a row that names no owner (a secret, variable or key from
// before 830, shared, owned by nobody): whose scope its scope is cannot be
// told, and it is covered as it was until I2, by any grant that covers the
// name. A grant that names no agency (one synthesized for a service token or
// in a test, not one resolved from access_grants, which carries its agency or
// is unrestricted) covers by name too.
func grantCoversIn(id Identity, agency, scope string) bool {
	grants := id.RoleGrants()
	for _, g := range grants {
		if g.Unrestricted() || ((agency == "" || g.Agency == "" || g.Agency == agency) && slices.Contains(g.Scopes, scope)) {
			return true
		}
	}
	// An identity that carries scopes and no grants (one built by hand, from
	// before grants were authoritative, RB-15) reads by name, as it did.
	return len(grants) == 0 && slices.Contains(id.AllowedScopes, scope)
}

// ReadableScopes is what a list filters rows by, as grantCoversIn would judge
// each row: the (agency, scope) pairs the identity's grants of an agency cover,
// and the scope names it reads in any agency (a grant that names no agency; an
// identity that carries scopes and no grants). unrestricted says it reads every
// row. A row with no scope is Global's and readable by anyone, as before.
func ReadableScopes(id Identity) (pairs [][2]string, byName []string, unrestricted bool) {
	if id.Unrestricted() {
		return nil, nil, true
	}
	grants := id.RoleGrants()
	for _, g := range grants {
		switch {
		case g.Unrestricted():
			return nil, nil, true
		case g.Agency == "":
			byName = append(byName, g.Scopes...)
		default:
			for _, sc := range g.Scopes {
				pairs = append(pairs, [2]string{g.Agency, sc})
			}
		}
	}
	if len(grants) == 0 {
		byName = append(byName, id.AllowedScopes...)
	}
	return pairs, byName, false
}

// ReaderIn is ScopeReadableIn as a CanRead callback for one agency, for the
// packages that take a scope-name predicate (secrets, runref) and ask it of
// rows that are all that agency's.
func ReaderIn(id Identity, agency string) func(scope string) bool {
	return func(scope string) bool { return ScopeReadableIn(id, agency, scope) }
}

// ScopeGrant is the resolved scope decision as a plain value, safe to hand to packages
// that must not import Identity (e.g. internal/secrets). The ZERO VALUE denies every
// scope except GLOBAL read — fail-closed by construction.
type ScopeGrant struct {
	Unrestricted bool
	Allowed      []string
}

// CanRead mirrors ScopeReadable for a resolved grant.
func (g ScopeGrant) CanRead(scope string) bool {
	return g.Unrestricted || scope == "" || slices.Contains(g.Allowed, scope)
}

// CanWrite mirrors ScopeWritable for a resolved grant.
func (g ScopeGrant) CanWrite(scope string) bool {
	return g.Unrestricted || (scope != "" && slices.Contains(g.Allowed, scope))
}

// ResolveAllowedScopes was the A5 single call-site for scope-access resolution
// (union over scope_restrictions). DELETED in RF-8 (the RBAC-fixes plan):
// ResolveGrants (RB-13) is the single resolver since the RB-15 switch — a user's
// scopes are union(Grants[].Scopes) — and RB-13's rule is "do not leave two
// resolvers". V2-5 (user-level overrides), if built, extends ResolveGrants.
