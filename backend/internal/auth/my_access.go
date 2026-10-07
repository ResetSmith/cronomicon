package auth

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// "My access" (LR-87, GET /api/v1/me/access).
//
// The first question an agency's own users have is "why can't I?", and until
// v2.3.0 only an administrator could answer it, from the Honest View. This
// answers it for the caller alone: the groups their sign-in carried, what each
// grant those groups hold lets them do and where, and the groups that matched
// no grant at all — which is the usual reason for "I was added to the group and
// nothing changed" (a grant's group name is matched exactly, case included).
//
// It reports the grants THIS request was authorised with, taken from the
// identity on the context. Which of the caller's groups holds each grant, and
// the agency names, are read from the snapshot a moment later; a grant row
// removed in between shows with no group beside it, and nothing worse.

// myAccessGrant is one (role, where) pair with what it carries.
type myAccessGrant struct {
	Role string `json:"role"`
	// AllScopes marks an all-agencies grant: the caller holds Role everywhere.
	AllScopes bool `json:"allScopes"`
	// AgencyID and AgencyName name the agency the grant covers. Empty exactly
	// when AllScopes is true.
	AgencyID   string `json:"agencyId,omitempty"`
	AgencyName string `json:"agencyName,omitempty"`
	// Scopes are the scopes that agency holds now. An agency with none grants
	// nothing, which is worth seeing: the grant exists and reaches no scope.
	Scopes []string `json:"scopes"`
	// Permissions are the permissions Role carries, in wire order.
	Permissions []string `json:"permissions"`
	// Groups are the caller's groups that hold this grant: the groups an access
	// grant names, or the bootstrap group for the break-glass grant.
	Groups []string `json:"groups"`
	// Origin is "group" for a grant an access-grant row supplies, "bootstrap"
	// for the break-glass administrator grant of CRONOMICON_BOOTSTRAP_ADMIN_GROUP,
	// and "dev" for the developer login.
	Origin string `json:"origin"`
}

type myAccess struct {
	Email  string          `json:"email"`
	Name   string          `json:"name"`
	Groups []string        `json:"groups"`
	Grants []myAccessGrant `json:"grants"`
	// UnmatchedGroups are the caller's groups that no access grant names.
	UnmatchedGroups []string `json:"unmatchedGroups"`
}

// MyAccess serves GET /api/v1/me/access. Any session; about the caller only.
func (s *Service) MyAccess(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	snap, err := s.snapshot(r.Context())
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "role_resolution_failed", "could not resolve access")
		return
	}
	isDev := s.devAuth && id.Email == devIdentity().Email
	// The break-glass floor is applied on the trusted-header path only, to a
	// member of the configured group (applyBootstrapAdmin).
	bootstrapGroup := ""
	if s.mode == config.AuthModeTrustedHeader && s.bootstrapAdminGroup != "" {
		for _, g := range id.Groups {
			if strings.EqualFold(g, s.bootstrapAdminGroup) {
				bootstrapGroup = g
			}
		}
	}

	out := myAccess{
		Email: id.Email, Name: id.DisplayName,
		Groups:          append([]string{}, id.Groups...),
		Grants:          make([]myAccessGrant, 0, len(id.Grants)),
		UnmatchedGroups: []string{},
	}
	sort.Strings(out.Groups)
	for _, g := range out.Groups {
		// A group that supplies access without a grant row is not "unmatched":
		// the bootstrap group, and the developer login's own.
		if len(snap.byGroup[g]) == 0 && g != bootstrapGroup && !isDev {
			out.UnmatchedGroups = append(out.UnmatchedGroups, g)
		}
	}
	for _, g := range id.Grants {
		v := myAccessGrant{
			Role: g.Role, AllScopes: g.Unrestricted(),
			Scopes: []string{}, Permissions: []string{}, Groups: []string{},
		}
		if !v.AllScopes {
			v.AgencyID = g.Agency
			v.AgencyName = snap.agencyNames[g.Agency]
			if v.AgencyName == "" {
				v.AgencyName = g.Agency // a deleted agency: show the id, not a blank
			}
			v.Scopes = append(v.Scopes, g.Scopes...)
		}
		perms := PermsForRoles([]string{g.Role})
		for _, name := range PermissionNames {
			if perms.Has(name) {
				v.Permissions = append(v.Permissions, name)
			}
		}
		for _, group := range out.Groups {
			if slices.ContainsFunc(snap.byGroup[group], func(row grantRow) bool {
				return row.role == g.Role && row.all == v.AllScopes && (row.all || row.agencyID == g.Agency)
			}) {
				v.Groups = append(v.Groups, group)
			}
		}
		// "bootstrap" is claimed only for the grant the floor adds — admin on
		// every agency, for a member of the bootstrap group — never as the
		// fallback for a grant whose row has just been removed.
		switch {
		case len(v.Groups) > 0:
			v.Origin = "group"
		case isDev:
			v.Origin = "dev"
		case bootstrapGroup != "" && v.AllScopes && g.Role == AdminRole:
			v.Origin = "bootstrap"
			v.Groups = append(v.Groups, bootstrapGroup)
		default:
			v.Origin = "group"
		}
		out.Grants = append(out.Grants, v)
	}
	httpx.JSON(w, http.StatusOK, out)
}
