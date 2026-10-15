package auth

import "testing"

// A grant of an agency's scope covers that agency's scope of the name and not
// another agency's (§0a, IN-5; Phase I2): with scope names unique within an
// agency only, Tax's grant on `web` reads Tax's `web`, not Finance's.
func TestAGrantOfAnAgencysScopeDoesNotCoverAnotherAgencys(t *testing.T) {
	tax := Identity{
		Email:         "tax@example.org",
		AllowedScopes: []string{"web"},
		Grants:        []RoleGrant{{Role: "operator", Agency: "ag-tax", Scopes: []string{"web"}}},
	}
	if !ScopeReadableIn(tax, "ag-tax", "web") || !ScopeWritableIn(tax, "ag-tax", "web") || !tax.CanIn(PermTriggerJobs, "ag-tax", "web") {
		t.Error("Tax's grant does not reach Tax's web")
	}
	if ScopeReadableIn(tax, "ag-fin", "web") || ScopeWritableIn(tax, "ag-fin", "web") || tax.CanIn(PermTriggerJobs, "ag-fin", "web") {
		t.Error("Tax's grant reaches Finance's web")
	}
	// What a list filters by says the same.
	pairs, byName, unrestricted := ReadableScopes(tax)
	if unrestricted || len(byName) != 0 || len(pairs) != 1 || pairs[0] != [2]string{"ag-tax", "web"} {
		t.Errorf("ReadableScopes = %v %v %v, want Tax's web alone", pairs, byName, unrestricted)
	}
	// A grant that names no agency, and an identity with scopes and no grants,
	// read by the name, as before.
	legacy := Identity{Email: "old@example.org", AllowedScopes: []string{"web"}}
	if !ScopeReadableIn(legacy, "ag-fin", "web") {
		t.Error("an identity with scopes and no grants lost its scope")
	}
	if _, byName, _ := ReadableScopes(legacy); len(byName) != 1 || byName[0] != "web" {
		t.Errorf("ReadableScopes of an identity with no grants = %v, want web by name", byName)
	}
	root := Identity{Email: "root@example.org", AllowedScopes: []string{AllScopes}, Grants: []RoleGrant{{Role: "admin", Agency: "*", Scopes: []string{AllScopes}}}}
	if _, _, unrestricted := ReadableScopes(root); !unrestricted {
		t.Error("an unrestricted identity is filtered")
	}
}
