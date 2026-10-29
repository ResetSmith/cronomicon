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

// A secret's or a variable's global row (scope "") reaches every scope of its
// owner agency and no other agency's runs (2.4.0), so writing one is the owner
// agency's permission holders' (2.5.4, RowScopeWritableIn). Global's row and a
// row with no owner stay a global administrator's, and the permission is asked
// for: unlike ScopeWritableIn's empty-scope arm, reach alone is not enough.
func TestRowScopeWritableInGivesAGlobalRowToItsAgency(t *testing.T) {
	taxAdmin := Identity{
		Email:         "tax-admin@example.org",
		AllowedScopes: []string{"web"},
		Grants:        []RoleGrant{{Role: "admin", Agency: "ag-tax", Scopes: []string{"web"}}},
	}
	taxOperator := Identity{
		Email:         "tax-op@example.org",
		AllowedScopes: []string{"web"},
		Grants:        []RoleGrant{{Role: "operator", Agency: "ag-tax", Scopes: []string{"web"}}},
	}
	root := Identity{
		Email:         "root@example.org",
		AllowedScopes: []string{AllScopes},
		Grants:        []RoleGrant{{Role: "admin", Agency: "*", Scopes: []string{AllScopes}}},
	}
	everywhereViewer := Identity{
		Email:         "ops@example.org",
		AllowedScopes: []string{AllScopes},
		Grants:        []RoleGrant{{Role: "viewer", Agency: "*", Scopes: []string{AllScopes}}},
	}
	for _, c := range []struct {
		name          string
		id            Identity
		agency, scope string
		want          bool
	}{
		{"own agency's global row", taxAdmin, "ag-tax", "", true},
		{"another agency's global row", taxAdmin, "ag-fin", "", false},
		{"Global's row", taxAdmin, "global", "", false},
		{"a row with no owner", taxAdmin, "", "", false},
		{"own agency's scope", taxAdmin, "ag-tax", "web", true},
		{"another agency's scope of the name", taxAdmin, "ag-fin", "web", false},
		{"own agency's global row without the permission", taxOperator, "ag-tax", "", false},
		{"global administrator, an agency's global row", root, "ag-tax", "", true},
		{"global administrator, Global's row", root, "global", "", true},
		{"global administrator, a row with no owner", root, "", "", true},
		{"viewer of everything, an agency's global row", everywhereViewer, "ag-tax", "", false},
		{"viewer of everything, Global's row", everywhereViewer, "global", "", false},
		{"viewer of everything, a row with no owner", everywhereViewer, "", "", false},
	} {
		if got := RowScopeWritableIn(c.id, PermManageEnvVars, c.agency, c.scope); got != c.want {
			t.Errorf("%s: RowScopeWritableIn(%q, %q) = %v, want %v", c.name, c.agency, c.scope, got, c.want)
		}
	}
	// ScopeWritableIn keeps its rule for jobs: the empty scope is an All-scopes
	// grant's, whatever the agency.
	if ScopeWritableIn(taxAdmin, "ag-tax", "") {
		t.Error("ScopeWritableIn gave an agency's grant the empty scope")
	}
}
