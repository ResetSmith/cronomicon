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

// ScopeReadable reports whether the actor may READ a row in `scope`. An unrestricted
// actor (holds "*") sees everything; a GLOBAL row (scope "", the NULL/” convention)
// is always readable; otherwise the row's scope must be among the actor's grants. An
// actor with no grants can read only GLOBAL rows. This and ScopeWritable are the
// single source of truth for A5 scope access — guards route through them and never
// re-derive "empty ⇒ all" inline.
func ScopeReadable(id Identity, scope string) bool {
	return id.Unrestricted() || scope == "" || slices.Contains(id.AllowedScopes, scope)
}

// ScopeWritable reports whether the actor may WRITE (create / move) a row INTO
// targetScope. Unlike ScopeReadable, a RESTRICTED actor may NOT write to the GLOBAL
// scope (""), because a global row is visible in — and injected into — every scope,
// which is beyond a restricted actor's reach. An unrestricted actor (holds "*") may
// write anywhere; an actor with no grants may write nowhere.
func ScopeWritable(id Identity, targetScope string) bool {
	return id.Unrestricted() || (targetScope != "" && slices.Contains(id.AllowedScopes, targetScope))
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
