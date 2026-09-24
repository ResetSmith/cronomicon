package auth

import "testing"

// The A5 scope-model fix (the scoping-fix plan) gives "unrestricted" a
// distinct signal — the AllScopes "*" grant — so an EMPTY grant finally means ZERO
// access instead of "all". These tests pin the new tri-state on the source-of-truth
// predicates that every guard now routes through (Phase 2).

func idWith(scopes ...string) Identity { return Identity{AllowedScopes: scopes} }

func TestIdentityUnrestricted(t *testing.T) {
	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"star grant", idWith(AllScopes), true},
		{"star among others", idWith("prod", AllScopes), true},
		{"scoped admin", Identity{Roles: []string{"admin"}, AllowedScopes: []string{"prod"}}, false},
		{"empty is zero not all", idWith(), false},
		{"nil is zero not all", Identity{}, false},
	}
	for _, tc := range cases {
		if got := tc.id.Unrestricted(); got != tc.want {
			t.Errorf("%s: Unrestricted() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestScopeReadableWritableTruthTable(t *testing.T) {
	all := idWith(AllScopes)
	scoped := idWith("prod")
	none := idWith()

	// Unrestricted ("*") reaches everything, read and write, including GLOBAL write.
	for _, s := range []string{"", "prod", "staging"} {
		if !ScopeReadable(all, s) {
			t.Errorf("unrestricted ScopeReadable(%q) = false, want true", s)
		}
		if !ScopeWritable(all, s) {
			t.Errorf("unrestricted ScopeWritable(%q) = false, want true", s)
		}
	}

	// Restricted to {prod}: reads its scope + GLOBAL, writes its scope but NOT GLOBAL.
	if !ScopeReadable(scoped, "prod") || !ScopeReadable(scoped, "") {
		t.Error("scoped actor must read granted scope and GLOBAL")
	}
	if ScopeReadable(scoped, "staging") {
		t.Error("scoped actor must NOT read an ungranted scope")
	}
	if !ScopeWritable(scoped, "prod") {
		t.Error("scoped actor must write its granted scope")
	}
	if ScopeWritable(scoped, "") || ScopeWritable(scoped, "staging") {
		t.Error("scoped actor must NOT write GLOBAL or an ungranted scope")
	}

	// Empty grant ⇒ zero access: the whole point of the fix. GLOBAL read still allowed.
	if !ScopeReadable(none, "") {
		t.Error("empty grant must still read GLOBAL rows")
	}
	if ScopeReadable(none, "prod") {
		t.Error("empty grant must NOT read a scoped row (this was the bug: read as unrestricted)")
	}
	if ScopeWritable(none, "prod") || ScopeWritable(none, "") {
		t.Error("empty grant must NOT write any scope (was: wrote everywhere)")
	}
}

// TestScopeGrantMirrorsIdentity: the transport value handed to non-auth packages
// (its ZERO VALUE fails closed) must decide identically to the Identity predicates.
func TestScopeGrantMirrorsIdentity(t *testing.T) {
	var zero ScopeGrant // restricted-to-nothing by construction
	if !zero.CanRead("") {
		t.Error("zero grant must read GLOBAL")
	}
	if zero.CanRead("prod") || zero.CanWrite("prod") || zero.CanWrite("") {
		t.Error("zero ScopeGrant must deny every scope except GLOBAL read")
	}

	for _, id := range []Identity{idWith(AllScopes), idWith("prod"), idWith()} {
		g := id.Grant()
		for _, s := range []string{"", "prod", "staging"} {
			if g.CanRead(s) != ScopeReadable(id, s) {
				t.Errorf("Grant().CanRead(%q) disagrees with ScopeReadable for %v", s, id.AllowedScopes)
			}
			if g.CanWrite(s) != ScopeWritable(id, s) {
				t.Errorf("Grant().CanWrite(%q) disagrees with ScopeWritable for %v", s, id.AllowedScopes)
			}
		}
	}
}
