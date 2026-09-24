package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// mountBindings wires the reference-binding surface (vault-integration.md P1.1):
// a job or script declares which Env Vars references it consumes, so the
// dispatch-time resolver (internal/runref, P1.2) injects ONLY the declared
// references (D2 = 2B explicit binding). Reads are session-gated; writes require
// ManageEnvVars (+ CSRF) — a binding is a least-privilege access control over
// secrets, so it lives behind the same gate as secret management, not the
// any-user tags gate. The body scan is a read (GET) that suggests bindings.
func (s *Server) mountBindings(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	manageEnvVars := s.requirePerm("manageEnvVars", permManageEnvVars)

	mux.Handle("GET /api/v1/job-reference-bindings/{jobId}",
		requireSession(http.HandlerFunc(s.getJobBindings)))
	mux.Handle("PUT /api/v1/job-reference-bindings/{jobId}",
		manageEnvVars(http.HandlerFunc(s.putJobBindings)))

	// Trailing wildcard {name...} so a sub-folder script name (a repo path)
	// resolves — same shape as /scripts/{name...} and /script-tags/{name...}.
	mux.Handle("GET /api/v1/script-reference-bindings/{name...}",
		requireSession(http.HandlerFunc(s.getScriptBindings)))
	mux.Handle("PUT /api/v1/script-reference-bindings/{name...}",
		manageEnvVars(http.HandlerFunc(s.putScriptBindings)))
	mux.Handle("GET /api/v1/script-reference-scan/{name...}",
		requireSession(http.HandlerFunc(s.scanScriptBindings)))

	// Run-detail injected references (P1.8): the reference NAMES a run injects,
	// derived from its job's (+ referenced script's) declared bindings. Names + kind
	// only, NEVER values. Scope-guarded like the run detail (FR-H2 IDOR).
	mux.Handle("GET /api/v1/runs/{traceId}/references",
		requireSession(http.HandlerFunc(s.getRunReferences)))

	// Authoring-time validation (agencies plan T1.1, AG-Q4): would these bindings
	// resolve for a run in this scope? Session-gated, not ManageEnvVars — the
	// answer is bounded by the CALLER's own row visibility (runref.Validate), so it
	// reveals nothing their Env Vars / Secrets list does not already show, and the
	// chips must render for the read-only operators who look at job detail. It is a
	// POST because the payload is a binding SET (a job resolves every chip in one
	// round-trip, and the Run dialog re-resolves the set on every scope override) —
	// CSRF-gated anyway so a cross-origin page cannot probe names on a live session.
	mux.Handle("POST /api/v1/references/validate",
		requireSession(s.auth.RequireCSRF(http.HandlerFunc(s.validateReferences))))

	// Reference usage (T1.9): how many jobs/scripts BIND each reference, so an
	// admin can see what an Env Vars row is actually wired into before editing its
	// scope. Counts only — never values, never which job.
	mux.Handle("GET /api/v1/reference-usage",
		requireSession(http.HandlerFunc(s.getReferenceUsage)))
}

// validateReferences answers the authoring-time question dispatch answers vaguely:
// would each declared reference resolve for a run in `scope`? See runref/validate.go
// for the oracle-safety contract — precision is bounded ROW BY ROW by the caller's
// own scope grants, which is what keeps AG-Q4(a) from becoming the back-door oracle
// M2 forbids on the run surface.
func (s *Server) validateReferences(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	var body struct {
		Scope      string `json:"scope"`
		References []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			As   string `json:"as"`
		} `json:"references"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", "request body is not valid JSON")
		return
	}
	// The scope being previewed must itself be readable: a restricted actor may not
	// ask "what would resolve in prod?" about a scope they cannot see, or the
	// per-row filter below would be answering a question they may not pose.
	if !auth.ScopeReadable(actor, body.Scope) {
		s.denyScope(w, r, body.Scope, "scope access denied")
		return
	}
	if len(body.References) > maxValidateReferences {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"too many references in one request")
		return
	}
	in := make([]runref.Binding, 0, len(body.References))
	for _, b := range body.References {
		in = append(in, runref.Binding{Kind: runref.Kind(b.Kind), Name: b.Name, As: strings.TrimSpace(b.As)})
	}
	results, err := runref.ValidateAll(r.Context(), s.db, in, body.Scope, actor.Grant().CanRead)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if results == nil {
		results = []runref.Validation{}
	}
	// RF-4/M6: this endpoint is the Run dialog's pre-check, so it must answer the
	// same question the trigger will. Resolution alone is no longer sufficient —
	// runJob additionally requires manageEnvVars on the agency that owns the row —
	// and a preview that says "resolves ✓" for a reference the trigger then 403s is
	// the discover-the-rule-via-a-403 shape RB-29 exists to eliminate.
	//
	// Downgraded rather than dropped: the operator still sees the row and is told
	// why it will be refused, which is what makes the dialog actionable.
	runAgencies, _ := execspec.ScopeAgencies(r.Context(), s.db, body.Scope)
	for i := range results {
		if !results[i].OK {
			continue
		}
		// Entitlement is judged on the ROW (kind+name), never the alias — the alias is
		// a destination and must not be able to select or widen anything (§2.2).
		permitted, err := s.refEntityPermitted(r.Context(), actor,
			runref.Binding{Kind: results[i].Kind, Name: results[i].Name}, body.Scope, runAgencies)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if !permitted {
			results[i].OK = false
			results[i].Outcome = runref.OutcomeOutOfScope
			results[i].Reason = "belongs to a department you do not have " +
				auth.PermManageEnvVars + " on, so it cannot be attached to a run"
		}
	}

	// RA-24, authoring half — the same rule the trigger enforces, applied where it
	// is cheap to obey. With NO scope bound, the run's agency snapshot is empty, so
	// an agency-owned row resolves for nobody and the trigger will 422. Without this
	// the Run dialog would show a green chip for exactly that reference: the
	// discover-the-rule-via-an-error shape RB-29 exists to eliminate, and the same
	// lesson RF-4 applied to the permission half two releases ago.
	//
	// Only for the unbound case, and only for references that otherwise passed —
	// a verdict already downgraded has a more specific reason worth keeping.
	if body.Scope == "" {
		for i := range results {
			if !results[i].OK {
				continue
			}
			_, found, err := runref.LookupEntityID(r.Context(), s.db,
				results[i].Kind, results[i].Name, "", nil)
			if err != nil && !errors.Is(err, runref.ErrAmbiguousReference) {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if err != nil || !found {
				results[i].OK = false
				results[i].Outcome = runref.OutcomeOutOfScope
				results[i].Reason = "is owned by a department, so it resolves for nobody on a run " +
					"with no scope; bind a scope to use it"
			}
		}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{"results": results})
}

// maxValidateReferences bounds one validation request. A job's binding set is a
// handful of rows; a large body is either a mistake or an attempt to turn one
// request into a bulk name-probe. The per-row scope filter already makes probing
// useless, so this is a cost bound, not the security control.
const maxValidateReferences = 200

// getReferenceUsage returns, per reference (kind + bare name), how many jobs and
// scripts declare a binding to it (T1.9). Job counts are SCOPE-FILTERED to the
// caller's grants — an unfiltered count would let a restricted actor infer that an
// out-of-scope job binds a given name. Script bindings carry no scope (scripts are
// a single shared catalog) and are counted for every session, matching the existing
// GET /script-reference-bindings read gate.
func (s *Server) getReferenceUsage(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	// One flat query joined to jobs for the owner's scope — never a nested iterator
	// (pool deadlock, see db.maxOpenConns).
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT rb.ref_kind, rb.ref_name, rb.owner_kind, COALESCE(j.scope,'')
		FROM reference_bindings rb
		LEFT JOIN jobs j
		  ON rb.owner_kind = 'job' AND j.source = rb.owner_source AND j.name = rb.owner_name`)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	type usage struct {
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Jobs    int    `json:"jobs"`
		Scripts int    `json:"scripts"`
	}
	agg := map[string]*usage{}
	for rows.Next() {
		var refKind, refName, ownerKind, jobScope string
		if err := rows.Scan(&refKind, &refName, &ownerKind, &jobScope); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if ownerKind == "job" && !auth.ScopeReadable(actor, jobScope) {
			continue
		}
		k := refKind + "\x00" + refName
		u := agg[k]
		if u == nil {
			u = &usage{Kind: refKind, Name: refName}
			agg[k] = u
		}
		if ownerKind == "job" {
			u.Jobs++
		} else {
			u.Scripts++
		}
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	out := make([]usage, 0, len(agg))
	for _, u := range agg {
		out = append(out, *u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	httpx.JSON(w, http.StatusOK, map[string]any{"usage": out})
}

// getRunReferences returns the reference bindings a run ACTUALLY injected at
// dispatch (P1.8) — names + kind + derived reference only, never values — so run
// detail can show WHICH references a run received. It reads the authoritative P1.6
// dispatch-injection audit (change_log) rather than re-deriving from live bindings:
// the audit is the dispatch-time snapshot (immune to later binding edits), it
// already excludes SSH-skipped keys, and a refused/kill-switched run has no audit
// row → an honest empty result. Empty (nothing injected, or not yet dispatched) is
// a valid 200.
func (s *Server) getRunReferences(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	var scope string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT COALESCE(scope,'') FROM runs WHERE id = ?`, traceID).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	// Scope guard, mirroring getRun (FR-H2): a restricted operator must not see the
	// references of an out-of-scope run.
	if id, ok := auth.IdentityFrom(r.Context()); ok && !auth.ScopeReadable(id, scope) {
		s.denyScope(w, r, scope, "scope access denied")
		return
	}

	// The dispatch-injection audit row for this run (P1.6, AuditInjectionOnce writes
	// it once). Absent ⇒ the run injected nothing (or hasn't dispatched) ⇒ empty.
	var details string
	aerr := s.db.QueryRowContext(r.Context(),
		`SELECT details FROM change_log WHERE category = 'Secrets' AND action = 'injected' AND target = ?
		 ORDER BY at DESC LIMIT 1`, traceID).Scan(&details)
	if aerr != nil && !errors.Is(aerr, sql.ErrNoRows) {
		httpx.Fail500(w, s.log, "db_error", aerr)
		return
	}
	refs := runref.ParseAuditReferences(details)
	if refs == nil {
		refs = []runref.Binding{}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"references": refs})
}

// bindingsBody is the PUT payload: a full-replace set of {kind, name, as} entries.
// reference is server-derived and ignored on write; `as` is the optional RA-1
// alias — the bare destination name the value is injected under, empty ⇒ the row's
// own name.
type bindingsBody struct {
	Bindings []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
		As   string `json:"as"`
	} `json:"bindings"`
}

func decodeBindings(w http.ResponseWriter, r *http.Request) ([]runref.Binding, bool) {
	var body bindingsBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_json", "request body is not valid JSON")
		return nil, false
	}
	out := make([]runref.Binding, 0, len(body.Bindings))
	for _, b := range body.Bindings {
		out = append(out, runref.Binding{Kind: runref.Kind(b.Kind), Name: b.Name, As: strings.TrimSpace(b.As)})
	}
	return out, true
}

// writeBindings maps ReplaceBindings' validation errors (*envref.Error → 422),
// re-reads the stored set, and responds. Returns false having written the
// response on any failure.
func (s *Server) writeBindings(w http.ResponseWriter, r *http.Request, owner runref.Owner, in []runref.Binding, actor, category, target string) bool {
	if err := runref.ReplaceBindings(r.Context(), s.db, owner, in, actor); err != nil {
		if ve, ok := errors.AsType[*envref.Error](err); ok {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_binding", ve.Error())
			return false
		}
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	bs, err := runref.ListBindings(r.Context(), s.db, owner)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	// M7: record the RESULTING binding set (kinds + bare names, never values) so a
	// stale full-replace that silently revokes/resurrects a binding is visible in the
	// audit — not an empty "updated" row indistinguishable from a tag edit.
	_ = settings.WriteChangeLog(r.Context(), s.db, actor, category, "updated", target, runref.AuditBindingSet(bs))
	httpx.JSON(w, http.StatusOK, map[string]any{"bindings": bs})
	return true
}

func (s *Server) getJobBindings(w http.ResponseWriter, r *http.Request) {
	jr := s.fetchJobByID(r, r.PathValue("jobId"))
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	// M3(b): the binding set enumerates a job's declared reference NAMES — a
	// scope-restricted operator must not read those for an out-of-scope job. Mirror
	// getRunReferences' guard (403 on out-of-scope). (Job DETAIL is now scope-gated
	// too — getJob returns 404 out-of-scope, SU-2.)
	if id, ok := auth.IdentityFrom(r.Context()); ok && jr.Scope != nil &&
		!auth.ScopeReadable(id, *jr.Scope) {
		s.denyScope(w, r, *jr.Scope, "scope access denied")
		return
	}
	bs, err := runref.ListBindings(r.Context(), s.db, runref.Owner{Kind: "job", Source: jr.Source, Name: jr.Name, UID: jr.UID})
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"bindings": bs})
}

func (s *Server) putJobBindings(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	jr := s.fetchJobByID(r, r.PathValue("jobId"))
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	// M1: a binding is an access-control write over the run's injected references, so
	// it must be scope-gated on the OWNER job's scope — not left ManageEnvVars-only.
	// A restricted manager could otherwise add a bogus binding to a prod job (every
	// prod dispatch fails closed) or empty its set (prod runs execute WITHOUT their
	// declared secrets). Mirror loadSecretWritable: 404 when the job is out of read
	// scope (no existence oracle), 403 on a global job a restricted actor may not
	// write.
	jobScope := ""
	if jr.Scope != nil {
		jobScope = *jr.Scope
	}
	if !auth.ScopeReadable(actor, jobScope) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if !auth.ScopeWritable(actor, jobScope) {
		s.denyScope(w, r, jobScope, "cannot modify bindings on a job outside your access")
		return
	}
	in, ok := decodeBindings(w, r)
	if !ok {
		return
	}
	// R2F-1 — the owner is the job's IDENTITY, not its name: this full replace
	// deletes the owner's rows first, and keyed by name it would take a same-named
	// sibling department's bindings with it.
	s.writeBindings(w, r, runref.Owner{Kind: "job", Source: jr.Source, Name: jr.Name, UID: jr.UID}, in, actor.Email, "Jobs", jr.Name)
}

// scriptExists reports whether a script row with this exact name is in the
// catalog. Scripts are keyed by name only (no source dimension), so binding
// owners use owner_source="".
func (s *Server) scriptExists(r *http.Request, name string) bool {
	var n int
	if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM scripts WHERE name = ?`, name).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func (s *Server) getScriptBindings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.scriptExists(r, name) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	bs, err := runref.ListBindings(r.Context(), s.db, runref.Owner{Kind: "script", Name: name})
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"bindings": bs})
}

func (s *Server) putScriptBindings(w http.ResponseWriter, r *http.Request) {
	actor, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	// M1/DEC-3: scripts have no scope column and are shared across jobs of many
	// scopes, so a per-scope gate is not well-defined. Require an UNRESTRICTED
	// manager to edit script bindings — the simplest safe rule (a restricted manager
	// rarely authors shared scripts). A restricted actor is refused with 403.
	if !actor.Unrestricted() {
		// The requirement is not a scope but "any scope", so this audits with the
		// AllScopes target rather than a scope name (see denyUnrestricted).
		s.denyUnrestricted(w, r,
			"script bindings are shared across scopes; only an unrestricted manager may edit them")
		return
	}
	name := r.PathValue("name")
	if !s.scriptExists(r, name) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	in, ok := decodeBindings(w, r)
	if !ok {
		return
	}
	s.writeBindings(w, r, runref.Owner{Kind: "script", Name: name}, in, actor.Email, "Scripts", name)
}

// scanScriptBindings scans a script's body and returns the reference bindings it
// declares in derived form (suggested), plus a bare-name lint (reference sites
// still on the legacy fallback chain that match a known Env Vars row). The
// suggestions prefill a script's binding set (W5 → D2 handoff).
func (s *Server) scanScriptBindings(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	body, ok := s.loadScriptBody(w, r, name)
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"suggested":      runref.ScanBody(body),
		"bareReferences": runref.LintBareNames(body, s.knownReferenceNames(r)),
	})
}

// loadScriptBody resolves a script's executable body (inline command/script, or a
// repo file read from the synced clone). Mirrors getScriptContent's resolution;
// writes the response and returns ok=false on a missing/unreadable script.
func (s *Server) loadScriptBody(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	var command, script, scriptPath sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT command, script, script_path FROM scripts WHERE name = ?`, name).
		Scan(&command, &script, &scriptPath)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return "", false
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return "", false
	}
	switch {
	case command.Valid && command.String != "":
		return command.String, true
	case script.Valid && script.String != "":
		return script.String, true
	case scriptPath.Valid && scriptPath.String != "":
		data, _, rerr := execspec.SafeReadRepoFile(gitlab.DefaultCloneDir(), scriptPath.String, scriptBodyDisplayCap)
		if rerr != nil {
			httpx.Fail(w, http.StatusConflict, "script_unreadable",
				"script file could not be read from the synced repository")
			return "", false
		}
		return string(data), true
	default:
		return "", true // empty body — nothing to scan
	}
}

// knownReferenceNames maps every Env Vars row's bare name → its reference kind,
// for the bare-name migration lint. Three small standalone queries (never nested
// iterators — pool deadlock, see db.maxOpenConns).
//
// M3(a): the scan endpoint echoes matches back (LintBareNames), so an UNFILTERED
// map lets any session confirm out-of-scope secret/var names by planting tokens in
// a script body — a cross-scope existence oracle in tension with P1.7's
// scope-filtered lists. Secrets and env_vars carry a scope column, so they are
// filtered by the caller's grants (global always visible; unrestricted "*" ⇒ all;
// empty grants ⇒ global only, A5 fix). ssh_credentials has NO scope column (access
// is the host/bastion FK — the accepted §8 limitation), so key labels are not
// scope-filtered.
func (s *Server) knownReferenceNames(r *http.Request) map[string]runref.Kind {
	id, _ := auth.IdentityFrom(r.Context())
	known := map[string]runref.Kind{}
	loadScoped := func(query string, kind runref.Kind) {
		rows, err := s.db.QueryContext(r.Context(), query)
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var scope sql.NullString
			if rows.Scan(&name, &scope) == nil && name != "" && auth.ScopeReadable(id, scope.String) {
				known[name] = kind
			}
		}
	}
	loadScoped(`SELECT key, scope FROM secrets`, runref.KindSecret)
	loadScoped(`SELECT key, scope FROM env_vars`, runref.KindVar)
	// Keys: no scope column (§8) — labels are not scope-filtered.
	rows, err := s.db.QueryContext(r.Context(), `SELECT label FROM ssh_credentials`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil && name != "" {
				known[name] = runref.KindKey
			}
		}
	}
	return known
}
