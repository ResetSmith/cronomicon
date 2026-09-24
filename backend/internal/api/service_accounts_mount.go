package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountServiceAccounts owns the machine-principal surface
// (the prod-features plan §1, ET-A/ET-B).
//
// Two halves that must be read together:
//
//   - The CRUD half is session-authenticated and gated on manageRoles. Minting
//     a service account IS granting a role — the row carries one, and the token
//     authorizes as it — so it sits behind the same permission that edits role
//     grants, not behind configureApp. Runner registration tokens set the
//     precedent for the shown-once mechanics (540/token.go); this reuses them.
//
//   - The trigger half is TOKEN-authenticated and is the only route family in
//     the tree that authorizes without a session. It deliberately does NOT use
//     requirePerm (which chains RequireSession → RequireCSRF and would reject
//     every token) — it delegates to the ordinary run handlers, which do their
//     own in-handler requireCan against the resolved Identity. That delegation
//     is the whole design: there is ONE enforcement site for a run, and a token
//     trigger passes through every gate a click does.
//
// # Addressing (PF-Q16)
//
// Trigger routes key on NAME, not rowid. Every other job route takes a rowid,
// which is correct for a UI that just listed the rows and wrong for a
// monitoring system holding a config file. Names are unique only per
// (source, name) under the dual-source model, so `?source=` disambiguates and
// an ambiguous bare name is a 409 that says which sources matched — never a
// silent pick.
//
//	GET    /api/v1/service-accounts            list (hashes only, never plaintext)
//	POST   /api/v1/service-accounts            mint — returns the token ONCE
//	DELETE /api/v1/service-accounts/{id}       revoke
//	POST   /api/v1/trigger/jobs/{name}         run a job as the token's principal
//	POST   /api/v1/trigger/workflows/{name}    trigger a workflow likewise
func (s *Server) mountServiceAccounts(mux *http.ServeMux, eng *workflow.Engine) {
	manageRoles := s.requirePerm("manageRoles", permManageRoles)
	mux.Handle("GET /api/v1/service-accounts", manageRoles(http.HandlerFunc(s.listServiceAccounts)))
	mux.Handle("POST /api/v1/service-accounts", manageRoles(http.HandlerFunc(s.createServiceAccount)))
	mux.Handle("DELETE /api/v1/service-accounts/{id}", manageRoles(http.HandlerFunc(s.revokeServiceAccount)))

	token := s.auth.RequireServiceToken
	mux.Handle("POST /api/v1/trigger/jobs/{name}", token(http.HandlerFunc(s.triggerJobByName)))
	mux.Handle("POST /api/v1/trigger/workflows/{name}", token(http.HandlerFunc(s.triggerWorkflowByName(eng))))
}

// ─────────────────────────────────────────────────────────────────────────────
// Service-account CRUD
// ─────────────────────────────────────────────────────────────────────────────

// serviceAccountRow is the read model. It carries NO token material — the
// plaintext exists only in the mint response, and the hash never leaves the DB.
type serviceAccountRow struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
	Role        string  `json:"role"`
	AgencyID    *string `json:"agencyId,omitempty"`
	AgencyName  *string `json:"agencyName,omitempty"`
	AllScopes   bool    `json:"allScopes"`
	CreatedBy   string  `json:"createdBy"`
	CreatedAt   string  `json:"createdAt"`
	ExpiresAt   *string `json:"expiresAt,omitempty"`
	RevokedAt   *string `json:"revokedAt,omitempty"`
	LastUsedAt  *string `json:"lastUsedAt,omitempty"`
	// Status is derived, not stored: active | expired | revoked. Mirrors
	// registration tokens' tokenStatus so the two lists read the same way.
	Status string `json:"status"`
}

func serviceAccountStatus(revokedAt, expiresAt sql.NullString, now time.Time) string {
	if revokedAt.Valid {
		return "revoked"
	}
	if expiresAt.Valid {
		if t, err := time.Parse(time.RFC3339, expiresAt.String); err == nil && !t.After(now) {
			return "expired"
		}
	}
	return "active"
}

func (s *Server) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT sa.id, sa.name, sa.description, sa.role, sa.agency_id, a.name,
		       sa.all_scopes, sa.created_by, sa.created_at, sa.expires_at, sa.revoked_at, sa.last_used_at
		  FROM service_accounts sa
		  LEFT JOIN agencies a ON a.id = sa.agency_id
		 ORDER BY sa.created_at DESC, sa.rowid DESC
		 LIMIT 200`)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	now := time.Now().UTC()
	out := []serviceAccountRow{}
	for rows.Next() {
		var (
			it                                           serviceAccountRow
			desc, agencyID, agencyName, expires, revoked sql.NullString
			lastUsed                                     sql.NullString
			allScopes                                    int
		)
		if err := rows.Scan(&it.ID, &it.Name, &desc, &it.Role, &agencyID, &agencyName,
			&allScopes, &it.CreatedBy, &it.CreatedAt, &expires, &revoked, &lastUsed); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		it.AllScopes = allScopes == 1
		it.Status = serviceAccountStatus(revoked, expires, now)
		for _, p := range []struct {
			src sql.NullString
			dst **string
		}{{desc, &it.Description}, {agencyID, &it.AgencyID}, {agencyName, &it.AgencyName},
			{expires, &it.ExpiresAt}, {revoked, &it.RevokedAt}, {lastUsed, &it.LastUsedAt}} {
			if p.src.Valid && p.src.String != "" {
				v := p.src.String
				*p.dst = &v
			}
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
}

// mintedServiceAccount is the ONLY shape that ever carries the plaintext, and
// only in the 201 response to the request that created it.
type mintedServiceAccount struct {
	serviceAccountRow
	Token string `json:"token"`
}

// isUniqueViolation reports whether err is SQLite's UNIQUE-constraint failure.
// Matched on the message because the sqlite3 driver's extended result codes are
// not surfaced as a typed error here; a name collision must read as a 409, not
// a 500.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// newServiceToken generates the plaintext credential: 32 bytes of crypto/rand,
// URL-safe base64, prefixed so a leaked string is greppable and identifiable at
// a glance in a log or a config file.
func newServiceToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "amasvc_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Server) createServiceAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Role        string `json:"role"`
		AgencyID    string `json:"agencyId"`
		AllScopes   bool   `json:"allScopes"`
		ExpiresAt   string `json:"expiresAt"` // RFC3339; empty ⇒ no expiry
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Role = strings.ToLower(strings.TrimSpace(in.Role))
	in.AgencyID = strings.TrimSpace(in.AgencyID)

	if in.Name == "" {
		httpx.Fail(w, http.StatusBadRequest, "validation", "name is required")
		return
	}
	// The name becomes the audit actor as `svc:<name>`, so it must not carry a
	// colon or whitespace that would make an actor string ambiguous to read or
	// to filter on.
	if strings.ContainsAny(in.Name, ": \t\n") {
		httpx.Fail(w, http.StatusBadRequest, "validation",
			"name may not contain spaces or ':' — it becomes the audit actor 'svc:<name>'")
		return
	}
	if !auth.RoleValid(in.Role) {
		httpx.Fail(w, http.StatusBadRequest, "validation", "unknown role "+strconv.Quote(in.Role))
		return
	}
	// The same one-shape-per-row rule access_grants enforces, checked here so the
	// caller gets a sentence instead of a CHECK-constraint error.
	if in.AllScopes == (in.AgencyID != "") {
		httpx.Fail(w, http.StatusBadRequest, "validation",
			"a service account is bound to exactly one of: an agency, or all scopes")
		return
	}
	if in.AgencyID != "" {
		var n int
		_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, in.AgencyID).Scan(&n)
		if n == 0 {
			httpx.Fail(w, http.StatusBadRequest, "validation", "unknown agency")
			return
		}
	}
	var expires sql.NullString
	if in.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, in.ExpiresAt)
		if err != nil {
			httpx.Fail(w, http.StatusBadRequest, "validation", "expiresAt must be an RFC3339 timestamp")
			return
		}
		if !t.After(time.Now().UTC()) {
			httpx.Fail(w, http.StatusBadRequest, "validation", "expiresAt must be in the future")
			return
		}
		expires = sql.NullString{String: t.UTC().Format(time.RFC3339), Valid: true}
	}

	token, err := newServiceToken()
	if err != nil {
		httpx.Fail500(w, s.log, "internal", err)
		return
	}
	rowID := db.NewID()
	now := time.Now().UTC().Format(time.RFC3339)
	allScopes := 0
	if in.AllScopes {
		allScopes = 1
	}
	_, err = s.db.ExecContext(r.Context(), `
		INSERT INTO service_accounts
			(id, name, description, token_hash, role, agency_id, all_scopes, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rowID, in.Name, nullStrIf(in.Description), auth.HashToken(token), in.Role,
		nullStrIf(in.AgencyID), allScopes, id.Email, now, expires)
	if err != nil {
		if isUniqueViolation(err) {
			httpx.Fail(w, http.StatusConflict, "conflict", "a service account named "+strconv.Quote(in.Name)+" already exists")
			return
		}
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	_ = auditlog.WriteChangeLog(r.Context(), s.db, id.Email, "Access", "Created", in.Name,
		"Service account created (role "+in.Role+")")
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		Kind: "config", Actor: id.Email, Category: "Access", Action: "Created", Target: in.Name,
		Summary: "Service account created",
	})

	out := mintedServiceAccount{
		serviceAccountRow: serviceAccountRow{
			ID: rowID, Name: in.Name, Role: in.Role, AllScopes: in.AllScopes,
			CreatedBy: id.Email, CreatedAt: now, Status: "active",
		},
		Token: token,
	}
	if in.Description != "" {
		out.Description = &in.Description
	}
	if in.AgencyID != "" {
		out.AgencyID = &in.AgencyID
	}
	if expires.Valid {
		out.ExpiresAt = &expires.String
	}
	httpx.JSON(w, http.StatusCreated, out)
}

func (s *Server) revokeServiceAccount(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	rowID := r.PathValue("id")

	var name string
	var revoked sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT name, revoked_at FROM service_accounts WHERE id = ?`, rowID).Scan(&name, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "service account not found")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if revoked.Valid {
		httpx.Fail(w, http.StatusConflict, "conflict", "service account is already revoked")
		return
	}

	// Revoke rather than delete: the account name is an audit actor on every run
	// it ever triggered, and a deleted row makes those rows unreadable. This is
	// the registration-token precedent (token.go), for the same reason.
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE service_accounts SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339), rowID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// TOCTOU guard, as HandleRevokeRegistrationToken does: two concurrent revokes
	// must not both report success.
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "service account is already revoked")
		return
	}

	_ = auditlog.WriteChangeLog(r.Context(), s.db, id.Email, "Access", "Revoked", name, "Service account revoked")
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		Kind: "config", Actor: id.Email, Category: "Access", Action: "Revoked", Target: name,
		Summary: "Service account revoked",
	})
	w.WriteHeader(http.StatusNoContent)
}

// ─────────────────────────────────────────────────────────────────────────────
// Trigger endpoints (ET-B)
// ─────────────────────────────────────────────────────────────────────────────

// resolveDefinitionByName maps a name (+ optional ?source=) to a rowid under the
// dual-source model. Returns ok=false having already written the response.
//
// The ambiguity case matters: git and amadeus namespaces are disjoint, so the
// SAME name can name two different definitions. Picking one silently would mean
// a monitoring system triggering whichever the query planner returned first.
func (s *Server) resolveDefinitionByName(w http.ResponseWriter, r *http.Request, table, name string) (int64, string, bool) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source != "" && source != "git" && source != "amadeus" {
		httpx.Fail(w, http.StatusBadRequest, "validation", "source must be 'git' or 'amadeus'")
		return 0, "", false
	}

	// RH: a recycle-binned definition resolves as absent. Without this the name
	// stays claimed (PRIMARY KEY (source, name)) but resolves to a rowid the
	// session routes would 404 on — a machine caller could fire a binned job.
	query := `SELECT rowid, source FROM ` + table + ` WHERE name = ? AND deleted_at IS NULL`
	args := []any{name}
	if source != "" {
		query += ` AND source = ?`
		args = append(args, source)
	}
	query += ` ORDER BY source`

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return 0, "", false
	}
	defer rows.Close()
	type hit struct {
		id  int64
		src string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if err := rows.Scan(&h.id, &h.src); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return 0, "", false
		}
		hits = append(hits, h)
	}
	switch len(hits) {
	case 0:
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such definition: "+name)
		return 0, "", false
	case 1:
		return hits[0].id, hits[0].src, true
	default:
		srcs := make([]string, 0, len(hits))
		for _, h := range hits {
			srcs = append(srcs, h.src)
		}
		httpx.Fail(w, http.StatusConflict, "ambiguous_name",
			"the name "+strconv.Quote(name)+" exists in more than one source ("+strings.Join(srcs, ", ")+
				"); pass ?source= to choose")
		return 0, "", false
	}
}

// triggerJobByName is the token-authenticated job trigger.
//
// It resolves the name, applies the ONE gate that is specific to machine
// triggers (requestable, PF-Q15), and then hands the request to runJob
// unchanged. Delegation rather than a parallel implementation is deliberate:
// runJob is ~800 lines of gates — scope+verb, RB-26/RB-27, unbound references,
// connect-as identity, host/group subsetting, ansible option validation, the
// concurrency cap, Forbid, declared-input prompt enforcement — and a second
// copy of that would drift on the first change to either.
func (s *Server) triggerJobByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rowid, _, ok := s.resolveDefinitionByName(w, r, "jobs", name)
	if !ok {
		return
	}

	// PF-Q15 — external triggerability is opt-in per job. `requestable` has been
	// parsed, persisted and echoed since A7 without ever being enforced; this is
	// the enforcement it was waiting for. It gates ONLY machine triggers: a human
	// clicking Run is unaffected, because the question this answers is "may this
	// job be started by something outside the UI", not "may it be started".
	//
	// The column defaults to 0, so every existing job is closed until an operator
	// opts it in — the correct default for a surface reachable with a bearer
	// token, and the reason the refusal names the field and where to set it.
	var requestable int
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(requestable,0) FROM jobs WHERE rowid = ?`, rowid).Scan(&requestable); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if requestable == 0 {
		id, _ := auth.IdentityFrom(r.Context())
		if s.auth != nil {
			s.auth.AuditDenied(r, id.Email, "not_requestable", "triggerJobs",
				auditDetails(r, "job is not marked requestable"))
		}
		httpx.Fail(w, http.StatusForbidden, "not_requestable",
			"job "+strconv.Quote(name)+" is not externally triggerable — set 'requestable: true' on the job "+
				"(Job Composer → Advanced, or the job's YAML) to allow service-account triggers")
		return
	}

	// runJob reads the rowid from the {jobId} path value; supply it and let every
	// gate downstream run against the token's Identity exactly as it would for a
	// session. TriggerKind 'webhook' already exists in the runs CHECK with no
	// producer (migration 890), so this needs no schema change.
	r.SetPathValue("jobId", strconv.FormatInt(rowid, 10))
	s.runJobWithKind(w, r, triggerKindAPI)
}

// triggerWorkflowByName is the workflow twin.
//
// ⚠️ Workflows have NO `requestable` column, so there is no per-definition
// opt-in here in v1 — the token's grant is the whole gate. That asymmetry is
// deliberate and recorded rather than papered over: adding the field to
// workflows means a YAML surface, a compose input, an OpenAPI field and an
// editor control, which is its own phase. Until then, scope a workflow-
// triggering token narrowly.
func (s *Server) triggerWorkflowByName(eng *workflow.Engine) http.HandlerFunc {
	inner := s.triggerWorkflow(eng)
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		rowid, _, ok := s.resolveDefinitionByName(w, r, "workflows", name)
		if !ok {
			return
		}
		r.SetPathValue("workflowId", strconv.FormatInt(rowid, 10))
		inner(w, r)
	}
}
