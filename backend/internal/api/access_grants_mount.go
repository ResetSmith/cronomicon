package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Access grants (RB-18, the rbac-update plan Phase 2).
//
// A grant is `ad_group → role → where`, and `where` is an AGENCY or the "*"
// sentinel (RB-Q1). It replaces the two-axis model — ad_group_mappings for the
// verb, scope_restrictions for the reach — whose independent OR-unions produced
// the cross-product leak: viewer@tax + operator@finance resolved to {trigger} ×
// {tax, finance} and could trigger Tax jobs.
//
// AUTHORITATIVE since RB-15 (v0.56.5): these rows ARE the access model — login
// resolves them onto the session and every Can/CanAgency decision reads them.
// The legacy endpoints (/ad-group-mappings, /scope-restrictions) survive only as
// labelled read-mostly surfaces until RB-19 drops their tables, which is gated on
// v0.56.4/.5 deploying and soaking (the pre-flight still reads them).
//
// All routes are ManageRoles-gated and every write revokes other sessions (SU-5):
// a grant edit changes who may do what on their next request.

type accessGrantResp struct {
	ID       string `json:"id"`
	AdGroup  string `json:"adGroup"`
	Role     string `json:"role"`
	AgencyID string `json:"agencyId,omitempty"`
	// AgencyName is denormalized for display so the grants table does not need a
	// second fetch to render a chip.
	AgencyName     string `json:"agencyName,omitempty"`
	AllScopes      bool   `json:"allScopes"`
	CreatedBy      string `json:"createdBy,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
	LastModifiedBy string `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string `json:"lastModifiedAt,omitempty"`
}

type accessGrantInput struct {
	AdGroup   string `json:"adGroup"`
	Role      string `json:"role"`
	AgencyID  string `json:"agencyId"`
	AllScopes bool   `json:"allScopes"`
}

func (s *Server) mountAccessGrants(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/access-grants", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleListAccessGrants)))
	mux.Handle("POST /api/v1/access-grants", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleCreateAccessGrant)))
	mux.Handle("PUT /api/v1/access-grants/{grantId}", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleUpdateAccessGrant)))
	mux.Handle("DELETE /api/v1/access-grants/{grantId}", s.requirePerm("manageRoles", permManageRoles)(http.HandlerFunc(s.handleDeleteAccessGrant)))
}

func (s *Server) handleListAccessGrants(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT g.id, g.ad_group, g.role, COALESCE(g.agency_id,''), COALESCE(a.name,''), g.all_scopes,
		       COALESCE(g.created_by,''), COALESCE(g.created_at,''),
		       COALESCE(g.last_modified_by,''), COALESCE(g.last_modified_at,'')
		FROM access_grants g
		LEFT JOIN agencies a ON a.id = g.agency_id
		ORDER BY g.ad_group, g.role, a.name`)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	out := []accessGrantResp{}
	for rows.Next() {
		var g accessGrantResp
		var all int
		if err := rows.Scan(&g.ID, &g.AdGroup, &g.Role, &g.AgencyID, &g.AgencyName, &all,
			&g.CreatedBy, &g.CreatedAt, &g.LastModifiedBy, &g.LastModifiedAt); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		g.AllScopes = all != 0
		// Whose it is (2.5.0 C2, CF-1: an agency's administrator listed every
		// agency's grants), then the agency the request works in. A grant on
		// every agency names none, and is Global's (SW-10).
		if !readsAgency(r, g.AgencyID) || !agencyctx.In(r, g.AgencyID) {
			continue
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

// validateGrant enforces the shape invariant the CHECK constraint also enforces,
// so the caller gets a 422 explaining which rule they broke rather than a 500 from
// a constraint violation.
func (s *Server) validateGrant(r *http.Request, in *accessGrantInput) (string, bool) {
	in.AdGroup = strings.TrimSpace(in.AdGroup)
	in.Role = auth.CanonRole(in.Role)
	in.AgencyID = strings.TrimSpace(in.AgencyID)

	if in.AdGroup == "" {
		return "adGroup is required", false
	}
	if !auth.RoleValid(in.Role) {
		return "unknown role: " + in.Role, false
	}
	// Exactly one shape. RB-Q1 chose agency-only + "*" deliberately: one
	// vocabulary, and a narrow need gets a deliberate narrow agency rather than a
	// loose per-scope grant.
	if in.AllScopes == (in.AgencyID != "") {
		return "a grant must name exactly one of an agency or allScopes", false
	}
	if in.AgencyID != "" {
		// LR-9, LR-25: Global is not grantable by itself. It belongs to the global
		// administrators, and the way to be one is an all-agencies grant — a grant
		// "on Global" would reach Global's scopes and nothing else while reading
		// like the keys to the installation.
		if in.AgencyID == agencyid.Global {
			return "Global cannot be named in a grant: use allScopes for a global administrator", false
		}
		var n int
		if err := s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM agencies WHERE id = ?`, in.AgencyID).Scan(&n); err != nil || n == 0 {
			return "unknown agency", false
		}
	}
	return "", true
}

// requireGrantWritable is the per-object half of the manageRoles gate (AF-3,
// the af3-delegated-governance plan). The route gate establishes only
// that the caller administers access SOMEWHERE — CanAnywhere semantics, kept
// deliberately so a scope-restricted admin can still reach Users & Access
// (identity.go's lockout note). This decides whether they may write THIS grant.
//
// An UNRESTRICTED holder is unaffected: they administer everything, as before.
// A DELEGATE — manageRoles on one or more agencies — is bound by three rules,
// and rule 3 is the one that makes delegation safe rather than a one-step
// escalation:
//
//  1. own agencies only: the grant must name an agency they administer;
//  2. no unrestricted grants: "everywhere" is not theirs to give;
//  3. no amplification: every permission the granted ROLE carries must be one
//     they themselves hold IN THAT AGENCY. Without this, a delegate grants a
//     powerful role to a group they belong to and escalates immediately.
//
// Callers apply it to the INCOMING grant and, on edit/delete, to the existing
// row as well — otherwise a delegate could re-point a global admin's grant at
// their own agency, or delete the grant that makes someone else an admin.
//
// A fourth rule, the agency's group list (2.5.0 C5, GA-1), is
// requireGroupListed: a create and both sides of an edit are held to it, a
// delete is not (GA-2).
//
// On refusal it has written the 403 and the audit row; the call site is
// `if !s.requireGrantWritable(...) { return }`.
func (s *Server) requireGrantWritable(w http.ResponseWriter, r *http.Request, id auth.Identity, role, agencyID string, allScopes bool) bool {
	// GC-3: "unrestricted" here must mean an unrestricted grant that itself
	// carries manageRoles. The bare Unrestricted() is permission-blind, so a
	// viewer on all scopes who delegates for one agency skipped all three rules.
	if id.GlobalAdmin(auth.PermManageRoles) {
		return true
	}
	deny := func(msg string) bool {
		if s.auth != nil {
			s.auth.AuditDenied(r, id.Email, "insufficient_permission", auth.PermManageRoles,
				auditDetails(r, msg))
		}
		httpx.Fail(w, http.StatusForbidden, "forbidden", msg)
		return false
	}
	if allScopes {
		return deny("only an unrestricted administrator may grant access to every agency")
	}
	if agencyID == "" || !id.CanAgency(auth.PermManageRoles, agencyID) {
		return deny("you may only administer access for the agencies you administer")
	}
	// Anti-amplification. Compared per PERMISSION rather than by role name so a
	// custom role cannot smuggle authority past a name-based allowlist, and
	// against the actor's authority IN THIS AGENCY rather than anywhere, so
	// holding a verb on Finance does not let them grant it on Tax.
	granted, ok := auth.LookupRole(role)
	if !ok {
		return deny("unknown role: " + role)
	}
	for _, perm := range auth.PermissionNames {
		if granted.Permissions.Has(perm) && !id.CanAgency(perm, agencyID) {
			return deny("you may not grant " + perm + " — you do not hold it in this agency")
		}
	}
	return true
}

// requireGroupListed is the fourth rule for a delegate (2.5.0 C5, GA-1): a
// grant they write names a group a global administrator listed for its
// agency. Without it a delegate could hand their agency's roles to any group
// in the directory ("Domain Users" included); the app cannot look groups up
// (§1.5), so the list is the global administrator's word for which groups
// belong to the agency. A global administrator is not bound (they may grant
// everywhere), and an all-agencies grant is theirs alone already.
//
// `existing` words the refusal for the row as it stands on an edit: such a
// grant keeps working, and its delegate may delete it but not change it
// (GA-2). On refusal it has written the 403 and the audit row.
func (s *Server) requireGroupListed(w http.ResponseWriter, r *http.Request, id auth.Identity, group, agencyID string, allScopes, existing bool) bool {
	if id.GlobalAdmin(auth.PermManageRoles) || allScopes {
		return true
	}
	listed, err := settings.AgencyGroupListed(r.Context(), s.db, agencyID, group)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if listed {
		return true
	}
	name := agencyID
	if ag, err := settings.GetAgency(r.Context(), s.db, agencyID); err == nil && ag != nil {
		name = ag.Name
	}
	msg := "the group " + group + " is not on " + name + "'s list: a global administrator lists the groups an agency's grants may name"
	if existing {
		msg = "this grant names " + group + ", which is not on " + name + "'s list: you may delete it, not change it, until a global administrator lists the group"
	}
	if s.auth != nil {
		s.auth.AuditDenied(r, id.Email, "group_not_listed", auth.PermManageRoles, auditDetails(r, msg))
	}
	httpx.Fail(w, http.StatusForbidden, "group_not_listed", msg)
	return false
}

// loadGrantForWrite reads the grant a write targets, so the EXISTING row can be
// checked alongside the incoming one. A missing row is a 404 written here.
func (s *Server) loadGrantForWrite(w http.ResponseWriter, r *http.Request, grantID string) (adGroup, role, agencyID string, allScopes bool, ok bool) {
	var agency sql.NullString
	var all int
	err := s.db.QueryRowContext(r.Context(),
		`SELECT ad_group, role, agency_id, all_scopes FROM access_grants WHERE id=?`, grantID).Scan(&adGroup, &role, &agency, &all)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "grant not found")
		return "", "", "", false, false
	}
	return adGroup, role, agency.String, all != 0, true
}

func (s *Server) handleCreateAccessGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var in accessGrantInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if msg, ok := s.validateGrant(r, &in); !ok {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", msg)
		return
	}
	// In the agency the request works in, as every creation (2.5.0 C11, defect
	// 87; question 108): a grant on another agency, or on every agency (none
	// named: Global's), was made and then never listed, as the list is
	// narrowed to the agency in use, so it read as if nothing had happened.
	if !s.ownerMatchesContext(w, r, in.AgencyID, "access grant") {
		return
	}
	// AF-3 — may this actor grant THIS role, HERE?
	if !s.requireGrantWritable(w, r, id, in.Role, in.AgencyID, in.AllScopes) ||
		!s.requireGroupListed(w, r, id, in.AdGroup, in.AgencyID, in.AllScopes, false) {
		return
	}
	newID := db.NewID()
	now := time.Now().UTC().Format(time.RFC3339)
	var agency any
	if in.AgencyID != "" {
		agency = in.AgencyID
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes,
		                           created_by, created_at, last_modified_by, last_modified_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		newID, in.AdGroup, in.Role, agency, boolInt(in.AllScopes), id.Email, now, id.Email, now); err != nil {
		// The dedupe index is the likely cause; report it as a conflict rather than
		// a 500 so the UI can say "that grant already exists".
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			httpx.Fail(w, http.StatusConflict, "conflict", "that grant already exists")
			return
		}
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, grantAgency(in), id.Email, "Roles", "created",
		"access grant: "+in.AdGroup, grantWhere(in))
	auth.GrantsChanged() // LR-78: live sessions pick the change up on their next request
	httpx.JSON(w, http.StatusCreated, accessGrantResp{
		ID: newID, AdGroup: in.AdGroup, Role: in.Role, AgencyID: in.AgencyID, AllScopes: in.AllScopes,
		CreatedBy: id.Email, CreatedAt: now, LastModifiedBy: id.Email, LastModifiedAt: now,
	})
}

func (s *Server) handleUpdateAccessGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	grantID := r.PathValue("grantId")
	var in accessGrantInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if msg, ok := s.validateGrant(r, &in); !ok {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", msg)
		return
	}
	// AF-3 — BOTH sides. The grant as it stands must be one this actor could
	// have written, or a delegate could re-point a global admin's grant at their
	// own agency; and the grant it becomes must be too.
	curGroup, curRole, curAgency, curAll, found := s.loadGrantForWrite(w, r, grantID)
	if !found {
		return
	}
	if !s.requireGrantWritable(w, r, id, curRole, curAgency, curAll) {
		return
	}
	// GA-2: a grant naming an unlisted group is its delegate's to delete, not
	// to change, even into a listed one: that is a new grant.
	if !s.requireGroupListed(w, r, id, curGroup, curAgency, curAll, true) {
		return
	}
	if !s.requireGrantWritable(w, r, id, in.Role, in.AgencyID, in.AllScopes) ||
		!s.requireGroupListed(w, r, id, in.AdGroup, in.AgencyID, in.AllScopes, false) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var agency any
	if in.AgencyID != "" {
		agency = in.AgencyID
	}
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE access_grants SET ad_group=?, role=?, agency_id=?, all_scopes=?,
		                         last_modified_by=?, last_modified_at=?
		WHERE id=?`,
		in.AdGroup, in.Role, agency, boolInt(in.AllScopes), id.Email, now, grantID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			httpx.Fail(w, http.StatusConflict, "conflict", "that grant already exists")
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, grantAgency(in), id.Email, "Roles", "updated",
		"access grant: "+in.AdGroup, grantWhere(in))
	auth.GrantsChanged() // LR-78: live sessions pick the change up on their next request
	httpx.JSON(w, http.StatusOK, accessGrantResp{
		ID: grantID, AdGroup: in.AdGroup, Role: in.Role, AgencyID: in.AgencyID, AllScopes: in.AllScopes,
		LastModifiedBy: id.Email, LastModifiedAt: now,
	})
}

func (s *Server) handleDeleteAccessGrant(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	grantID := r.PathValue("grantId")
	// AF-3 — revoking is administering: same authority as writing the row.
	// Not held to the group list (GA-2): a grant naming an unlisted group is
	// its agency's delegates' to delete.
	_, curRole, curAgency, curAll, found := s.loadGrantForWrite(w, r, grantID)
	if !found {
		return
	}
	if !s.requireGrantWritable(w, r, id, curRole, curAgency, curAll) {
		return
	}
	var adGroup string
	grantAgencyID := agencyid.Global // the grant's, read while the row is there (C1)
	_ = s.db.QueryRowContext(r.Context(), `SELECT ad_group, `+agencyid.ColumnOwnerSQL("", "agency_id")+` FROM access_grants WHERE id=?`, grantID).Scan(&adGroup, &grantAgencyID)

	res, err := s.db.ExecContext(r.Context(), `DELETE FROM access_grants WHERE id=?`, grantID)
	if err != nil {
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "grant not found")
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, grantAgencyID, id.Email, "Roles", "deleted", "access grant: "+adGroup, "")
	auth.GrantsChanged() // LR-78: live sessions pick the change up on their next request
	w.WriteHeader(http.StatusNoContent)
}

// grantWhere renders a grant's reach for the change log, so an audit row says
// where the grant applies rather than only that one was edited.
// grantAgency is the agency a grant's audit rows are of (C1): the agency it
// names, Global's for one that reaches every agency.
func grantAgency(in accessGrantInput) string {
	if in.AllScopes || in.AgencyID == "" {
		return agencyid.Global
	}
	return in.AgencyID
}

func grantWhere(in accessGrantInput) string {
	if in.AllScopes {
		return in.Role + " on all scopes"
	}
	return in.Role + " on agency " + in.AgencyID
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
