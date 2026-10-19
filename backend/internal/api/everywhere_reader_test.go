package api

import (
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// asEverywhereReader is req as an administrator whose grant reaches every
// agency, for a test that calls a list handler directly: since 2.5.0 C1 and
// C2 a list answers by who reads it, and a request with no identity, or an
// identity with no grant, reads nothing.
func asEverywhereReader(req *http.Request) *http.Request {
	return req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{
		Roles:         []string{"admin"},
		AllowedScopes: []string{auth.AllScopes},
		Grants:        []auth.RoleGrant{{Role: "admin", Scopes: []string{auth.AllScopes}}},
	}))
}
