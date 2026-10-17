package auth

import "net/http"

// asRolesAdmin is req as a global administrator for manageRoles, who reads
// every recent login (2.5.0 C2, question 68: the list is an administrator's,
// and an agency's administrator's is trimmed to their agencies).
func asRolesAdmin(req *http.Request) *http.Request {
	return req.WithContext(WithIdentity(req.Context(), Identity{
		Roles:  []string{"admin"},
		Grants: []RoleGrant{{Role: "admin", Scopes: []string{AllScopes}}},
	}))
}
