package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/ResetSmith/cronomicon/internal/vaultpath"
	"net/http"
	"strconv"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// mountAgencies registers the agency catalog (a network-isolation zone registry)
// and the scope→agency binding (agency-support.md M1). Reads are session-gated so
// scope/runner views can show agency labels; mutations require ConfigureApp + CSRF
// — the same gate as the runner fleet and scope mutations (agency is operator-
// governed, never self-declared; D-OWN / §2.1).
//
//	GET    /api/v1/agencies                     (session)
//	POST   /api/v1/agencies                     (ConfigureApp + CSRF)
//	PUT    /api/v1/agencies/{agencyId}          (ConfigureApp + CSRF)
//	DELETE /api/v1/agencies/{agencyId}          (ConfigureApp + CSRF) — 409 agency_in_use
//	PUT    /api/v1/scopes/{scopeId}/agency      (ConfigureApp + CSRF) — 422 unknown_agency
//	PUT    /api/v1/agencies/{agencyId}/members  (ConfigureApp + CSRF) — 422, RF-1b per added entity
func (s *Server) mountAgencies(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/agencies", s.auth.RequireSession(http.HandlerFunc(s.handleListAgencies)))
	mux.Handle("POST /api/v1/agencies", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleCreateAgency)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleUpdateAgency)))
	mux.Handle("DELETE /api/v1/agencies/{agencyId}", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleDeleteAgency)))
	mux.Handle("PUT /api/v1/scopes/{scopeId}/agency", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetScopeAgency)))
	// RB-22: the agency-scoped member setter — replace ONE AGENCY's member list.
	// The inverse of the per-kind matrices below; it touches only rows with this
	// agency_id, so edits to two different agencies cannot race by construction.
	mux.Handle("PUT /api/v1/agencies/{agencyId}/members", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetAgencyMembers)))
	// LR-80: the Vault paths an agency may name. Assigned by a global
	// administrator (the installation has one Vault connection, and which part
	// of it an agency may point at is the installation's decision); read by the
	// agency's own administrators, who need to know where they may write.
	mux.Handle("GET /api/v1/agencies/{agencyId}/vault-prefixes", s.auth.RequireSession(http.HandlerFunc(s.handleListVaultPrefixes)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}/vault-prefixes", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleSetVaultPrefixes)))
	// Runner ↔ agency membership matrix (M2). Reads session-gated so the Runners
	// view can render membership; writes ConfigureApp + CSRF (operator-assigned).
	mux.Handle("GET /api/v1/runner-agencies", s.auth.RequireSession(http.HandlerFunc(s.handleListRunnerAgencies)))
	mux.Handle("PUT /api/v1/runner-agencies", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetRunnerAgencies)))

	// Scope / secret / variable / SSH-key ↔ agency membership (migration 670,
	// the agencies plan T2.6). Same shape and same gate as the runner
	// matrix above: reads session-gated, writes ConfigureApp + CSRF.
	//
	// ConfigureApp rather than ManageEnvVars (a deliberate deviation from the plan's
	// T2.6 wording): membership is an ISOLATION-ZONE assignment, and every sibling
	// operation on that axis — the agency catalog, runner membership, scope binding —
	// is already ConfigureApp. Gating the secret/variable side on the weaker
	// ManageEnvVars would let a secrets manager re-home an isolation zone that an
	// admin owns, and would be an outright privilege inversion for SSH keys, whose
	// own CRUD is ConfigureApp (SK-D6). One axis, one gate.
	//
	// Nothing reads these tables for dispatch or resolution yet — see the note on
	// settings.SetAgencyMembership.
	for _, kind := range []settings.MemberKind{
		settings.MemberScope, settings.MemberSecret, settings.MemberEnvVar, settings.MemberSSHCredential,
	} {
		k := kind // capture per iteration — the handlers below outlive the loop
		mux.Handle("GET /api/v1/"+string(k)+"-agencies",
			s.auth.RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.handleListAgencyMembership(w, r, k)
			})))
		mux.Handle("PUT /api/v1/"+string(k)+"-agencies",
			s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.handleSetAgencyMembership(w, r, k)
			})))
	}

	// (GET /agency-preflight, the T2.12 report of what agency isolation would
	// break once switched on, was removed in v2.3.0: the switch has been on
	// since Phase 3, and nothing called it.)

	// RA-10 — the shadow warnings, for the Env Vars view.
	// Session-gated rather than ConfigureApp, and scope-filtered per caller, because
	// the audience is whoever is editing the catalogue rather than whoever is
	// planning a tightening: a warning only an admin can see is a warning the person
	// who created the row never gets.
	mux.Handle("GET /api/v1/env-var-shadows",
		s.auth.RequireSession(http.HandlerFunc(s.handleEnvVarShadows)))

	// Phase 4 (T4.1/T4.2) — the at-a-glance surface that closes G5. Reads are
	// session-gated like every other agency read; the writes behind the matrix's
	// cells are the existing ConfigureApp membership setters.
	mux.Handle("GET /api/v1/agency-matrix",
		s.auth.RequireSession(http.HandlerFunc(s.handleAgencyMatrix)))
	mux.Handle("GET /api/v1/agencies/{agencyId}",
		s.auth.RequireSession(http.HandlerFunc(s.handleAgencyDetail)))
}

// handleAgencyMatrix serves the whole membership grid in one read (T4.1): the
// agency columns plus every scope, secret, variable, SSH key and runner that could
// belong to one — including those that belong to none.
//
// One endpoint rather than the client joining nine catalog and membership reads,
// because the join it would have to do (entity id → name, per kind) is exactly the
// bookkeeping the matrix exists to spare an operator.
func (s *Server) handleAgencyMatrix(w http.ResponseWriter, r *http.Request) {
	m, err := settings.BuildAgencyMatrix(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, m)
}

// handleAgencyDetail serves one agency's contents plus the two numbers that decide
// whether anything will actually run in it (T4.2/T4.3).
func (s *Server) handleAgencyDetail(w http.ResponseWriter, r *http.Request) {
	d, err := settings.BuildAgencyDetail(r.Context(), s.db, r.PathValue("agencyId"))
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if d == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

func (s *Server) handleListAgencyMembership(w http.ResponseWriter, r *http.Request, kind settings.MemberKind) {
	list, err := settings.ListAgencyMembership(r.Context(), s.db, kind)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// membershipAuthzFor maps a membership kind to the permission and join table its
// departmental check uses — the same pairs the write routes pass to
// requireEntityAgency, kept in one place so the re-home guard and the write guard
// can never drift into disagreeing about who owns what (RF-1b).
//
// A scope is here since 2.3.0 (LR-7): it has one agency, and is moved under the
// same two-sided rule as the rest.
func membershipAuthzFor(kind settings.MemberKind) (perm, joinTable, joinCol, label string) {
	switch kind {
	case settings.MemberScope:
		return auth.PermConfigureApp, "scope_agencies", "scope_id", "scope"
	case settings.MemberSecret:
		return auth.PermManageEnvVars, "secret_agencies", "secret_id", "secret"
	case settings.MemberEnvVar:
		return auth.PermManageEnvVars, "env_var_agencies", "env_var_id", "variable"
	case settings.MemberSSHCredential:
		return auth.PermConfigureApp, "ssh_credential_agencies", "credential_id", "SSH key"
	default:
		// Runners (the M2 matrix) and anything added later: configureApp, matching
		// requireRunnerAgency. A new kind that forgets to register here still gets a
		// real check rather than silently getting none.
		return auth.PermConfigureApp, "runner_agencies", "runner_id", "runner"
	}
}

// requireMove authorizes giving an entity to an agency: the caller needs the
// kind's permission on the agency that has it today AND on the agency it is
// going to (RF-1b). It is one rule for a scope, a secret, a variable and a key.
//
// A scope was the exception until 2.3.0 (GC-6: a global administrator only),
// because a scope's agency decides which grants reach it and a scope could be
// in several agencies, so "the agency that has it" had no single answer. With
// one agency per scope (LR-7) it has, and the same two-sided rule that stops a
// department taking another's secret stops it taking another's scope — or
// pushing its own into an agency that did not ask for it. Global is a side like
// any other: moving a row into or out of Global takes a global administrator.
//
// An entity in NO agency may be placed by a global administrator (the repair
// path; see requireEntityAgencyOrRepair).
func (s *Server) requireMove(w http.ResponseWriter, r *http.Request, id auth.Identity,
	kind settings.MemberKind, entityID string, targets []string) bool {

	perm, joinTable, joinCol, label := membershipAuthzFor(kind)
	// The source side is EVERY agency the entity is in today, not any one of
	// them. For a row with one agency that is the same thing. For a row from
	// before 2.3.0 that two agencies share it is the difference between settling
	// it together and one of them taking it: a move replaces the whole list, so
	// an administrator of FIN alone naming FIN would remove it from TAX — and for
	// a secret or a key that TAX owns, make FIN its owner.
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT agency_id FROM `+joinTable+` WHERE `+joinCol+` = ? ORDER BY agency_id`, entityID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	var current []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			httpx.Fail500(w, s.log, "db_error", err)
			return false
		}
		current = append(current, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	rows.Close()
	if len(current) == 0 {
		// In no agency (damage: the repair path), or no such entity (the
		// handler's 404 or 422, for a global administrator).
		if !s.requireEntityAgencyOrRepair(w, r, id, perm, joinTable, joinCol, entityID, label) {
			return false
		}
	}
	for _, aid := range current {
		if id.CanAgency(perm, aid) {
			continue
		}
		if aid == agencyid.Global {
			s.denyEntityAgency(w, r, id, perm, auth.AllScopes,
				"this "+label+" is Global's, so only a global administrator may move it")
			return false
		}
		msg := "you do not have " + perm + " on the agency that has this " + label
		if len(current) > 1 {
			msg = "this " + label + " is shared by several agencies, and you do not have " + perm +
				" on all of them; moving it takes every one of them, or a global administrator"
		}
		s.denyEntityAgency(w, r, id, perm, aid, msg)
		return false
	}
	for _, aid := range targets {
		if !id.CanAgency(perm, aid) {
			s.denyEntityAgency(w, r, id, perm, aid,
				"you do not have "+perm+" on agency "+aid+", so you cannot move this "+label+" into it")
			return false
		}
	}
	return true
}

// handleSetAgencyMembership replaces the agency set of each posted entity
// (replace-per-row, like the runner-agencies and scope-restrictions matrices).
func (s *Server) handleSetAgencyMembership(w http.ResponseWriter, r *http.Request, kind settings.MemberKind) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body []settings.AgencyMembership
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// 🔴 RF-1b (the RBAC-fixes plan): re-homing IS an authorization act, and
	// without this check every departmental gate is a no-op.
	//
	// The bypass, in three requests: a Tax admin is refused
	// `PUT /ssh/credentials/k-fin` by requireEntityAgency; they move `k-fin` into
	// Tax through THIS endpoint (it asked only for the permission somewhere,
	// which they hold); they re-issue the write and it succeeds. Same shape for
	// secrets — re-home, then reveal — which is exactly the cross-department
	// credential read RB-32 exists to prevent. So the caller must hold the
	// permission on BOTH sides of the move (requireMove).
	for _, m := range body {
		if !s.requireMove(w, r, id, kind, m.ID, m.AgencyIDs) {
			return
		}
		// LR-80: a Vault-backed secret or key that moves to an agency must lie
		// inside THAT agency's Vault paths, or the move would be the way to get
		// a path into an agency that was never assigned it.
		for _, aid := range m.AgencyIDs {
			if !s.requireVaultRowFits(w, r, id, kind, m.ID, aid) {
				return
			}
		}
	}
	if err := settings.SetAgencyMembership(r.Context(), s.db, kind, body, id.Email); err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownAgency):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "a referenced agency does not exist")
		case errors.Is(err, settings.ErrUnknownMember):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_member", "a referenced "+string(kind)+" does not exist")
		case errors.Is(err, settings.ErrOwnerRemoval):
			// RA-15 — the message carries the entity id from the error because the
			// matrix posts many rows at once and "one of these is owned" is not enough
			// to act on.
			httpx.Fail(w, http.StatusUnprocessableEntity, "owner_removal", err.Error())
		case failAgencyRule(w, err):
		default:
			httpx.Fail500(w, s.log, "update_failed", err)
		}
		return
	}
	// Scope membership decides what an agency grant reaches. Since LR-78 that is
	// resolved per request from the grant snapshot, which the setter itself
	// invalidates (settings.SetAgencyMembership → auth.GrantsChanged), so the
	// move is in force on the next request and nobody is signed out. Entity
	// membership (secret, variable, SSH key, runner) is read per request by
	// requireEntityAgency, as it always was.
	list, err := settings.ListAgencyMembership(r.Context(), s.db, kind)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// handleEnvVarShadows serves the RA-10 shadow warnings for the Env Vars view: every
// scoped Secrets/Variables row that shadows a global row of the same key, and in
// particular the ones that do so with NO department membership — where the scoped
// row wins for every department's runs in that scope and nothing says so.
//
// Scope-filtered to the caller's grants, matching GET /env-vars and GET
// /env-secrets: a finding names a row and a scope, so an unfiltered list would tell
// a restricted operator which keys exist in scopes their own list hides (P1.7/M3).
// The global counterpart needs no filter of its own — a global row is readable from
// every scope, which is exactly why it can be shadowed at all.
func (s *Server) handleEnvVarShadows(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		// Fail CLOSED on a missing identity (L4): RequireSession should guarantee one,
		// and a read that names rows must not fail open if it somehow does not.
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	all, err := settings.ShadowFindings(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	out := make([]settings.ShadowFinding, 0, len(all))
	for _, f := range all {
		if auth.ScopeReadable(id, f.Scope) {
			out = append(out, f)
		}
	}
	// RA-18 — the multi-owner ambiguities ride the same response, so the Env Vars
	// view can warn about a name that will fail closed for a cross-department run
	// without a second round trip. Same scope filter; keys carry no scope, so they
	// are visible to any session exactly as key labels already are elsewhere.
	amb, err := settings.Ambiguities(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	ambOut := make([]settings.AmbiguityFinding, 0, len(amb))
	for _, f := range amb {
		if f.Kind == "key" || auth.ScopeReadable(id, f.Scope) {
			ambOut = append(ambOut, f)
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"shadows": out, "ambiguities": ambOut})
}

func (s *Server) handleListRunnerAgencies(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListRunnerAgencies(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// handleSetRunnerAgencies replaces the agency membership of each posted runner
// (replace-per-row, like the scope-restrictions matrix). Operator-assigned only.
func (s *Server) handleSetRunnerAgencies(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body []settings.RunnerAgencies
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// The serve list is the runner's, so writing it takes the runner's OWNER
	// (LR-59), and each agency named besides. What may be written at all is the
	// invariant's to say (settings.CheckRunnerPlacement, MA-11): an agent's list
	// is its owner and cannot be widened by anyone, so the only writes that get
	// through are a no-op and the narrowing of a legacy placement — and that
	// runner is Global's, a global administrator's.
	for _, m := range body {
		if !s.requireRunnerOwner(w, r, id, m.RunnerID) {
			return
		}
		for _, aid := range m.AgencyIDs {
			if !id.CanAgency(auth.PermConfigureApp, aid) {
				s.denyEntityAgency(w, r, id, auth.PermConfigureApp, aid,
					"you do not have configureApp on agency "+aid+", so you cannot move this runner into it")
				return
			}
		}
	}
	if err := settings.SetRunnerAgencies(r.Context(), s.db, body, id.Email); err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownAgency):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "a referenced agency does not exist")
		case errors.Is(err, settings.ErrUnknownRunner):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_runner", "a referenced runner does not exist")
		case failAgencyRule(w, err):
		default:
			httpx.Fail500(w, s.log, "update_failed", err)
		}
		return
	}
	list, err := settings.ListRunnerAgencies(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (s *Server) handleListAgencies(w http.ResponseWriter, r *http.Request) {
	list, err := settings.ListAgencies(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

type agencyInputBody struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
}

func (s *Server) handleCreateAgency(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var inp agencyInputBody
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(inp.Name) == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "agency name is required")
		return
	}
	a, err := settings.CreateAgency(r.Context(), s.db, settings.AgencyInput{Name: inp.Name, Description: inp.Description}, id.Email)
	if err != nil {
		if failAgencyRule(w, err) {
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "an agency with this name already exists")
			return
		}
		httpx.Fail500(w, s.log, "create_failed", err)
		return
	}
	httpx.JSON(w, http.StatusCreated, a)
}

func (s *Server) handleUpdateAgency(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	aid := r.PathValue("agencyId")
	var inp agencyInputBody
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(inp.Name) == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "agency name is required")
		return
	}
	a, err := settings.UpdateAgency(r.Context(), s.db, aid, settings.AgencyInput{Name: inp.Name, Description: inp.Description}, id.Email)
	if err != nil {
		if failAgencyRule(w, err) {
			return
		}
		if strings.Contains(err.Error(), "UNIQUE") {
			httpx.Fail(w, http.StatusConflict, "conflict", "an agency with this name already exists")
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if a == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	httpx.JSON(w, http.StatusOK, a)
}

func (s *Server) handleDeleteAgency(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	aid := r.PathValue("agencyId")
	// RB-17: an agency-shaped grant is ON DELETE CASCADE, so deleting an agency
	// would silently revoke every grant authored against it. Refuse instead, with
	// the same posture as the existing scope guard — quiet revocation is the exact
	// failure this whole surface exists to prevent.
	var grantRefs int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM access_grants WHERE agency_id = ?`, aid).Scan(&grantRefs); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if grantRefs > 0 {
		httpx.Fail(w, http.StatusConflict, "agency_in_use",
			"agency is referenced by "+strconv.Itoa(grantRefs)+" access grant(s); remove those grants first")
		return
	}
	// A service account bound to this agency is the same shape as a grant —
	// service_accounts.agency_id is ON DELETE CASCADE too — and it was not
	// counted: deleting the agency deleted its tokens' rows, and an integration
	// stopped authenticating with nothing said. Refuse while one is ACTIVE; a
	// revoked account is already dead and goes with the agency.
	var tokenRefs int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM service_accounts WHERE agency_id = ? AND revoked_at IS NULL`, aid).Scan(&tokenRefs); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if tokenRefs > 0 {
		httpx.Fail(w, http.StatusConflict, "agency_in_use",
			"agency is referenced by "+strconv.Itoa(tokenRefs)+" active service account(s); revoke those first")
		return
	}
	found, err := settings.DeleteAgency(r.Context(), s.db, aid, id.Email)
	if err != nil {
		if failAgencyRule(w, err) {
			return
		}
		if errors.Is(err, settings.ErrAgencyInUse) {
			// RA-Q22 (rev 7): name what actually blocks the delete. This message used
			// to say "referenced by one or more scopes" unconditionally, which was
			// WRONG whenever the blocker was a runner, a membership row, or ownership
			// — sending an operator to audit the one thing that was already clean.
			// Ownership is the case that matters most: it has no clearing path while
			// owner transfer is deferred, so the operator has to know the remedy is
			// destructive before they start.
			msg := "agency is still referenced; clear these first: "
			if inUse, ok := errors.AsType[*settings.AgencyInUseError](err); ok {
				msg += strings.Join(inUse.Blockers(), "; ")
			} else {
				msg += "one or more scopes, runners, memberships or owned entities"
			}
			httpx.Fail(w, http.StatusConflict, "agency_in_use", msg)
			return
		}
		httpx.Fail500(w, s.log, "delete_failed", err)
		return
	}
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetScopeAgency binds (or clears) a scope's agency — an operator overlay
// valid for both git- and cronomicon-source scopes. A nil/absent agencyId clears it.
func (s *Server) handleSetScopeAgency(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	var inp struct {
		AgencyID *string `json:"agencyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// LR-7: a scope's agency is changed by whoever administers it on BOTH sides —
	// where it is and where it is going (a null agencyId means Global, which is a
	// global administrator's on either side). An unknown scope id falls through
	// to the 404 below for a global administrator and is refused for anyone else,
	// which is requireEntityAgency's ordinary answer for a row that is not there.
	target := agencyid.Global
	if inp.AgencyID != nil && strings.TrimSpace(*inp.AgencyID) != "" {
		target = strings.TrimSpace(*inp.AgencyID)
	}
	if !s.requireMove(w, r, id, settings.MemberScope, sid, []string{target}) {
		return
	}
	sc, err := settings.SetScopeAgency(r.Context(), s.db, sid, inp.AgencyID, id.Email)
	if err != nil {
		if errors.Is(err, settings.ErrUnknownAgency) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "the referenced agency does not exist")
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	httpx.JSON(w, http.StatusOK, sc)
}

// handleSetAgencyMembers replaces one agency's member list across all five kinds
// (RB-22). This is the write half of the per-agency editor: the entity-centric
// matrices above answer "which agencies hold this secret?", this answers "what
// does Tax contain?" — and writing through the agency axis is what keeps two
// admins editing two different departments from racing on the same rows.
//
// Authorization mirrors handleSetAgencyMembership's RF-1b guard, restated for the
// inverted axis. ADDING an entity to this agency is a re-homing act: the caller
// must hold the kind's permission on an agency that owns the entity TODAY (you may
// only move what is yours; unowned entities are shared infrastructure and
// unrestricted-only, RB-Q14) and on THIS agency (you may only place things where
// you have authority). REMOVING needs the permission on this agency alone — being
// a member here is precisely what makes this one of the entity's owning agencies.
// A scope follows the same rule as the other kinds since 2.3.0 (LR-7).
//
// A RUNNER does not (Phase G3). Its serve list is its owner's to write, so both
// adding and removing one take configureApp on the runner's OWNER
// (requireRunnerOwner), not on this agency; and the writer's invariant then
// refuses every addition (MA-11), leaving one runner change this route makes:
// taking a legacy placement off this agency's list.
func (s *Server) handleSetAgencyMembers(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	var body struct {
		Members []settings.AgencyMemberRef `json:"members"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	for _, m := range body.Members {
		if !settings.ValidAgencyMemberKind(m.Kind) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_kind",
				"unknown member kind "+m.Kind)
			return
		}
	}

	// The delta is computed BEFORE the write so each ADDED entity can be
	// authorized individually and a denial names the entity. The read and the
	// write are not one transaction — the same accepted TOCTOU as every other
	// membership route, bounded by the ConfigureApp gate on the route itself.
	delta, err := settings.ComputeAgencyMembersDelta(r.Context(), s.db, agencyID, body.Members)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// A scope is authorized like every other kind since 2.3.0 (LR-7): an add is
	// a move INTO this agency, and needs authority on where the entity is today
	// and on this agency.
	for _, m := range delta.Added {
		if m.Kind == "runner" {
			// A runner is not moved between agencies (MA-11): the writer refuses
			// the addition whoever asks. The gate still runs first, so a caller
			// with no authority over the runner learns nothing from the refusal.
			if !s.requireRunnerOwner(w, r, id, m.ID) {
				return
			}
			continue
		}
		if !s.requireMove(w, r, id, settings.MemberKind(m.Kind), m.ID, []string{agencyID}) {
			return
		}
		if !s.requireVaultRowFits(w, r, id, settings.MemberKind(m.Kind), m.ID, agencyID) {
			return
		}
	}
	for _, m := range delta.Removed {
		if m.Kind == "runner" {
			// Taking a runner off an agency's serve list narrows a legacy
			// placement. It is a write on the runner, so it is the owner's
			// (LR-59), not the agency's it stops serving.
			if !s.requireRunnerOwner(w, r, id, m.ID) {
				return
			}
			continue
		}
		perm, _, _, label := membershipAuthzFor(settings.MemberKind(m.Kind))
		if !id.CanAgency(perm, agencyID) {
			s.denyEntityAgency(w, r, id, perm, agencyID,
				"you do not have "+perm+" on this agency, so you cannot remove this "+label+" from it")
			return
		}
	}

	if _, err := settings.SetAgencyMembers(r.Context(), s.db, agencyID, body.Members, id.Email); err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownAgency):
			httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		case errors.Is(err, settings.ErrUnknownMember):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_member", err.Error())
		case errors.Is(err, settings.ErrOwnerRemoval):
			httpx.Fail(w, http.StatusUnprocessableEntity, "owner_removal", err.Error())
		case failAgencyRule(w, err):
		default:
			httpx.Fail500(w, s.log, "update_failed", err)
		}
		return
	}

	// A scope moving in or out of this agency changes what grants reach; the
	// setter told the grant snapshot (LR-78). No session is signed out.

	d, err := settings.BuildAgencyDetail(r.Context(), s.db, agencyID)
	if err != nil || d == nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// failAgencyRule answers the refusals that Global being a real agency introduced
// (LR-21, LR-25, LR-26), for every route that writes membership or the catalog.
// It reports whether err was one of them (and the response is written).
func failAgencyRule(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, settings.ErrAgencyRequired):
		httpx.Fail(w, http.StatusUnprocessableEntity, "agency_required", err.Error())
	case errors.Is(err, settings.ErrGlobalMixed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "global_mixed", err.Error())
	case errors.Is(err, settings.ErrBuiltinAgency):
		httpx.Fail(w, http.StatusUnprocessableEntity, "builtin_agency", err.Error())
	case errors.Is(err, settings.ErrAgencyNameReserved):
		httpx.Fail(w, http.StatusUnprocessableEntity, "name_reserved", err.Error())
	case errors.Is(err, settings.ErrOneAgency):
		httpx.Fail(w, http.StatusUnprocessableEntity, "one_agency", err.Error())
	case errors.Is(err, settings.ErrOwnerConflict):
		httpx.Fail(w, http.StatusConflict, "owner_conflict", err.Error()+
			": rename or remove one of the two before moving this one")
	case errors.Is(err, settings.ErrServeListFixed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "serve_list_fixed", err.Error())
	case errors.Is(err, settings.ErrOwnerChangeRefused):
		httpx.Fail(w, http.StatusUnprocessableEntity, "owner_change_refused", err.Error())
	default:
		return false
	}
	return true
}

// requireVaultRowFits is the Vault-path half of a move (LR-80): when the entity
// is a Vault-backed secret or SSH key and it is going to a named agency, its
// path must lie inside that agency's prefixes. Any other kind, a row that is
// not Vault-backed, and a move into Global (a global administrator's, already
// established by requireMove) need nothing here.
func (s *Server) requireVaultRowFits(w http.ResponseWriter, r *http.Request, id auth.Identity,
	kind settings.MemberKind, entityID, targetAgency string) bool {

	if targetAgency == agencyid.Global {
		return true
	}
	var table, perm, label string
	switch kind {
	case settings.MemberSecret:
		table, perm, label = "secrets", auth.PermManageEnvVars, "secret"
	case settings.MemberSSHCredential:
		table, perm, label = "ssh_credentials", auth.PermConfigureApp, "SSH key"
	default:
		return true
	}
	var source string
	var ref sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(source, ''), vault_ref FROM `+table+` WHERE id = ?`, entityID).Scan(&source, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return true // the setter's own "unknown member"
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	// Each store's own question (see secretNamesVault and credentialNamesVault).
	vault := ref.String != ""
	if kind == settings.MemberSecret {
		vault = vault || source != "stored"
	} else {
		vault = vault || source == "vault"
	}
	if !vault {
		return true
	}
	return s.requireVaultPath(w, r, id, perm, targetAgency, ref.String, label)
}

type vaultPrefixesJSON struct {
	AgencyID string   `json:"agencyId"`
	Prefixes []string `json:"prefixes"`
}

func (s *Server) handleListVaultPrefixes(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	// Readable by whoever writes Vault-backed rows for the agency (manageEnvVars
	// for its secrets, configureApp for its keys) and by a global administrator.
	// Which part of Vault another agency may point at is not everyone's to read.
	if !id.GlobalAdmin(auth.PermConfigureApp) &&
		!id.CanAgency(auth.PermManageEnvVars, agencyID) && !id.CanAgency(auth.PermConfigureApp, agencyID) {
		s.denyEntityAgency(w, r, id, auth.PermManageEnvVars, agencyID,
			"you do not administer this agency, so its Vault paths are not yours to read")
		return
	}
	ag, err := settings.GetAgency(r.Context(), s.db, agencyID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if ag == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
	prefixes, err := settings.ListVaultPrefixes(r.Context(), s.db, agencyID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, vaultPrefixesJSON{AgencyID: agencyID, Prefixes: prefixes})
}

func (s *Server) handleSetVaultPrefixes(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	var body struct {
		Prefixes *[]string `json:"prefixes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// This route REPLACES the list, so a body without the field — `{}`, or a
	// misspelt key — must not read as "no prefixes" and clear it.
	if body.Prefixes == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"prefixes is required: send the whole list, or [] to remove every prefix")
		return
	}
	prefixes, err := settings.SetVaultPrefixes(r.Context(), s.db, agencyID, *body.Prefixes, id.Email)
	switch {
	case errors.Is(err, settings.ErrUnknownAgency):
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
	case errors.Is(err, settings.ErrGlobalHasNoVaultPrefixes):
		httpx.Fail(w, http.StatusUnprocessableEntity, "builtin_agency", err.Error())
	case errors.Is(err, vaultpath.ErrInvalid):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
	default:
		httpx.JSON(w, http.StatusOK, vaultPrefixesJSON{AgencyID: agencyID, Prefixes: prefixes})
	}
}
