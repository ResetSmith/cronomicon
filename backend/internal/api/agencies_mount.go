package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/vaultpath"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/notify"
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
	// 2.4.0: a scope cloned to an agency, by whoever may move it there.
	mux.Handle("POST /api/v1/scopes/{scopeId}/clone", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleCloneScope)))
	// RB-22: the agency-scoped member setter — replace ONE AGENCY's member list.
	// The inverse of the per-kind matrices below; it touches only rows with this
	// agency_id, so edits to two different agencies cannot race by construction.
	mux.Handle("PUT /api/v1/agencies/{agencyId}/members", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetAgencyMembers)))
	// LR-80: the Vault paths an agency may name. Assigned by a global
	// administrator (the installation has one Vault connection, and which part
	// of it an agency may point at is the installation's decision); read by the
	// agency's own administrators, who need to know where they may write.
	mux.Handle("GET /api/v1/agencies/{agencyId}/vault-prefixes", s.auth.RequireSession(http.HandlerFunc(s.handleListVaultPrefixes)))
	// 2.5.1 SG-26: an agency with a Vault of its own sets its own prefixes
	// (they limit only itself there); otherwise a global administrator does,
	// since they divide the installation's Vault between agencies.
	mux.Handle("PUT /api/v1/agencies/{agencyId}/vault-prefixes", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetVaultPrefixes)))
	// 2.5.2 OP-28: who the central team calls about the agency's runs when a
	// job names no one. The agency's administrators keep it current; a global
	// administrator may set any agency's.
	mux.Handle("PUT /api/v1/agencies/{agencyId}/contact", s.requirePerm("configureApp", permConfigureApp)(http.HandlerFunc(s.handleSetAgencyContact)))
	// GA-1 (2.5.0 C5): the directory groups an agency's grants may name. Set by
	// a global administrator; read by the agency's own administrators of access
	// (question 82), whose grant form offers them (GA-3).
	mux.Handle("GET /api/v1/agencies/{agencyId}/groups", s.auth.RequireSession(http.HandlerFunc(s.handleListAgencyGroups)))
	mux.Handle("PUT /api/v1/agencies/{agencyId}/groups", s.requireGlobal(auth.PermManageRoles)(http.HandlerFunc(s.handleSetAgencyGroups)))
	// 2.5.0 C10.1 (IN-12): a job given to the agency by one commit into its repository.
	mux.Handle("POST /api/v1/agencies/{agencyId}/assign", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleAssign)))
	// QT-1 (2.5.0 C6): an agency's own concurrency cap, a global administrator's,
	// as the agency catalog is. Read on the agency's row (maxConcurrent).
	mux.Handle("PUT /api/v1/agencies/{agencyId}/concurrency", s.requireGlobal(auth.PermConfigureApp)(http.HandlerFunc(s.handleSetAgencyConcurrency)))
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
	// The rows of what the caller reads (2.5.0 C2, CF-1: every agency's
	// scopes, keys and runners were listed by name to everyone). A row read
	// keeps its whole agency list (C0: filter rows, never trim a row's
	// agencies); the agency columns are names, which are not confidential.
	rows := make([]settings.MatrixRow, 0, len(m.Rows))
	for _, row := range m.Rows {
		if readsAnyAgency(r, row.AgencyIDs) {
			rows = append(rows, row)
		}
	}
	m.Rows = rows
	httpx.JSON(w, http.StatusOK, m)
}

// handleAgencyDetail serves one agency's contents plus the two numbers that decide
// whether anything will actually run in it (T4.2/T4.3).
func (s *Server) handleAgencyDetail(w http.ResponseWriter, r *http.Request) {
	// An agency's contents are its own people's and everywhere-readers'
	// (2.5.0 C2, CF-1); to anyone else, as if it were not there (question 72).
	if !readsAgency(r, r.PathValue("agencyId")) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
		return
	}
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
	// The entities the caller reads (2.5.0 C2, CF-1), each with its whole
	// agency list.
	kept := make([]settings.AgencyMembership, 0, len(list))
	for _, m := range list {
		if readsAnyAgency(r, m.AgencyIDs) {
			kept = append(kept, m)
		}
	}
	httpx.JSON(w, http.StatusOK, kept)
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
//
// A SCOPE is moved by a global administrator only (the owner's decision of
// 2026-10-13, on 2.4.0's question 21). A scope's agency decides whose
// repository's jobs may name it and which workflows are one agency's (GR-7),
// and a scope that comes from an agency's repository cannot be moved at all
// (GR-18): what was left, a scope built in the app or one of Global's
// repository passing between two agencies on the word of somebody who
// administers both, was the one way round the rule that a save which makes a
// workflow span agencies is a global administrator's. The two-sided rule is
// asked first all the same, so a caller with no authority over the scope is
// told what they were always told. A MOVE is a request that names an agency
// the scope is not in today. One that names only where it is moves nothing,
// and one that narrows a scope two agencies share from before 2.3.0 to one of
// them is those agencies' to settle, as it was: both are held to the
// two-sided rule alone. So is a CLONE of a scope (requireBothSides), which
// leaves the scope where it is.
//
// Since 2.4.0 Phase I4 (IN-7) EVERY kind is so, and a move is any change of
// the agency set — to another agency, or a narrowing of a set two agencies
// share from before 2.3.0: it is a global administrator's repair, with
// `repair: true` and a reason (requireRepair). A write that names the set as
// it is changes nothing and is held to the two-sided rule alone. It returns
// whether the write is a repair, for the caller's audit row.
func (s *Server) requireMove(w http.ResponseWriter, r *http.Request, id auth.Identity,
	kind settings.MemberKind, entityID string, targets []string, rep repairInput) (repaired, ok bool) {
	// Repeats of one id are one id (the setters dedupe too), so that what is
	// judged below is what will be written (the review of 2.5.3 CA1).
	targets = slices.Compact(slices.Sorted(slices.Values(targets)))

	// An entity that is not there is the setter's to answer (422, or the
	// handler's 404), before anything about repair (from the review of I4).
	exists, owner, err := settings.MemberOwner(r.Context(), s.db, kind, entityID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false, false
	}
	if !exists {
		if !id.GlobalAdmin(auth.PermConfigureApp) {
			_, ok := s.requireBothSides(w, r, id, kind, entityID, targets)
			return false, ok
		}
		return false, true
	}
	current, ok := s.requireBothSides(w, r, id, kind, entityID, targets)
	if !ok {
		return false, false
	}
	// Unchanged: the same agencies, and — for a row with an owner — the same
	// owner. The setter makes the one agency named the owner (moveOwner), so a
	// row Global owns that one agency uses (left by 1220) was claimed by
	// posting its membership as it stood, with no repair (from the review of
	// I4; possible before it under the two-sided rule).
	if sameAgencySet(current, targets) && (owner == "" || len(targets) != 1 || owner == targets[0]) {
		return false, true
	}
	// 2.5.3 (CA-4): nothing moves INTO Central; a leftover may move out.
	if len(targets) == 1 && targets[0] == agencyid.Global {
		_, _, _, label := membershipAuthzFor(kind)
		failCentralRunsNothing(w, label)
		return false, false
	}
	_, _, _, label := membershipAuthzFor(kind)
	if !s.requireRepair(w, r, id, rep, "this "+label) {
		return false, false
	}
	return true, true
}

// requireBothSides is requireMove's two-sided rule on its own: the kind's
// permission on every agency the entity is in today and on every agency
// named. It returns the agencies the entity is in.
func (s *Server) requireBothSides(w http.ResponseWriter, r *http.Request, id auth.Identity,
	kind settings.MemberKind, entityID string, targets []string) ([]string, bool) {

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
		return nil, false
	}
	var current []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			httpx.Fail500(w, s.log, "db_error", err)
			return nil, false
		}
		current = append(current, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	rows.Close()
	if len(current) == 0 {
		// In no agency (damage: the repair path), or no such entity (the
		// handler's 404 or 422, for a global administrator).
		if !s.requireEntityAgencyOrRepair(w, r, id, perm, joinTable, joinCol, entityID, label) {
			return nil, false
		}
	}
	for _, aid := range current {
		if id.CanAgency(perm, aid) {
			continue
		}
		if aid == agencyid.Global {
			s.denyEntityAgency(w, r, id, perm, auth.AllScopes,
				"this "+label+" is Global's, so only a global administrator may move it")
			return nil, false
		}
		msg := "you do not have " + perm + " on the agency that has this " + label
		if len(current) > 1 {
			msg = "this " + label + " is shared by several agencies, and you do not have " + perm +
				" on all of them; moving it takes every one of them, or a global administrator"
		}
		s.denyEntityAgency(w, r, id, perm, aid, msg)
		return nil, false
	}
	for _, aid := range targets {
		if !id.CanAgency(perm, aid) {
			s.denyEntityAgency(w, r, id, perm, aid,
				"you do not have "+perm+" on agency "+aid+", so you cannot move this "+label+" into it")
			return nil, false
		}
	}
	return current, true
}

// handleSetAgencyMembership replaces the agency set of each posted entity
// (replace-per-row, like the runner-agencies and scope-restrictions matrices).
func (s *Server) handleSetAgencyMembership(w http.ResponseWriter, r *http.Request, kind settings.MemberKind) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	// A bare array (the matrix as it is), or, for a repair (I4; question 44),
	// {repair, reason, assignments}.
	var body []settings.AgencyMembership
	var rep repairInput
	if !decodeRepairable(w, r, &body, &rep) {
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
	var repaired []settings.AgencyMembership
	audited := map[string]bool{} // one row per entity, however often it is posted
	for _, m := range body {
		isRepair, ok := s.requireMove(w, r, id, kind, m.ID, m.AgencyIDs, rep)
		if !ok {
			return
		}
		if isRepair && !audited[m.ID] {
			audited[m.ID] = true
			repaired = append(repaired, m)
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
	// 2.5.1 SG-17: a secret or key that changes hands is told to the alert
	// rules of the agency it leaves and of the one it reaches.
	movedTable, movedNoun := "", ""
	switch kind {
	case settings.MemberSecret:
		movedTable, movedNoun = "secrets", "Secret"
	case settings.MemberSSHCredential:
		movedTable, movedNoun = "ssh_credentials", "SSH key"
	}
	ownersBefore := map[string]string{}
	if movedTable != "" {
		for _, m := range body {
			if o, err := s.rowOwnerOrGlobal(r.Context(), movedTable, m.ID); err == nil {
				ownersBefore[m.ID] = o
			}
		}
	}
	if err := settings.SetAgencyMembership(r.Context(), s.db, kind, body, id.Email,
		settings.MoveOptions{ConfirmVaultChange: rep.ConfirmVaultChange}); err != nil {
		switch {
		case failVaultChanges(w, err):
		case errors.Is(err, settings.ErrUnknownAgency):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "a referenced agency does not exist")
		case errors.Is(err, settings.ErrUnknownMember):
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_member", "a referenced "+string(kind)+" does not exist")
		case errors.Is(err, settings.ErrScopeNameTaken):
			httpx.Fail(w, http.StatusConflict, "conflict", "scope name already exists in that agency")
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
	for _, m := range repaired {
		s.auditRepair(r.Context(), s.memberAgency(r.Context(), string(kind), m.ID), id.Email, string(kind)+" "+m.ID, agencySet(m.AgencyIDs), rep)
	}
	s.alertOwnerMoves(r, movedTable, movedNoun, ownersBefore)
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
		// Whose it is too (2.5.0 C2): the scope filter let an agency-wide
		// finding of any agency through.
		if auth.ScopeReadableIn(id, f.OwnerAgency, f.Scope) && readsAgency(r, f.OwnerAgency) {
			out = append(out, f)
		}
	}
	// An ambiguity names its owners (agencies, by name): it is shown to a
	// reader of the scope in any of them (I2: the scope is each owner's own).
	agencyIDs := map[string]string{}
	if rows, err := s.db.QueryContext(r.Context(), `SELECT id, name FROM agencies`); err == nil {
		for rows.Next() {
			var aid, name string
			if rows.Scan(&aid, &name) == nil {
				agencyIDs[name] = aid
			}
		}
		rows.Close()
	}
	// Shown to a reader of one of its owners (2.5.0 C2, CF-1): a key's
	// ambiguity, or one with no scope, had been shown to everyone.
	ambiguityReadable := func(f settings.AmbiguityFinding) bool {
		if id.Unrestricted() {
			return true
		}
		for _, owner := range f.Owners {
			if !readsAgency(r, agencyIDs[owner]) {
				continue
			}
			if f.Kind == "key" || f.Scope == "" || auth.ScopeReadableIn(id, agencyIDs[owner], f.Scope) {
				return true
			}
		}
		return false
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
		if ambiguityReadable(f) {
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
	// The runners the caller reads (2.5.0 C2, CF-1), each with its whole
	// agency list.
	kept := make([]settings.RunnerAgencies, 0, len(list))
	for _, m := range list {
		if readsAnyAgency(r, m.AgencyIDs) {
			kept = append(kept, m)
		}
	}
	httpx.JSON(w, http.StatusOK, kept)
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
	var rep repairInput
	if !decodeRepairable(w, r, &body, &rep) {
		return
	}
	// A runner's placement that changes (MA-11 lets through only the
	// narrowing of a legacy placement) is a repair (I4; IN-7). One that names
	// the list as it is changes nothing.
	current, err := settings.ListRunnerAgencies(r.Context(), s.db)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	serves := make(map[string][]string, len(current))
	for _, c := range current {
		serves[c.RunnerID] = c.AgencyIDs
	}
	var repaired []settings.RunnerAgencies
	seen := map[string]bool{}
	for _, m := range body {
		was, listed := serves[m.RunnerID]
		if !listed {
			// A runner that is there and serves nobody (damage) is placed by a
			// repair as well (from the review of I4); one that is not there is
			// the setter's 422.
			var n int
			if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM runners WHERE id = ?`, m.RunnerID).Scan(&n); err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if n == 0 {
				continue
			}
		}
		if !sameAgencySet(was, m.AgencyIDs) && !seen[m.RunnerID] {
			seen[m.RunnerID] = true
			repaired = append(repaired, m)
		}
	}
	if len(repaired) > 0 && !s.requireRepair(w, r, id, rep, "a runner's placement") {
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
	for _, m := range repaired {
		s.auditRepair(r.Context(), s.memberAgency(r.Context(), "runner", m.RunnerID), id.Email, "runner "+m.RunnerID+" placement", agencySet(m.AgencyIDs), rep)
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
	id, _ := auth.IdentityFrom(r.Context())
	// Names are every session's (CF-1: not confidential, and every view
	// resolves ids by them); an agency's contents are its people's (2.5.0 C2,
	// question 69): for one the caller does not read, its id, name and builtin
	// flag only.
	for i, a := range list {
		if !id.ReadsAgency(a.ID) {
			list[i] = settings.Agency{ID: a.ID, Name: a.Name, Builtin: a.Builtin}
		}
	}
	rows, err := withRowFlags(list, func(a settings.Agency) (rowFlags, error) { return agencyFlags(id, a.ID), nil })
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, rows)
}

type agencyInputBody struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// Repository is the agency's repository connection, required on create
	// (§0a, IN-2; Phase I3): an agency is created together with it. Not read
	// by an update; PUT /agencies/{agencyId}/git-repo changes a connection.
	Repository *settings.GitlabConfig `json:"repository,omitempty"`
}

// errSyncUnavailable is a delete's answer while the repositories' sync
// services are not running: its repository cannot be taken with it.
var errSyncUnavailable = errors.New("the repositories' sync services are not running; try again in a moment")

// agencyCreated is a create's answer: the agency, and the repository it was
// created with.
type agencyCreated struct {
	*settings.Agency
	Repository *settings.Repo `json:"repository"`
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
	// The name's own rule first: it needs no remote. Since 2.5.2 (GR) no name is
	// reserved; a name is unique, and that is all.
	var taken int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE name = ?`, strings.TrimSpace(inp.Name)).Scan(&taken); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if taken > 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "an agency with this name already exists")
		return
	}
	// An agency connects its repository at creation or later (SG-40, 2.5.1;
	// IN-2 as reworded): without one it cannot receive an assignment or
	// publish, and its administrator connects it in Settings → Repositories.
	// Given, the connection is tested — it answers, and its token may push —
	// before anything is written, and written with the agency (§0a, Phase I3).
	withRepo := inp.Repository != nil
	if withRepo && strings.TrimSpace(inp.Repository.RepoUrl) == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"give the repository's repoUrl and a token that can push, or leave `repository` out to create the agency without one")
		return
	}
	if withRepo && !s.requirePushableConnection(w, r, "repo-untested", false, *inp.Repository) {
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
	if !withRepo {
		httpx.JSON(w, http.StatusCreated, agencyCreated{Agency: a})
		return
	}
	repo, _, err := settings.PutAgencyRepo(r.Context(), s.db, s.cfg, a.ID, *inp.Repository, id.Email)
	if err != nil {
		// An agency without its repository is what this route exists not to
		// make: take the agency back (it holds nothing yet), with a repository
		// row the write may have left before it failed (from the review of
		// I3: a refusal for that row left both).
		takeBack := func(ctx context.Context, repoID string) (func(*sql.Tx) error, func(bool), error) {
			if s.git != nil {
				if rm, fin, err := s.git.RemoveWithAgency(ctx, repoID, id.Email); err == nil {
					return rm, fin, nil
				}
			}
			rm := func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `DELETE FROM git_repos WHERE id = ?`, repoID)
				return err
			}
			return rm, func(bool) {}, nil
		}
		if _, derr := settings.DeleteAgencyWith(r.Context(), s.db, a.ID, id.Email, takeBack); derr != nil {
			s.log.Error("an agency whose repository could not be written could not be taken back",
				"agency_id", a.ID, "error", derr)
		}
		switch {
		case errors.Is(err, settings.ErrRepoURLNotAllowed):
			httpx.Fail(w, http.StatusUnprocessableEntity, "repo_url_not_allowed", err.Error())
		case errors.Is(err, settings.ErrValidation):
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		default:
			httpx.Fail500(w, s.log, "create_failed", err)
		}
		return
	}
	if s.git != nil {
		if err := s.git.Restart(r.Context(), repo.ID); err != nil {
			s.log.Error("the agency and its repository were created, but its sync service could not be started",
				"repo_id", repo.ID, "actor", id.Email, "error", err)
		}
	}
	httpx.JSON(w, http.StatusCreated, agencyCreated{Agency: a, Repository: repo})
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
	// would silently revoke every grant authored against it; a service account
	// bound to it is the same shape. DeleteAgencyWith refuses both, and an
	// unspent registration token, inside the delete's transaction (2.4.2,
	// present defect 62: until then they were asked here only, and the create
	// route's rollback did not ask them).
	//
	// An empty agency is deleted with its repository (§0a, IN-13; Phase I3;
	// question 37): the connection, and what the repository supplied, which
	// is all still in Git, go in the same transaction as the agency.
	takeRepo := func(ctx context.Context, repoID string) (func(*sql.Tx) error, func(bool), error) {
		if s.git == nil {
			return nil, nil, errSyncUnavailable
		}
		return s.git.RemoveWithAgency(ctx, repoID, id.Email)
	}
	found, err := settings.DeleteAgencyWith(r.Context(), s.db, aid, id.Email, takeRepo)
	if err != nil {
		if failAgencyRule(w, err) {
			return
		}
		var busy *gitlab.ErrScopesBusy
		if errors.As(err, &busy) {
			httpx.Fail(w, http.StatusConflict, "scopes_busy", busy.Error())
			return
		}
		if errors.Is(err, errSyncUnavailable) {
			httpx.Fail(w, http.StatusServiceUnavailable, "unavailable", err.Error())
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
		repairInput
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
	// An agency that is not there is that, before it is a repair.
	if target != agencyid.Global && !s.requireKnownAgency(w, r, target) {
		return
	}
	isRepair, ok := s.requireMove(w, r, id, settings.MemberScope, sid, []string{target}, inp.repairInput)
	if !ok {
		return
	}
	sc, err := settings.SetScopeAgency(r.Context(), s.db, sid, inp.AgencyID, id.Email)
	if err != nil {
		if errors.Is(err, settings.ErrUnknownAgency) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "the referenced agency does not exist")
			return
		}
		if errors.Is(err, settings.ErrScopeNameTaken) {
			httpx.Fail(w, http.StatusConflict, "conflict", "scope name already exists in that agency")
			return
		}
		if failAgencyRule(w, err) {
			return
		}
		httpx.Fail500(w, s.log, "update_failed", err)
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	if isRepair {
		s.auditRepair(r.Context(), s.memberAgency(r.Context(), "scope", sid), id.Email, "scope "+sid, agencySet([]string{target}), inp.repairInput)
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
		repairInput
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
	// Any change of what this agency holds changes some object's agency: an
	// addition moves it here (one agency each since 2.3.0), a removal takes
	// it away. A repair (I4; IN-7), whatever the kind.
	repairing := len(delta.Added)+len(delta.Removed) > 0
	if repairing {
		// What is not there is said first, before anything about repair (from
		// the review of I4): an agency that is not there, a member that is not.
		var known int
		if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, agencyID).Scan(&known); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if known == 0 {
			httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
			return
		}
		for _, m := range delta.Added {
			if m.Kind == "runner" {
				continue // a runner addition is refused by its writer, whoever asks (MA-11)
			}
			exists, _, err := settings.MemberOwner(r.Context(), s.db, settings.MemberKind(m.Kind), m.ID)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if !exists {
				httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_member", "a referenced "+m.Kind+" does not exist: "+m.ID)
				return
			}
		}
	}
	if repairing && !s.requireRepair(w, r, id, body.repairInput, "what agency "+agencyID+" holds") {
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
		if _, ok := s.requireMove(w, r, id, settings.MemberKind(m.Kind), m.ID, []string{agencyID}, body.repairInput); !ok {
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

	// 2.5.1 SG-17 (review A10): a secret or key this changes hands is told to
	// both agencies' alert rules, as the per-kind route does.
	secretsBefore, keysBefore := map[string]string{}, map[string]string{}
	for _, list := range [][]settings.AgencyMemberRef{delta.Added, delta.Removed} {
		for _, m := range list {
			var table string
			var into map[string]string
			switch settings.MemberKind(m.Kind) {
			case settings.MemberSecret:
				table, into = "secrets", secretsBefore
			case settings.MemberSSHCredential:
				table, into = "ssh_credentials", keysBefore
			default:
				continue
			}
			if o, err := s.rowOwnerOrGlobal(r.Context(), table, m.ID); err == nil {
				into[m.ID] = o
			}
		}
	}
	if _, err := settings.SetAgencyMembers(r.Context(), s.db, agencyID, body.Members, id.Email,
		settings.MoveOptions{ConfirmVaultChange: body.ConfirmVaultChange}); err != nil {
		switch {
		case failVaultChanges(w, err):
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

	s.alertOwnerMoves(r, "secrets", "Secret", secretsBefore)
	s.alertOwnerMoves(r, "ssh_credentials", "SSH key", keysBefore)

	// A scope moving in or out of this agency changes what grants reach; the
	// setter told the grant snapshot (LR-78). No session is signed out.
	if repairing {
		for _, m := range delta.Added {
			s.auditRepair(r.Context(), s.memberAgency(r.Context(), m.Kind, m.ID), id.Email, m.Kind+" "+m.ID, "added to agency "+agencyID, body.repairInput)
		}
		for _, m := range delta.Removed {
			s.auditRepair(r.Context(), s.memberAgency(r.Context(), m.Kind, m.ID), id.Email, m.Kind+" "+m.ID, "removed from agency "+agencyID, body.repairInput)
		}
	}

	d, err := settings.BuildAgencyDetail(r.Context(), s.db, agencyID)
	if err != nil || d == nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, d)
}

// alertOwnerMoves tells the alert rules of the agency a secret or key left and
// of the one it reached (2.5.1 SG-17), for each row of table whose owner is no
// longer the one ownersBefore recorded. Both routes that move an owner call it
// (the 2.5.1 review's A10: the per-agency members route did not).
func (s *Server) alertOwnerMoves(r *http.Request, table, noun string, ownersBefore map[string]string) {
	for rowID, before := range ownersBefore {
		after, err := s.rowOwnerOrGlobal(r.Context(), table, rowID)
		if err != nil || after == before {
			continue
		}
		nameCol := "key"
		if table == "ssh_credentials" {
			nameCol = "label"
		}
		var name string
		_ = s.db.QueryRowContext(r.Context(), `SELECT `+nameCol+` FROM `+table+` WHERE id = ?`, rowID).Scan(&name) //nolint:gosec // fixed identifiers
		s.raiseSecretAlert(r, notify.TriggerSecretChanged, noun, name, before, "moved to "+s.agencyNameOr(r, after))
		s.raiseSecretAlert(r, notify.TriggerSecretChanged, noun, name, after, "moved in from "+s.agencyNameOr(r, before))
	}
}

// failVaultChanges answers a move refused for taking Vault-backed rows to
// another Vault (2.5.1 SG6.2): 409 vault_changes with the rows, the shape of
// the agency Vault's references_unresolved. It reports whether err was one.
func failVaultChanges(w http.ResponseWriter, err error) bool {
	var vc *settings.VaultChangesError
	if !errors.As(err, &vc) {
		return false
	}
	httpx.JSON(w, http.StatusConflict, map[string]any{
		"code": "vault_changes",
		"message": "moving it changes the Vault its path is read from: the same path in the other Vault may hold " +
			"another value, or none. Confirm to move it anyway",
		"rows": vc.Rows,
	})
	return true
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
	case errors.Is(err, settings.ErrOneAgency):
		httpx.Fail(w, http.StatusUnprocessableEntity, "one_agency", err.Error())
	case errors.Is(err, settings.ErrScopeAgencyFixed):
		httpx.Fail(w, http.StatusConflict, "scope_agency_fixed", err.Error())
	case errors.Is(err, settings.ErrOwnerConflict):
		httpx.Fail(w, http.StatusConflict, "owner_conflict", err.Error()+
			": rename or remove one of the two before moving this one")
	case errors.Is(err, settings.ErrServeListFixed):
		httpx.Fail(w, http.StatusUnprocessableEntity, "serve_list_fixed", err.Error())
	case errors.Is(err, settings.ErrOwnerChangeRefused):
		httpx.Fail(w, http.StatusUnprocessableEntity, "owner_change_refused", err.Error())
	case errors.Is(err, settings.ErrLocalRunnerGlobalOnly):
		httpx.Fail(w, http.StatusUnprocessableEntity, "local_runner_global_only", err.Error())
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
	// OwnVault: the agency has a Vault of its own (2.5.1 SG-26), where no
	// prefix means any path.
	OwnVault bool `json:"ownVault"`
}

// vaultPrefixesAnswer is the list with whether the agency has its own Vault.
func (s *Server) vaultPrefixesAnswer(w http.ResponseWriter, r *http.Request, agencyID string, prefixes []string) {
	own, err := settings.AgencyHasOwnVault(r.Context(), s.db, agencyID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, vaultPrefixesJSON{AgencyID: agencyID, Prefixes: prefixes, OwnVault: own})
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
	s.vaultPrefixesAnswer(w, r, agencyID, prefixes)
}

func (s *Server) handleSetVaultPrefixes(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	if !id.GlobalAdmin(auth.PermConfigureApp) {
		own, err := settings.AgencyHasOwnVault(r.Context(), s.db, agencyID)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if !own || !id.CanAgency(auth.PermConfigureApp, agencyID) {
			s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
				"an agency's Vault path prefixes divide the installation's Vault, so a global administrator sets them; "+
					"an agency with a Vault of its own sets its own")
			return
		}
	}
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
		s.vaultPrefixesAnswer(w, r, agencyID, prefixes)
	}
}

type agencyGroupsJSON struct {
	AgencyID string   `json:"agencyId"`
	Groups   []string `json:"groups"`
}

func (s *Server) handleListAgencyGroups(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	// Read by whoever writes the agency's grants (manageRoles there) and by a
	// global administrator (question 82): the grant form offers the list.
	if !id.GlobalAdmin(auth.PermManageRoles) && !id.CanAgency(auth.PermManageRoles, agencyID) {
		s.denyEntityAgency(w, r, id, auth.PermManageRoles, agencyID,
			"you do not administer access for this agency, so its group list is not yours to read")
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
	groups, err := settings.ListAgencyGroups(r.Context(), s.db, agencyID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, agencyGroupsJSON{AgencyID: agencyID, Groups: groups})
}

func (s *Server) handleSetAgencyGroups(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	var body struct {
		Groups *[]string `json:"groups"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// This route REPLACES the list, so a body without the field must not read
	// as "no groups" and clear it (as for the Vault prefixes).
	if body.Groups == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"groups is required: send the whole list, or [] to remove every group")
		return
	}
	groups, err := settings.SetAgencyGroups(r.Context(), s.db, agencyID, *body.Groups, id.Email)
	switch {
	case errors.Is(err, settings.ErrUnknownAgency):
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
	case errors.Is(err, settings.ErrGlobalHasNoGroups):
		httpx.Fail(w, http.StatusUnprocessableEntity, "builtin_agency", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
	default:
		httpx.JSON(w, http.StatusOK, agencyGroupsJSON{AgencyID: agencyID, Groups: groups})
	}
}

func (s *Server) handleSetAgencyConcurrency(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// null removes the cap, so a body without the field must not read as null.
	raw, present := body["maxConcurrent"]
	if !present {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"maxConcurrent is required: a number of runs, or null for no cap of the agency's own")
		return
	}
	var maxConcurrent *int
	if err := json.Unmarshal(raw, &maxConcurrent); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "maxConcurrent must be a whole number of runs, or null")
		return
	}
	a, err := settings.SetAgencyConcurrency(r.Context(), s.db, r.PathValue("agencyId"), maxConcurrent, id.Email)
	switch {
	case errors.Is(err, settings.ErrUnknownAgency):
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
	default:
		httpx.JSON(w, http.StatusOK, a)
	}
}

// handleSetAgencyContact sets the agency's contact (2.5.2 OP-28): who the
// central team calls about its runs when the job or workflow names no contact
// of its own. Its administrators keep it, as they keep its alert rules; a
// global administrator may set any agency's, Global's included (Global is the
// global administrators' agency).
func (s *Server) handleSetAgencyContact(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	agencyID := r.PathValue("agencyId")
	if !id.GlobalAdmin(auth.PermConfigureApp) && (agencyID == agencyid.Global || !id.CanAgency(auth.PermConfigureApp, agencyID)) {
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, agencyID,
			"an agency's contact is set by its administrators or a global administrator")
		return
	}
	var body struct {
		Contact *string `json:"contact"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	// "" clears it, so a body without the field must not read as "".
	if body.Contact == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", `contact is required: the text, or "" to clear it`)
		return
	}
	a, err := settings.SetAgencyContact(r.Context(), s.db, agencyID, *body.Contact, id.Email)
	switch {
	case errors.Is(err, settings.ErrUnknownAgency):
		httpx.Fail(w, http.StatusNotFound, "not_found", "agency not found")
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case err != nil:
		httpx.Fail500(w, s.log, "update_failed", err)
	default:
		httpx.JSON(w, http.StatusOK, a)
	}
}

// handleCloneScope clones a scope to an agency (the owner's decision of
// 2026-10-13; settings.CloneScope says what is and is not copied).
//
// Who may: an administrator of the agency the scope is in and of the one the
// clone is for, or a global administrator (requireBothSides). A clone shows
// one agency's host list to another, so it takes both. It is NOT held to the
// rule for moving a scope (a global administrator only, requireMove): the
// scope stays where it is, and the clone is a new scope of the other agency.
func (s *Server) handleCloneScope(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	sid := r.PathValue("scopeId")
	var inp struct {
		AgencyID string `json:"agencyId"`
		Name     string `json:"name"`
	}
	// The gate first, on what the path and the body name, before the body is
	// held to anything else: a caller who may not is told nothing more.
	// A body that is not JSON says so (2.4.2: it was read as empty, and a
	// clone into Global with no name was refused for its name). That tells a
	// caller nothing about any agency.
	if err := json.NewDecoder(r.Body).Decode(&inp); err != nil && !errors.Is(err, io.EOF) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", err.Error())
		return
	}
	target := strings.TrimSpace(inp.AgencyID)
	if target == "" {
		target = agencyid.Global
	}
	// 2.5.3 (CA-4): a clone is a new scope, and no new scope is Central's.
	if target == agencyid.Global {
		failCentralRunsNothing(w, "scope")
		return
	}
	if _, ok := s.requireBothSides(w, r, id, settings.MemberScope, sid, []string{target}); !ok {
		return
	}
	// The copy is created in the target agency (I2: a scope's name is unique there).
	sc, copied, err := settings.CloneScope(r.Context(), s.db, sid, inp.Name, target, id.Email)
	switch {
	case errors.Is(err, settings.ErrNoSuchScope):
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	case errors.Is(err, settings.ErrUnknownAgency):
		httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "the referenced agency does not exist")
		return
	case errors.Is(err, settings.ErrScopeNameTaken):
		httpx.Fail(w, http.StatusConflict, "conflict", "scope name already exists")
		return
	case errors.Is(err, settings.ErrValidation):
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	case err != nil:
		httpx.Fail500(w, s.log, "clone_failed", err)
		return
	}
	note := "The clone is a scope of its own and does not follow the original. Its runners are not bound and no host key is approved for it."
	if copied.HostRecords > 0 {
		note += " Its host records were copied without their SSH keys and bastions: it connects to nothing until its agency sets its own."
	}
	httpx.JSON(w, http.StatusCreated, map[string]any{"scope": sc, "copied": copied, "note": note})
}
