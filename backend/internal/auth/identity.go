// Package auth implements operator identity (OIDC / trusted-header SSO), operator sessions
// (T8 cookie + CSRF), runner authentication (T9 bearer), role derivation from
// the AD groups claim (A3.1), and the single-call-site scope resolver (A5).
package auth

import (
	"context"
	"slices"
	"time"
)

// Identity is the authenticated operator carried in the session cookie and the
// request context. Roles are derived from the OIDC groups claim via the
// ad_group_mappings table; AllowedScopes is the resolved A5 role-level access.
type Identity struct {
	Email         string    `json:"email"`
	DisplayName   string    `json:"name"`
	Groups        []string  `json:"groups"`
	Roles         []string  `json:"roles"`
	AllowedScopes []string  `json:"scopes"`
	IssuedAt      time.Time `json:"iat"`
	// LastSeen is the sliding activity stamp behind the sessionPolicy idle cap
	// (FX-E4). Refreshed into the cookie at most once a minute so an active
	// operator's session slides forward without a cookie write per request.
	// Zero on a legacy cookie, which readers must treat as IssuedAt — otherwise
	// the deploy that introduces this field would idle-out every live session
	// at once.
	LastSeen time.Time `json:"seen,omitzero"`
	// Epoch is the server-side session epoch stamped at login (SU-5). A request is
	// rejected when it is below the current epoch, which is bumped on any RBAC change
	// (AD-group mapping / scope grant) — server-side revocation of stale sessions.
	// Legacy cookies (pre-SU-5) decode with Epoch == 0, matching the initial epoch.
	Epoch int `json:"-"`
	// Grants is the per-grant resolution from access_grants (RB-14): which role was
	// granted WHERE, the association the two flat lists above destroy.
	//
	// ⚠️ CARRIED BUT NOT CONSULTED as of v0.56.2. Roles and AllowedScopes remain
	// authoritative and RoleGrants() still synthesizes, so every authorization
	// outcome is unchanged. RB-15 makes this field authoritative — that is the
	// release where the leak closes and multi-role users narrow, and it ships alone
	// after the pre-flight has been read against production.
	//
	// It is stored UNEXPANDED-adjacent: each grant carries its agency id and the
	// scopes that agency held at login. Grants are strictly larger than the two flat
	// lists they replace and the whole Identity is encrypted into the session cookie,
	// so if the 4KB ceiling is ever approached the fix is to drop Scopes from the
	// cookie and re-expand per request — measure before choosing.
	Grants []RoleGrant `json:"grants,omitempty"`
}

// HasRole reports whether the identity holds the named role.
func (i Identity) HasRole(role string) bool {
	return slices.Contains(i.Roles, role)
}

// Unrestricted reports whether the actor may reach EVERY scope. It is derived from
// the resolved grants (which carry the AllScopes "*" sentinel), NOT the role name:
// a scope-restricted admin is restricted, and an empty grant is zero access — never
// unrestricted. This is the A5 signal that replaces the old "empty ⇒ all" overload
// (see the scoping-fix plan).
func (i Identity) Unrestricted() bool {
	return slices.Contains(i.AllowedScopes, AllScopes)
}

// Grant returns the actor's resolved scope decision as a plain ScopeGrant value,
// for handing to packages that must not import Identity.
func (i Identity) Grant() ScopeGrant {
	return ScopeGrant{Unrestricted: i.Unrestricted(), Allowed: i.AllowedScopes}
}

// ── The enforcement seam (RB-1, the rbac-update plan) ───────────────
//
// Authorization runs on two axes that are resolved INDEPENDENTLY and both
// OR-unioned: permissions over roles, scopes over roles. A user in tax-viewers
// (viewer→tax) and finance-operators (operator→finance) resolves to permissions
// {trigger} and scopes {tax, finance} — and can therefore trigger TAX jobs. The
// two axes never meet, so a multi-role user gets the strongest verb applied to the
// widest scope set. Invisible with one department; the default with two dozen.
//
// RoleGrant keeps the association the flat lists destroy: which role granted which
// scope. Can then asks whether ONE grant carries both the permission and the
// scope, which is the whole fix.
//
// In v0.56.0 grants are SYNTHESIZED from the existing Roles × AllowedScopes (one
// grant per role, each carrying the full unioned scope list), so every Can outcome
// is bit-identical to the pre-seam behavior by construction — see
// TestCanMatchesLegacyAuthorization. Phase 2 (RB-15) swaps the synthesis for real
// per-grant rows from access_grants, and THAT is the release where the leak closes
// and multi-role users narrow. Nothing before it changes a single decision.

// RoleGrant is one (role, where) pair: a role's permissions applied to a specific
// set of scopes. Named RoleGrant rather than the plan's "Grant" to avoid colliding
// with the existing ScopeGrant type and Identity.Grant method.
type RoleGrant struct {
	// Role is the lowercase canonical role name supplying the permissions.
	Role string
	// Agency is the agency this grant was authored against, or "" when the grant is
	// unrestricted or (pre-Phase-2) synthesized. Always "" in v0.56.0.
	Agency string
	// Scopes is the expanded scope list this grant covers, or [AllScopes] when the
	// grant is unrestricted.
	Scopes []string
}

// Unrestricted reports whether this single grant reaches every scope.
func (g RoleGrant) Unrestricted() bool { return slices.Contains(g.Scopes, AllScopes) }

// Covers reports whether this grant's "where" includes the named scope, using the
// same semantics as ScopeReadable: the AllScopes sentinel covers everything, and
// the empty scope is the global/unscoped object.
//
// The empty-scope rule is Q-F7 ("a NULL/empty effective scope is always allowed")
// and it is the hole RB-26 closes by requiring restricted actors to bind a scope
// at trigger time. It is preserved verbatim here because v0.56.0 changes no
// decision; RB-26 changes it at the call site, not here.
func (g RoleGrant) Covers(scope string) bool {
	return g.Unrestricted() || scope == "" || slices.Contains(g.Scopes, scope)
}

// RoleGrants returns the actor's grants.
//
// v0.56.0 synthesizes them: one grant per role, each carrying the FULL unioned
// scope list. That reproduces today's cross-product exactly — deliberately, so
// this release is behavior-neutral. RB-14 makes Grants an authoritative field
// resolved at login from access_grants (and bumps the session cookie to _v3,
// since Identity is encrypted whole into it); this method is the seam that lets
// every caller be written against the final shape a release early.
func (i Identity) RoleGrants() []RoleGrant {
	// 🔴 RB-15 (v0.56.5) — THE PREDICATE SWITCH. Grants are authoritative.
	//
	// Until this release the grants were SYNTHESIZED here: one per role, each
	// carrying the full unioned scope list, which reproduced the cross-product
	// exactly. That is what made every earlier release behavior-neutral. Now the
	// resolved grants are returned as-is, so a permission and a scope must come from
	// the SAME grant — and viewer@tax + operator@finance stops being able to trigger
	// Tax jobs.
	//
	// An identity with no grants has no authority. That is the correct answer and
	// the reason every login path was made to populate the field a release early:
	// the trusted-header and OIDC resolvers (ResolveGrants), the bootstrap floor
	// (WithBootstrapGrant — the break-glass path, which has no grant row by
	// definition), and the dev identity, which is constructed rather than resolved.
	return i.Grants
}

// Can reports whether the actor may exercise perm ON the named scope.
//
// It ORs over grants where THE SAME grant both carries the permission (via its
// role's row in the matrix) and covers the scope. That single conjunction — same
// grant, both halves — is the fix: under real grants a viewer@tax + operator@finance
// answers false for Can(PermTriggerJobs, "tax").
//
// ⚠️ Can(perm, AllScopes) is STRICTLY STRONGER than today's requirePerm, and the
// two are NOT interchangeable. requirePerm evaluates PermsForRoles(id.Roles) with
// NO scope component at all, so a scope-restricted admin (AllowedScopes ["tax"])
// passes it; Can(perm, AllScopes) additionally demands an unrestricted grant, so
// that same admin fails. A scope-restricted admin is an explicitly supported
// control (see the R2 note at access_mount.go's scope-restrictions handler), and
// swapping requirePerm to this call would lock such an install out of its own
// Users & Access screen with no recourse.
//
// Use CanAnywhere for the "does the actor hold this verb at all" question that
// requirePerm asks today. The plan's §2.2 sketch says requirePerm should evaluate
// Can(perm, "*"); that is a real narrowing which no release's risk column
// accounts for, and it must be decided deliberately rather than adopted as a
// mechanical substitution.
func (i Identity) Can(perm, scope string) bool {
	for _, g := range i.RoleGrants() {
		if !PermsForRoles([]string{g.Role}).Has(perm) {
			continue
		}
		if scope == AllScopes {
			// A global permission requires a grant that reaches everywhere. Asking
			// "may you do this somewhere" is a different question — that is what
			// /capabilities answers, and it must never gate a route.
			if g.Unrestricted() {
				return true
			}
			continue
		}
		if g.Covers(scope) {
			return true
		}
	}
	return false
}

// CanAnywhere reports whether ANY of the actor's grants carries perm, ignoring
// scope entirely — "may this actor do this somewhere".
//
// This is exactly what requirePerm evaluates today, and it is the honest
// primitive for a route with no object scope. It is also the semantics behind the
// /capabilities flags, which exist for coarse nav gating and must never gate a
// route: an operator scoped to Finance answers true here and still cannot touch a
// Tax job. Use Can(perm, scope) wherever an object scope exists.
func (i Identity) CanAnywhere(perm string) bool {
	for _, g := range i.RoleGrants() {
		if PermsForRoles([]string{g.Role}).Has(perm) {
			return true
		}
	}
	return false
}

// CanUnbound reports whether the actor may exercise perm on an object with NO
// scope at all — an unscoped job, or a run that was never bound to one.
//
// Can() deliberately preserves the Q-F7 rule that an empty scope is allowed for
// everyone, because v0.56.0 changes no decision. But RB-2 and RB-26 both need the
// opposite rule — "the empty effective scope is allowed only for an unrestricted
// actor" — and expressing that as a bare id.Unrestricted() check hand-rolled at
// every call site is precisely the easy-to-forget, fails-open shape this plan
// exists to eliminate. One named predicate, used everywhere, is the point.
//
// Unused in v0.56.0. RB-2/RB-26 call it in v0.56.4; it ships now so that release
// is a substitution rather than a new mechanism arriving alongside a behavioral
// shock.
func (i Identity) CanUnbound(perm string) bool {
	for _, g := range i.RoleGrants() {
		if !PermsForRoles([]string{g.Role}).Has(perm) {
			continue
		}
		if g.Unrestricted() {
			return true
		}
	}
	return false
}

// CanAgency reports whether the actor may exercise perm on an entity whose
// isolation is expressed as AGENCY membership rather than a scope — SSH keys and
// runners have no scope column at all, and secrets/variables carry membership too
// (RB-16 / RB-32).
//
// An entity with an EMPTY membership set is not "member of nothing" but "no agency
// restriction" (AG-Q1(b)), and RB-Q14 resolves that writes and reveals on such an
// entity require an unrestricted actor: absence of membership carries no authority,
// exactly as an unscoped job does not authorize itself under RB-26. Callers pass
// "" for that case.
//
// v0.56.0 has no agency-shaped grants (synthesis produces none), so this returns
// true only for unrestricted actors holding the permission. It is wired to real
// membership by RB-16/RB-32 in Phase 2; it exists now so the seam is complete and
// testable, not because anything calls it yet.
func (i Identity) CanAgency(perm, agencyID string) bool {
	for _, g := range i.RoleGrants() {
		if !PermsForRoles([]string{g.Role}).Has(perm) {
			continue
		}
		if g.Unrestricted() {
			return true
		}
		if agencyID != "" && g.Agency == agencyID {
			return true
		}
	}
	return false
}

// AgenciesFor lists the agency IDs this actor holds perm on through a DEPARTMENTAL
// grant, sorted and de-duplicated. Unrestricted grants contribute nothing — they
// reach every agency without naming one, so there is no id to return, and a caller
// that needs "does this actor reach everywhere?" must ask CanAgency(perm, "").
//
// RA-9: this is what lets a new secret/variable/SSH key INHERIT its creator's
// department instead of being refused for not naming one. Exactly one entry is the
// common departmental case and the only one inheritance can act on unambiguously;
// several means the actor genuinely belongs to more than one department and must
// say which, since silently enrolling all of them would widen the row past what
// they are likely to have meant.
func (i Identity) AgenciesFor(perm string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range i.RoleGrants() {
		if g.Agency == "" || g.Unrestricted() || !PermsForRoles([]string{g.Role}).Has(perm) {
			continue
		}
		if seen[g.Agency] {
			continue
		}
		seen[g.Agency] = true
		out = append(out, g.Agency)
	}
	slices.Sort(out)
	return out
}

type ctxKey int

const (
	identityKey ctxKey = iota
	runnerIDKey
)

// withIdentity returns a context carrying the authenticated operator.
func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// WithIdentity returns a context carrying id, exactly as the auth middleware does
// on an authenticated request. Exported so callers (and tests) that invoke a handler
// directly — bypassing RequireSession — can supply the identity the handler expects.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return withIdentity(ctx, id)
}

// IdentityFrom extracts the authenticated operator from the request context.
// ok is false on unauthenticated requests.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKey).(Identity)
	return id, ok
}
