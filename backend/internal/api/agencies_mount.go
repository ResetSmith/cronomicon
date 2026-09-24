package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

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
	mux.Handle("POST /api/v1/agencies", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCreateAgency)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleUpdateAgency)))
	mux.Handle("DELETE /api/v1/agencies/{agencyId}", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleDeleteAgency)))
	mux.Handle("PUT /api/v1/scopes/{scopeId}/agency", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetScopeAgency)))
	// RB-22: the agency-scoped member setter — replace ONE AGENCY's member list.
	// The inverse of the per-kind matrices below; it touches only rows with this
	// agency_id, so edits to two different agencies cannot race by construction.
	mux.Handle("PUT /api/v1/agencies/{agencyId}/members", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetAgencyMembers)))
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

	// T2.12 pre-flight report — read-only, ConfigureApp. What WOULD change if the
	// Phase-3 predicates were switched on today. Ship and review before Phase 3.
	mux.Handle("GET /api/v1/agency-preflight",
		s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleAgencyPreflight)))

	// RA-10 — the shadow warnings, split out of the preflight for the Env Vars view.
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
// MemberScope is not represented: scope membership defines the grant expansion
// rather than being governed by it, and its caller skips this path entirely.
func membershipAuthzFor(kind settings.MemberKind) (perm, joinTable, joinCol, label string) {
	switch kind {
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
	// 🔴 RF-1b (the RBAC-fixes plan): re-homing IS an authorization act,
	// and without this check every departmental gate added in v0.56.7 is a no-op.
	//
	// The bypass, in three requests: a Tax admin is refused
	// `PUT /ssh/credentials/k-fin` by requireEntityAgency; they move `k-fin` into
	// Tax through THIS endpoint (it asked only for configureApp, which they hold);
	// they re-issue the write and it succeeds. Same shape for secrets — re-home,
	// then reveal — which is exactly the cross-department credential read RB-32
	// exists to prevent.
	//
	// So the caller must hold the permission on BOTH sides of the move: on an
	// agency that owns the entity today (you may only move what is yours) and on
	// every agency they are moving it INTO (you may only place it where you have
	// authority). An entity with no membership is shared infrastructure, so
	// claiming one is unrestricted-only — the same RB-Q14 rule that governs
	// writing it, for the same reason: absence of membership carries no authority.
	//
	// Scope membership is exempt: a scope's agencies define the grant expansion
	// itself, so it is administered by the global configureApp gate above and not
	// by the departmental axis it produces.
	if kind != settings.MemberScope {
		perm, joinTable, joinCol, label := membershipAuthzFor(kind)
		for _, m := range body {
			if !s.requireEntityAgency(w, r, id, perm, joinTable, joinCol, m.ID, label) {
				return
			}
			for _, aid := range m.AgencyIDs {
				if !id.CanAgency(perm, aid) {
					s.denyEntityAgency(w, r, id, perm, aid,
						"you do not have "+perm+" on agency "+aid+", so you cannot move this "+label+" into it")
					return
				}
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
		default:
			httpx.Fail500(w, s.log, "update_failed", err)
		}
		return
	}
	// RB-Q10: agency membership is an authorization input now. SCOPE membership
	// feeds LOGIN-TIME grant expansion (agency → scopes), so an edit does not
	// reach live sessions until the epoch bump signs them out — the same
	// machinery every other RBAC write uses. ENTITY membership
	// (secret/variable/SSH-key/runner) deliberately does NOT bump: those sets are
	// read per-request by requireEntityAgency (RB-16/RB-32), so an edit takes
	// effect on the next request with no session churn. Two different lifetimes,
	// both correct — this comment is what keeps the asymmetry from reading as an
	// omission (RF-8).
	if kind == settings.MemberScope {
		s.auth.RevokeOtherSessions(w, r)
	}
	list, err := settings.ListAgencyMembership(r.Context(), s.db, kind)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

// handleAgencyPreflight serves the T2.12 report: what would BREAK or CHANGE if the
// Phase-3 predicates were switched on right now, computed against live data.
//
// This exists because Phase 2 and Phase 3 are deliberately not merged. Phase 2 is
// reversible and observable — the tables are populated but nothing dispatches or
// resolves against them — so this report can be read against REAL production data
// before any predicate changes. Merging the phases would mean discovering AG-Q5's
// tightening in production instead of here.
func (s *Server) handleAgencyPreflight(w http.ResponseWriter, r *http.Request) {
	rep, err := settings.AgencyPreflight(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rep)
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
	// RF-1b, the runner half — see handleSetAgencyMembership for the reasoning.
	// Without it, RF-2's gate is bypassable by moving another department's runner
	// (or a general-pool one) into your own agency and then draining it.
	for _, m := range body {
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
			"runner_agencies", "runner_id", m.RunnerID, "runner") {
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
	found, err := settings.DeleteAgency(r.Context(), s.db, aid, id.Email)
	if err != nil {
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
// valid for both git- and amadeus-source scopes. A nil/absent agencyId clears it.
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
// Scopes are exempt from the departmental axis (their membership DEFINES the grant
// expansion; the global ConfigureApp gate on the route governs them), and runners
// follow the same configureApp rule as the M2 matrix.
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
	for _, m := range delta.Added {
		if m.Kind == "scope" {
			continue
		}
		perm, joinTable, joinCol, label := membershipAuthzFor(settings.MemberKind(m.Kind))
		if !s.requireEntityAgency(w, r, id, perm, joinTable, joinCol, m.ID, label) {
			return
		}
		if !id.CanAgency(perm, agencyID) {
			s.denyEntityAgency(w, r, id, perm, agencyID,
				"you do not have "+perm+" on this agency, so you cannot move this "+label+" into it")
			return
		}
	}
	for _, m := range delta.Removed {
		if m.Kind == "scope" {
			continue
		}
		perm, _, _, label := membershipAuthzFor(settings.MemberKind(m.Kind))
		if !id.CanAgency(perm, agencyID) {
			s.denyEntityAgency(w, r, id, perm, agencyID,
				"you do not have "+perm+" on this agency, so you cannot remove this "+label+" from it")
			return
		}
	}

	result, err := settings.SetAgencyMembers(r.Context(), s.db, agencyID, body.Members, id.Email)
	if err != nil {
		switch {
		case errors.Is(err, settings.ErrUnknownAgency):
			httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		case errors.Is(err, settings.ErrUnknownMember):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_member", err.Error())
		case errors.Is(err, settings.ErrOwnerRemoval):
			httpx.Fail(w, http.StatusUnprocessableEntity, "owner_removal", err.Error())
		default:
			httpx.Fail500(w, s.log, "update_failed", err)
		}
		return
	}

	// RB-Q10, same asymmetry as handleSetAgencyMembership and for the same reason:
	// SCOPE membership feeds login-time grant expansion, so a scope moving in or
	// out of this agency changes what grants reach and must bounce live sessions.
	// Entity membership is read per-request and needs no churn.
	scopeChanged := false
	for _, m := range append(append([]settings.AgencyMemberRef{}, result.Added...), result.Removed...) {
		if m.Kind == "scope" {
			scopeChanged = true
		}
	}
	if scopeChanged {
		s.auth.RevokeOtherSessions(w, r)
	}

	d, err := settings.BuildAgencyDetail(r.Context(), s.db, agencyID)
	if err != nil || d == nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}
