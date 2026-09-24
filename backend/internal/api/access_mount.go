package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// mountAccess registers the Users & Roles surface (LB3): the roles CRUD (RB-8)
// and the RB-4 pre-flight. The legacy AD-group→role mappings and
// scope-restrictions endpoints were retired in v0.57.8 (RB-19 step 4) along with
// their tables — access_grants_mount.go has been the live model since RB-15.
//
// All routes here require the ManageRoles permission (admin-only) — these ARE
// the privilege-granting surface, so reads and writes alike are gated (PP-B1):
//
//	GET    /api/v1/roles                        (ManageRoles)
//
// Role canonicalization: roles are stored lowercase everywhere in this codebase
// (seed, auth.rolePrecedence, the dev identity, header.go's admin floor). The
// frozen OpenAPI enum is Capitalized for display, but the running authz system
// matches role strings exactly against the lowercase values the resolver emits.
// To make UI edits actually take effect at login, these endpoints accept role
// names case-insensitively and persist/return them canonicalized to lowercase.
func (s *Server) mountAccess(mux *http.ServeMux) {
	// PP-B1: these routes ARE the privilege-granting surface — a role edit changes
	// what every holder of that role can do on the next request. Gate every read
	// and write behind ManageRoles (admin-only per builtinRoles); the reads expose
	// the permission matrix, so they are gated too.
	mux.Handle("GET /api/v1/roles", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleListRoles)))
	mux.Handle("POST /api/v1/roles", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleCreateRole)))
	mux.Handle("PUT /api/v1/roles/{roleName}", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleUpdateRole)))
	mux.Handle("DELETE /api/v1/roles/{roleName}", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleDeleteRole)))

	// RB-4 pre-flight report — read-only, ManageRoles. What WOULD change if the
	// departmental-RBAC switches were flipped today, computed against live data.
	// Ship and read this BEFORE v0.56.4 (RB-2/RB-26) and again before v0.56.5
	// (RB-15): those two releases narrow real users' access, and this is what
	// turns that from a discovery into a decision. Mirrors GET /agency-preflight.
	mux.Handle("GET /api/v1/rbac-preflight", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleRbacPreflight)))
}

// handleRbacPreflight serves the RB-4 report: who loses what, and which data must
// be curated first, if the departmental-RBAC predicates were switched on now.
//
// This exists for the same reason agency-preflight does, and the phasing is copied
// from it deliberately: the tightening releases are separated from the seam
// release so this can be read against REAL production data rather than discovered
// in production. An empty report on a fresh database is expected and correct —
// read UsersEvaluated/MappingsEvaluated alongside the findings, because zero there
// means the report had nothing to bite on, not that the switch is safe.
func (s *Server) handleRbacPreflight(w http.ResponseWriter, r *http.Request) {
	rep, err := settings.RbacPreflight(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
}

// The role/permission matrix moved to internal/auth in v0.56.0 (RB-1): the
// enforcement seam Identity.Can needs it, Identity lives there, and internal/api
// imports internal/auth — so the dependency could only point that way. The names
// below are ALIASES, not copies, so all 75 requirePerm call sites, the predicate
// signatures, and the JSON wire shape are unchanged by the move.
//
// rolePermissions is a type alias (=), not a defined type: predicates declared as
// func(rolePermissions) bool stay assignable from auth.Permissions values.

type rolePermissions = auth.Permissions

func canonRole(role string) string { return auth.CanonRole(role) }

// ── Roles ────────────────────────────────────────────────────────────────────

type roleResp = auth.Role

func permsForRoles(roles []string) rolePermissions { return auth.PermsForRoles(roles) }

// Permission predicates for requirePerm. Named for readable call sites.
func permManageRoles(p rolePermissions) bool     { return p.ManageRoles }
func permConfigureApp(p rolePermissions) bool    { return p.ConfigureApp }
func permManageEnvVars(p rolePermissions) bool   { return p.ManageEnvVars }
func permPublishSchedule(p rolePermissions) bool { return p.PublishSchedule }

// requirePerm gates a route behind a permission predicate evaluated against the
// caller's matrix permissions (PP-B1). It chains RequireSession → RequireCSRF →
// permission check, mirroring requireCompose. Unlike RequireRole (single role),
// this expresses permissions held by more than one role — e.g. PublishSchedule
// = admin OR approver. RequireCSRF passes GET/HEAD/OPTIONS through, so the same
// helper safely gates sensitive reads (Q2) as well as writes.
//
// name is the permission's JSON name from rolePermissions — the SAME string
// /capabilities reports. It exists because the predicate erases the permission
// at the call boundary: `has` is an opaque func, so the 403 could not say what
// was required and the audit row (LU-9) would have had an empty target. Keeping
// it in sync with the struct tag is what lets an auditor join a denial against
// the capability set the operator was actually shown.
func (s *Server) requirePerm(name string, has func(rolePermissions) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return s.auth.RequireSession(s.auth.RequireCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := auth.IdentityFrom(r.Context())
			if !ok {
				// Unreachable while RequireSession runs ahead of this — which is
				// exactly why it is audited (LU-9). If it ever fires, the session
				// middleware has been bypassed, and that is a finding, not a 401.
				if s.auth != nil {
					s.auth.AuditDenied(r, "", "unauthenticated", name,
						auditDetails(r, "permission guard reached without an identity"))
				}
				httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
				return
			}
			if !has(permsForRoles(id.Roles)) {
				if s.auth != nil {
					s.auth.AuditDenied(r, id.Email, "insufficient_permission", name,
						auditDetails(r, "insufficient permissions"))
				}
				// RB-25: name the permission. "insufficient permissions" is
				// unactionable — the operator cannot tell whether to ask for a role
				// change, a scope grant, or neither. The audit row has carried the
				// permission since LU-9; the message did not, so the one person who
				// most needs it could not see it. requireCan names the permission AND
				// the failing scope; this route has no object scope to name.
				httpx.Fail(w, http.StatusForbidden, "forbidden",
					"insufficient permissions: "+name+" is required")
				return
			}
			next.ServeHTTP(w, r)
		})))
	}
}

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, http.StatusOK, auth.AllRoles())
}

// roleInput is the POST/PUT body. Permissions arrive as the same fixed object the
// Role schema emits, so a round-trip of GET → edit → PUT is lossless.
type roleInput struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Rank        int              `json:"rank"`
	Permissions auth.Permissions `json:"permissions"`
}

// writeRole upserts a role row and refreshes the process cache. Shared by create
// and update so the column list exists once.
func (s *Server) writeRole(ctx context.Context, name string, in roleInput, builtin bool, actor string, create bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	p := in.Permissions
	b := 0
	if builtin {
		b = 1
	}
	var err error
	if create {
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO roles (name, description, builtin, rank,
			                   trigger_jobs, kill_jobs, manage_env_vars,
			                   publish_schedule, configure_app, manage_roles, compose,
			                   created_by, created_at, last_modified_by, last_modified_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			name, in.Description, b, in.Rank,
			p.TriggerJobs, p.KillJobs, p.ManageEnvVars,
			p.PublishSchedule, p.ConfigureApp, p.ManageRoles, p.Compose,
			actor, now, actor, now)
	} else {
		// builtin is deliberately NOT updatable: it is what makes a role
		// undeletable, so letting a PUT clear it would be a way to delete admin.
		_, err = s.db.ExecContext(ctx, `
			UPDATE roles SET description=?, rank=?,
			                 trigger_jobs=?, kill_jobs=?, manage_env_vars=?,
			                 publish_schedule=?, configure_app=?, manage_roles=?, compose=?,
			                 last_modified_by=?, last_modified_at=?
			WHERE name=?`,
			in.Description, in.Rank,
			p.TriggerJobs, p.KillJobs, p.ManageEnvVars,
			p.PublishSchedule, p.ConfigureApp, p.ManageRoles, p.Compose,
			actor, now, name)
	}
	if err != nil {
		return err
	}
	return auth.RefreshRoles(ctx, s.db)
}

// requireRoleTemplateAdmin gates WRITES to role templates on an unrestricted
// grant (AF-3, AF3-D1a). A role is a SHARED object: editing `operator` changes
// it for every agency at once, so it is not a departmental administrator's to
// change — and if it were, delegation would be circular. A delegate could add
// configureApp to some role and then grant that role to a group they belong to,
// passing requireGrantWritable's amplification check because by then they hold
// the permission the role they just edited gave them.
//
// READS stay on manageRoles alone. The Users & Access screen must remain
// reachable by a scope-restricted admin (identity.go's lockout note): locking
// them out of the screen that fixes their own grants is unrecoverable, and
// seeing the role list grants nothing.
func (s *Server) requireRoleTemplateAdmin(w http.ResponseWriter, r *http.Request, id auth.Identity) bool {
	if id.Unrestricted() {
		return true
	}
	if s.auth != nil {
		s.auth.AuditDenied(r, id.Email, "insufficient_permission", auth.PermManageRoles,
			auditDetails(r, "role templates are shared across agencies; only an unrestricted administrator may edit them"))
	}
	httpx.Fail(w, http.StatusForbidden, "forbidden",
		"role definitions are shared by every agency — only an unrestricted administrator may change them. "+
			"You can still grant and revoke the existing roles for your agencies.")
	return false
}

func (s *Server) handleCreateRole(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireRoleTemplateAdmin(w, r, id) {
		return
	}
	var in roleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	name := canonRole(in.Name)
	if name == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "a role name is required")
		return
	}
	// Names are canonicalized lowercase at the boundary (see the mountAccess note),
	// so "Tax-Operator" and "tax-operator" are the same role and cannot both exist.
	if auth.RoleValid(name) {
		httpx.Fail(w, http.StatusConflict, "conflict", "role "+name+" already exists")
		return
	}
	if err := s.writeRole(r.Context(), name, in, false, id.Email, true); err != nil {
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, id.Email, "Roles", "created", name, grantSummaryPerms(in.Permissions))
	s.auth.RevokeOtherSessions(w, r) // RB-10: a role's permissions change everyone's authz
	role, _ := auth.LookupRole(name)
	httpx.JSON(w, http.StatusCreated, role)
}

func (s *Server) handleUpdateRole(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireRoleTemplateAdmin(w, r, id) {
		return
	}
	name := canonRole(r.PathValue("roleName"))
	existing, found := auth.LookupRole(name)
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "role not found")
		return
	}
	var in roleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// The lockout floor: admin must be STRUCTURALLY unable to lose manageRoles.
	// Without this an admin can clear the one permission that reaches this endpoint
	// and no one can ever restore it — there is no recovery path short of editing
	// the database by hand. Built-in roles stay editable otherwise, deliberately.
	if name == auth.AdminRole && !in.Permissions.ManageRoles {
		httpx.Fail(w, http.StatusConflict, "last_admin_lockout",
			"the admin role cannot give up manageRoles — nothing could restore it")
		return
	}
	// Same floor, one level up: SOME role must retain manageRoles.
	if !in.Permissions.ManageRoles && existing.Permissions.ManageRoles && !anyOtherRoleManagesRoles(name) {
		httpx.Fail(w, http.StatusConflict, "last_admin_lockout",
			"refusing to leave no role holding manageRoles")
		return
	}
	if err := s.writeRole(r.Context(), name, in, existing.Builtin, id.Email, false); err != nil {
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, id.Email, "Roles", "updated", name, grantSummaryPerms(in.Permissions))
	s.auth.RevokeOtherSessions(w, r) // RB-10
	role, _ := auth.LookupRole(name)
	httpx.JSON(w, http.StatusOK, role)
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	if !s.requireRoleTemplateAdmin(w, r, id) {
		return
	}
	name := canonRole(r.PathValue("roleName"))
	existing, found := auth.LookupRole(name)
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "role not found")
		return
	}
	if existing.Builtin {
		httpx.Fail(w, http.StatusConflict, "conflict", "built-in roles cannot be deleted")
		return
	}
	// Refuse while referenced, mirroring agency deletion (409 agency_in_use). The
	// alternative — cascade — would silently strip access from users whose only
	// role this was, which is exactly the kind of quiet revocation this whole plan
	// exists to make visible. access_grants.role has NO foreign key: SQLite adds
	// FKs only via table rebuild and there is no role-rename endpoint, so this
	// app-level guard is the deliberate whole of the referential integrity — do
	// not "fix" it into a rebuild migration.
	//
	// v0.57.7 removed the matching check against ad_group_mappings. It had become
	// an UNCLEARABLE blocker: the legacy card was the only way to delete a mapping,
	// and with the card gone the 409 said "remove those mappings first" about a
	// screen that no longer exists. Guarding referential integrity for a table that
	// decides nothing is not integrity — it is a locked door in front of an empty
	// room. Dangling legacy rows are harmless (nothing reads them for access) and
	// the pre-flight still reports them until the drop migration clears the table.
	var grantRefs int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM access_grants WHERE role = ?`, name).Scan(&grantRefs); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if grantRefs > 0 {
		httpx.Fail(w, http.StatusConflict, "role_in_use",
			"role is referenced by "+strconv.Itoa(grantRefs)+" access grant(s); remove those grants first")
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM roles WHERE name = ?`, name); err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	// The companion `DELETE FROM scope_restrictions` went with that table (RB-19,
	// v0.57.8). Nothing else needs cleaning: the guard above refuses while any
	// access grant references the role, so by this line there are none to orphan.
	if err := auth.RefreshRoles(r.Context(), s.db); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, id.Email, "Roles", "deleted", name, "")
	s.auth.RevokeOtherSessions(w, r) // RB-10
	w.WriteHeader(http.StatusNoContent)
}

// anyOtherRoleManagesRoles reports whether some role OTHER than the named one
// carries manageRoles.
func anyOtherRoleManagesRoles(except string) bool {
	for _, r := range auth.AllRoles() {
		if r.Name != except && r.Permissions.ManageRoles {
			return true
		}
	}
	return false
}

// grantSummaryPerms renders a permission set for the change log, so an audit row
// says what the role can now DO rather than merely that it was edited.
func grantSummaryPerms(p auth.Permissions) string {
	var held []string
	for _, n := range auth.PermissionNames {
		if p.Has(n) {
			held = append(held, n)
		}
	}
	if len(held) == 0 {
		return "no permissions"
	}
	return strings.Join(held, ", ")
}
