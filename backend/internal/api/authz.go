package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"net/http"
	"slices"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Authorization denial helpers (LU-9).
//
// Before this, every scope denial in this package was a hand-written 403 and
// NOTHING recorded it. That is the one authz event an auditor actually needs:
// a 403 is the visible edge of someone reaching for a resource they were not
// granted, and a burst of them across many scopes is an enumeration attempt.
// Routing every denial through one helper is what makes that burst visible as a
// pattern rather than as N unrelated log lines that were never written at all.
//
// Two rules the call sites depend on:
//
//   - The response is byte-identical to the hand-written 403 it replaces. The
//     message is passed in, not derived, because several are specific ("cannot
//     create a secret in a scope outside your access") and the SPA surfaces them
//     verbatim. Auditing must not change what an operator sees.
//   - Only DECISIONS are audited, never row-level FILTERS. A filter that skips
//     rows the caller may not see is normal operation and would emit one event
//     per row, drowning the real denials. See the deliberately-untouched sites in
//     schedules_api.go (scheduleOwnerReadable), bindings_mount.go and
//     settings_mount.go's list handlers.
//
// The 404-shaped denials are also left alone on purpose: they return 404 rather
// than 403 so an out-of-scope caller cannot use the status code as an existence
// oracle, and auditing them here would be auditing a lookup miss.

// auditActor returns the caller's email for an audit row, or "" when the request
// carries no identity. Empty is a legitimate value in auth_events (see
// AuthEventParams.Actor) — inventing a sentinel would corrupt the column an
// auditor filters on.
func auditActor(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		return id.Email
	}
	return ""
}

// auditDetails describes WHICH request was denied. The audit row already carries
// the actor and addressing; without the method and path an auditor cannot tell a
// single fat-fingered request from a walk across the API surface.
func auditDetails(r *http.Request, message string) string {
	if r == nil {
		return message
	}
	return r.Method + " " + r.URL.Path + ": " + message
}

// denyScope writes the 403 for a scope denial and records it (LU-9).
//
// scope is the scope the caller failed to reach and becomes the audit target, so
// "which scopes is this actor probing?" is a single GROUP BY. message is the
// operator-facing text and is emitted unchanged.
func (s *Server) denyScope(w http.ResponseWriter, r *http.Request, scope, message string) {
	if s.auth != nil {
		s.auth.AuditDenied(r, auditActor(r), "insufficient_scope", scope, auditDetails(r, message))
	}
	httpx.Fail(w, http.StatusForbidden, "forbidden", message)
}

// requireCan is the in-handler counterpart to requirePerm for SCOPED permissions
// (RB-1/RB-5, the rbac-update plan).
//
// requirePerm cannot serve these: the object's scope is not known until the object
// is fetched, so a scoped verb cannot be mount-time middleware. Call this beside
// the existing ScopeReadable/ScopeWritable guard at the same route — the pairing is
// deliberate and the two answer different questions:
//
//	ScopeReadable → may this actor SEE this scope at all   (visibility, unioned)
//	requireCan    → may this actor do THIS VERB on it      (authority, per-grant)
//
// Visibility stays unioned across grants on purpose (§2.2): Alice SHOULD see both
// Tax and Finance; she just may not act on Tax. Splitting visibility too would be a
// far larger and much riskier change than the one being made.
//
// It reports whether the caller may proceed, and on denial has already written the
// 403 and the audit row — so the call site is `if !s.requireCan(...) { return }`.
//
// The audit row carries reason insufficient_permission (matching requirePerm, so
// one query finds both) with the permission name AND the scope in the details. The
// scope is what makes "why was I denied?" answerable under departmental RBAC:
// without it the row says only that a verb was refused, not where.
//
// Wired at every execution route since v0.56.4 (RB-2): runJob, killJob,
// pauseJob/resumeJob, triggerWorkflow, patchWorkflow and the workflow-run cancel
// all route their verb checks through here (directly or via workflowScopesPermit).
// requireReadableJob is the SU-2 read gate for job sub-routes, extracted
// (FX2-F1) from the block getJob and listFileSightings carried verbatim:
// fetch by rowid → binned-reads-as-absent 404 → no-oracle ScopeReadable 404.
// On refusal the 404 is already written; the call site is
// `jr, ok := s.requireReadableJob(w, r, jobID); if !ok { return }`.
//
// Extracted because the copy is a proven omission hazard: the sightings route
// first shipped WITHOUT the gate, behind a comment asserting fetchJobByID
// provided one (it filters nothing) — and one omitted copy is a cross-scope
// information leak. Routes that VARY the shape stay on their own guards, on
// purpose: pause/resume follow with a requireCan verb check and 403,
// putJobBindings splits 404/403 across Readable/Writable, the tags routes
// admit binned definitions (FX-Q5), and killJob is the studied exception that
// acts on the run rather than the definition. If a new sub-route needs exactly
// "may this actor read this job at all", it belongs here.
func (s *Server) requireReadableJob(w http.ResponseWriter, r *http.Request, jobID string) (*jobRow, bool) {
	jr := s.fetchJobByID(r, jobID)
	if jr == nil || jr.DeletedAt != nil {
		// RH: a binned job reads as absent — fetchJobDetail deliberately does NOT
		// filter deleted rows (the recycle bin must read them), so every acting
		// route refuses one here.
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return nil, false
	}
	// SU-2: 404, never 403, so the route cannot serve as a cross-scope existence
	// oracle for enumerable rowids. Unrestricted actors and global (NULL-scope)
	// jobs pass.
	if id, hasID := auth.IdentityFrom(r.Context()); hasID && !id.Unrestricted() {
		scope := ""
		if jr.Scope != nil {
			scope = *jr.Scope
		}
		if !auth.ScopeReadable(id, scope) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
			return nil, false
		}
	}
	return jr, true
}

func (s *Server) requireCan(w http.ResponseWriter, r *http.Request, id auth.Identity, perm, scope string) bool {
	if id.Can(perm, scope) {
		return true
	}
	where := scope
	if where == "" {
		// The unscoped/global object. Naming it "" in an audit row is unreadable;
		// the AllScopes sentinel is what the resolver already uses for "everywhere"
		// and reads correctly in both the target column and the message.
		where = auth.AllScopes
	}
	if s.auth != nil {
		s.auth.AuditDenied(r, auditActor(r), "insufficient_permission", perm,
			auditDetails(r, "insufficient permissions for "+perm+" on scope "+where))
	}
	// RB-25 completes this message with the grant the actor DOES hold ("you have
	// Operator, but on Finance, and this job is in Tax"); until then it names the
	// permission and the scope, which is already strictly more than the bare
	// "insufficient permissions" it replaces.
	httpx.Fail(w, http.StatusForbidden, "forbidden",
		"insufficient permissions: "+perm+" is required on scope "+where)
	return false
}

// denyUnrestricted writes the 403 for a guard that requires an UNRESTRICTED
// actor rather than access to any one scope (currently only script bindings,
// which are shared across scopes and so have no scope to name).
//
// It reuses reason insufficient_scope with target "*" — the same AllScopes
// sentinel the resolver uses for "every scope" — rather than minting a distinct
// reason, so a query for scope denials does not silently miss this one. The
// details string says plainly that the requirement was unrestricted access, for
// the auditor reading a row rather than aggregating.
func (s *Server) denyUnrestricted(w http.ResponseWriter, r *http.Request, message string) {
	if s.auth != nil {
		s.auth.AuditDenied(r, auditActor(r), "insufficient_scope", auth.AllScopes,
			auditDetails(r, "unrestricted scope access required: "+message))
	}
	httpx.Fail(w, http.StatusForbidden, "forbidden", message)
}

// requireGlobal gates an act on the INSTALLATION rather than on any one agency's
// objects (GC-1): install-wide settings, the audit export, the agency catalog,
// and every shared object that has no owner yet. It requires an UNRESTRICTED
// grant that itself CARRIES perm — id.GlobalAdmin(perm), the predicate
// requireFleetWide has always used for runner tokens.
//
// The two weaker checks it replaces are the reason it exists. requirePerm asks
// "do you hold this verb SOMEWHERE", so an administrator of one agency passed it
// and could rewrite the Vault connection for all of them. A bare
// id.Unrestricted() asks "does ANY grant reach every scope" and is
// permission-blind: a viewer on all scopes who is also an admin of one agency
// reads as unrestricted. Neither is "a global administrator".
//
// It chains requirePerm first so the session + CSRF checks and the plain
// "you lack this permission entirely" 403 are unchanged; only the caller who
// holds the verb on one agency sees the new refusal.
func (s *Server) requireGlobal(perm string) func(http.Handler) http.Handler {
	has := func(p rolePermissions) bool { return p.Has(perm) }
	return func(next http.Handler) http.Handler {
		return s.requirePerm(perm, has)(s.globalOnly(perm,
			"this changes the whole installation, not one agency — only an administrator "+
				"whose "+perm+" grant covers every agency may do it", next))
	}
}

// globalOnly is the in-chain half of requireGlobal, for routes that already sit
// behind requirePerm and need their own refusal sentence (requireFleetWide).
func (s *Server) globalOnly(perm, message string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if !id.GlobalAdmin(perm) {
			s.denyEntityAgency(w, r, id, perm, auth.AllScopes, message)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isGlobal reports whether the caller is a global administrator for perm, for
// handlers that shape a response on it rather than refuse (GC-14).
func isGlobal(r *http.Request, perm string) bool {
	id, ok := auth.IdentityFrom(r.Context())
	return ok && id.GlobalAdmin(perm)
}

// requireScopeAgency wraps a scope route with the departmental gate (GC-6): the
// caller must hold configureApp on an agency the scope belongs to. A scope with
// NO agency is shared by every department, so it is a global administrator's —
// the same RB-Q14 rule requireEntityAgency applies to every other unmembered
// entity.
//
// Before this, the scope routes asked only for configureApp SOMEWHERE: an
// administrator of one agency could edit, re-inventory, delete or unbind another
// agency's scope (reproduced 2026-10-06 — PATCH 200, DELETE 204, unbind 200).
// A scope id that matches no row reads as unmembered, so a departmental caller
// gets the same 403 for "not yours" and "does not exist".
//
// Sits INSIDE requirePerm, like requireRunnerAgency.
func (s *Server) requireScopeAgency(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
			"scope_agencies", "scope_id", r.PathValue(pathVar), "scope") {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireHostOwner wraps a write on ONE ssh host record (GC-7, LR-69). A record
// that was imported for a scope follows that scope's gate. A record written by
// hand belongs to its owner_agency: that agency's administrators change it, and
// a global administrator changes Global's. (Until 2.3.0 a hand-written record
// had no owner, applied to every scope, and was a global administrator's alone.)
//
// A host id that matches no row falls through to the handler's own 404.
func (s *Server) requireHostOwner(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		var scopeID sql.NullString
		var owner string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT scope_id, owner_agency FROM ssh_hosts WHERE id = ?`, r.PathValue(pathVar)).Scan(&scopeID, &owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			next.ServeHTTP(w, r)
			return
		case err != nil:
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if scopeID.Valid && scopeID.String != "" {
			if !s.requireEntityAgency(w, r, id, auth.PermConfigureApp,
				"scope_agencies", "scope_id", scopeID.String, "scope") {
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !s.requireRecordOwner(w, r, id, owner, "host record") {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireBastionOwner wraps a write on ONE bastion (LR-69): its owner agency's
// administrators, and a global administrator for Global's. An id that matches
// no row falls through to the handler's own 404.
func (s *Server) requireBastionOwner(pathVar string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		var owner string
		err := s.db.QueryRowContext(r.Context(),
			`SELECT owner_agency FROM bastions WHERE id = ?`, r.PathValue(pathVar)).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			next.ServeHTTP(w, r)
			return
		case err != nil:
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if !s.requireRecordOwner(w, r, id, owner, "bastion") {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireRecordOwner is the owner check both wrappers share: configureApp on the
// owning agency, which for Global is the global-administrator check.
func (s *Server) requireRecordOwner(w http.ResponseWriter, r *http.Request, id auth.Identity, owner, label string) bool {
	if id.CanAgency(auth.PermConfigureApp, owner) {
		return true
	}
	if owner == agencyid.Global {
		s.denyEntityAgency(w, r, id, auth.PermConfigureApp, auth.AllScopes,
			"this "+label+" is Global's, so it applies to every agency; only a global administrator may change it")
		return false
	}
	s.denyEntityAgency(w, r, id, auth.PermConfigureApp, owner,
		"you do not have "+auth.PermConfigureApp+" on the agency that owns this "+label)
	return false
}

// requireJobVisible is the read gate for the routes any signed-in user may
// WRITE through — tags and annotations (GC-12). "Any signed-in user" was meant
// as "no role needed", not "no scope needed": these routes address a job by
// row id and used to check nothing, so a viewer in one agency could rewrite the
// tags and notes of another agency's job and receive its full row in reply.
//
// It differs from requireReadableJob in one way, on purpose: a binned job
// passes, because tags and annotations stay editable in the recycle bin
// (FX-Q5). A job the caller cannot read answers 404, never 403 — the same
// no-oracle rule as every other row-id route.
func (s *Server) requireJobVisible(w http.ResponseWriter, r *http.Request, jobID string) bool {
	jr := s.fetchJobByID(r, jobID)
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return false
	}
	id, hasID := auth.IdentityFrom(r.Context())
	if !hasID {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return false
	}
	scope := ""
	if jr.Scope != nil {
		scope = *jr.Scope
	}
	if !id.Unrestricted() && !auth.ScopeReadable(id, scope) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return false
	}
	return true
}

// requireWorkflowVisible is requireJobVisible for a workflow, which has no scope
// of its own: it is visible when every scope its jobs are in — its
// sub-workflows' jobs included — is readable by the caller. A workflow none of
// whose jobs resolve has nothing to protect and passes, as it always has.
func (s *Server) requireWorkflowVisible(w http.ResponseWriter, r *http.Request, wfID string) bool {
	wr := s.fetchWorkflowByID(r, wfID)
	if wr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return false
	}
	id, hasID := auth.IdentityFrom(r.Context())
	if !hasID {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return false
	}
	if id.Unrestricted() {
		return true
	}
	// With the workflow's home (GR-16): the jobs the engine would run for THIS
	// workflow, which for a Git workflow are its own repository's.
	eng := workflow.New(s.db, s.log)
	scopes, err := eng.JobScopesAt(r.Context(), wr.Steps, wr.Source, eng.Home(r.Context(), wr.ID))
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	for _, sc := range scopes {
		if !auth.ScopeReadable(id, sc) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
			return false
		}
	}
	return true
}

// requireEntityAgency is the agency-side counterpart to requireCan, for entities
// whose isolation is expressed as AGENCY MEMBERSHIP rather than as a scope (RB-16 /
// RB-32).
//
// SSH keys and runners have no scope column at all — isolation is pure agency
// intersection (AG-Q5). Secrets and variables carry membership too, and RB-Q2 made
// manageEnvVars departmental precisely so a Tax admin owns Tax's secrets: the routes
// that read and rewrite secret material are gated on manageEnvVars, INCLUDING
// reveal, so leaving them global meant anyone trusted with any department's
// variables could read every department's credentials. That was the
// highest-consequence gap in the plan.
//
// An entity with NO membership is not "member of nothing" but "no agency
// restriction" (AG-Q1(b)) — it is global infrastructure every department consumes.
// RB-Q14 resolves that writes and reveals on such an entity require an UNRESTRICTED
// actor: absence of membership carries no authority, exactly as an unscoped job does
// not authorize itself under RB-26. Reads and run-time consumption are unchanged.
//
// It reports whether the caller may proceed, having already written the 403 and the
// audit row on denial — so the call site is `if !s.requireEntityAgency(...) { return }`.
func (s *Server) requireEntityAgency(w http.ResponseWriter, r *http.Request, id auth.Identity,
	perm, joinTable, joinCol, entityID, label string) bool {

	ok, deniedAgency, err := s.entityAgencyPermitted(r.Context(), id, perm, joinTable, joinCol, entityID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if ok {
		return true
	}
	if deniedAgency == "" {
		s.denyEntityAgency(w, r, id, perm, auth.AllScopes,
			label+" belongs to Global, so it is shared by every agency; only a global "+
				"administrator may change or reveal it")
		return false
	}
	s.denyEntityAgency(w, r, id, perm, deniedAgency,
		"you do not have "+perm+" on the agency that owns this "+label)
	return false
}

// entityAgencyPermitted is the transport-free core of requireEntityAgency, for
// call sites that are not a whole route — the per-run reference/credential
// checks inside runJob and the compose path (RF-4), where the denial response is
// shaped by the caller. deniedAgency is "" when the entity is Global's (RB-Q14's
// shared tier: a global administrator's to change) and the first owning agency
// otherwise; it is meaningful only when permitted is false.
//
// Three outcomes when the entity has NO membership rows (migration 1220):
//
//   - the entity does not exist: there is nothing to own. A global administrator
//     proceeds to the handler's own 404 or 422; anyone else is refused. (An id
//     nothing matches has always read this way, and two callers rely on it: a
//     scope id that names no row, and the ledger of a deregistered runner, whose
//     membership went with it.)
//   - the entity exists: that is an ERROR. Every scope, runner, secret, variable
//     and key is born with a Global row and no route leaves one with none, so
//     the row was deleted around the application. It is never read as "global".
//   - (and with rows, the ordinary case: any one of its agencies suffices.)
func (s *Server) entityAgencyPermitted(ctx context.Context, id auth.Identity,
	perm, joinTable, joinCol, entityID string) (permitted bool, deniedAgency string, err error) {

	rows, err := s.db.QueryContext(ctx,
		`SELECT agency_id FROM `+joinTable+` WHERE `+joinCol+` = ?`, entityID)
	if err != nil {
		return false, "", err
	}
	var agencies []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return false, "", err
		}
		agencies = append(agencies, a)
	}
	// A mid-iteration error would otherwise yield a SHORT membership list, and a
	// short list is an authorization answer: drop the caller's own agency and they
	// are denied; drop every row and the entity reads as unmembered. Both fail
	// closed, but silently — surface it as the 500 it is.
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, "", err
	}
	rows.Close()

	if len(agencies) == 0 {
		catalog, known := membershipCatalog[joinTable]
		if !known {
			return false, "", fmt.Errorf("entityAgencyPermitted: unknown membership table %q", joinTable)
		}
		var exists bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM `+catalog+` WHERE id = ?)`, entityID).Scan(&exists); err != nil {
			return false, "", err
		}
		if exists {
			return false, "", fmt.Errorf("%w: %s %s — every row has at least Global's "+
				"(migration 1220), so this one was changed outside the application", errInNoAgency, catalog, entityID)
		}
		return id.GlobalAdmin(perm), "", nil
	}
	onlyGlobal := true
	for _, a := range agencies {
		if a != agencyid.Global {
			onlyGlobal = false
		}
		if id.CanAgency(perm, a) {
			return true, "", nil
		}
	}
	if onlyGlobal {
		return false, "", nil
	}
	return false, agencies[0], nil
}

// errInNoAgency is entityAgencyPermitted's answer for a row that exists and has
// no membership rows. Every route but one turns it into a 500: nobody can be
// shown to own the row, so nobody is let through. The one is the route that
// GIVES a row its agencies (requireEntityAgencyOrRepair) — without it the row
// could never be put right from inside the application.
var errInNoAgency = errors.New("belongs to no agency")

// requireEntityAgencyOrRepair is requireEntityAgency for the membership setters
// alone. A row in no agency is damage, and assigning it an agency is the repair:
// a GLOBAL administrator may do that (the row is nobody's, and what nobody owns
// is theirs to place), and it is logged. Anyone else gets the 500 every other
// route gives — a department's administrator claiming an orphan would be taking
// a row they cannot be shown to own.
func (s *Server) requireEntityAgencyOrRepair(w http.ResponseWriter, r *http.Request, id auth.Identity,
	perm, joinTable, joinCol, entityID, label string) bool {

	_, _, err := s.entityAgencyPermitted(r.Context(), id, perm, joinTable, joinCol, entityID)
	if errors.Is(err, errInNoAgency) && id.GlobalAdmin(perm) {
		s.log.Warn("agency membership: repairing a row that was in no agency",
			"kind", label, "id", entityID, "actor", id.Email)
		return true
	}
	return s.requireEntityAgency(w, r, id, perm, joinTable, joinCol, entityID, label)
}

// membershipCatalog maps a membership join table to the table its entity lives
// in. Compile-time constants, never input.
var membershipCatalog = map[string]string{
	"scope_agencies":          "scopes",
	"runner_agencies":         "runners",
	"secret_agencies":         "secrets",
	"env_var_agencies":        "env_vars",
	"ssh_credential_agencies": "ssh_credentials",
}

// requireCreationAgencies enforces the RF-Q2(a) creation rule
// (the RBAC-fixes plan): a RESTRICTED creator must place a new
// secret/variable/SSH key in at least one agency they hold perm on. Without
// this, the entity would land with EMPTY membership — which RB-Q14 makes
// unrestricted-only for writes and reveal — locking the creator out of the
// thing they just made. Unrestricted creators may create unmembered (global
// infrastructure, a deliberate statement only they can make) or pass any
// membership; either way every named agency must exist.
//
// RA-9 (Phase D) softens the restricted case from a refusal to an INHERITANCE.
// The rule above was correct but made the safe outcome something the caller had
// to opt into on every create, and the plan's §2.5 argument is that membership
// must stop being paperwork: a department-scoped actor who says nothing now gets
// their own department, which is what they meant. The refusal survives only where
// inheritance has no unambiguous answer — an actor holding perm on SEVERAL
// agencies, where enrolling all of them would widen the row past what they are
// likely to have intended, and where naming one is a real decision rather than a
// formality.
//
// It returns the EFFECTIVE agency set the caller must persist — the caller's own
// list when supplied, the inherited one otherwise — and reports whether creation
// may proceed, having already written the error response on denial.
func (s *Server) requireCreationAgencies(w http.ResponseWriter, r *http.Request, id auth.Identity,
	perm string, agencyIDs []string, label string) ([]string, bool) {

	// ⚠️ The predicate is GlobalAdmin(perm), not Unrestricted(). Unrestricted() is
	// permission-BLIND — it asks only whether some grant reaches every scope, and
	// UnionGrantScopes collapses to ["*"] if ANY grant does. A user who is an
	// unrestricted VIEWER and a Tax ADMIN reads as unrestricted, would be waved
	// through with no agencyIds, and would then be refused by requireEntityAgency on
	// the very next request — because the empty-membership branch demands an
	// unrestricted grant CARRYING THE PERMISSION, which their viewer grant does not.
	// That is precisely the lockout this rule exists to prevent, so the two must ask
	// the same question.
	if !id.GlobalAdmin(perm) && len(agencyIDs) == 0 {
		// RA-9: inherit, when "their department" has exactly one answer. The inherited
		// id still goes through the existence + entitlement loop below, so inheritance
		// can never place a row somewhere an explicit request could not.
		if held := id.AgenciesFor(perm); len(held) == 1 {
			agencyIDs = held
		} else {
			// The body field differs by route (agencyIds for a scope, secret,
			// variable or key; ownerAgency for a host record or bastion), so the
			// sentence names the thing to do, not the field.
			msg := "your access is department-scoped: name the agency that owns this " + label +
				", one of yours, so your department keeps ownership of it"
			if len(held) > 1 {
				// Naming the departments makes this actionable — the actor holds the
				// permission on all of them, so this reveals nothing they cannot list.
				msg = "you hold " + perm + " on more than one department, so this " + label +
					" cannot inherit one: name the owning agency explicitly (" +
					strings.Join(held, ", ") + ")"
			}
			httpx.Fail(w, http.StatusUnprocessableEntity, "agency_required", msg)
			return nil, false
		}
	}
	// A global administrator who names nothing creates in Global (LR-22). It used
	// to be "in no agency"; Global is where that row has always belonged, and it
	// is said as a membership now, so nothing downstream has to read an empty set.
	if len(agencyIDs) == 0 {
		agencyIDs = []string{agencyid.Global}
	}
	// One agency, or Global (LR-7, LR-54). Repeats of one id are one id.
	agencyIDs = slices.Compact(slices.Sorted(slices.Values(agencyIDs)))
	if err := settings.ValidateAgencySet(agencyIDs); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "global_mixed",
			"a new "+label+" is Global's or an agency's, never both: name Global alone, or the agency")
		return nil, false
	}
	if len(agencyIDs) > 1 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "one_agency",
			"a new "+label+" belongs to exactly one agency: name the one that owns it. "+
				"What several agencies need is a copy in each, or one that is Global's")
		return nil, false
	}
	for _, a := range agencyIDs {
		var n int
		if err := s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM agencies WHERE id = ?`, a).Scan(&n); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return nil, false
		}
		if n == 0 {
			httpx.Fail(w, http.StatusUnprocessableEntity, "unknown_agency", "agency "+a+" does not exist")
			return nil, false
		}
		if !id.CanAgency(perm, a) {
			s.denyEntityAgency(w, r, id, perm, a,
				"you do not have "+perm+" on agency "+a+", so you cannot place a new "+label+" in it")
			return nil, false
		}
	}
	return agencyIDs, true
}

// requireRefEntityAgency is the departmental gate for ONE per-run reference
// (RF-4, the RBAC-fixes plan — the RB-32 half the in-handler sites
// were missing). It resolves the row DISPATCH will resolve, then applies the
// same entity-agency rule as the settings routes (RB-Q14 for unmembered rows).
//
// ⚠️ Resolving the same row is the whole correctness argument. The predicate
// below mirrors runref.lookupScoped exactly — the scope clause, the AG-Q1(b)
// agency clause against the RUN's agency snapshot, and the scope-exact-beats-
// global ordering. Checking a row the injector would not pick is a check in name
// only: with a global row and a departmental row sharing a key, the two
// resolutions can disagree, and the one that matters is the one whose value
// lands in the run.
//
// A name that resolves to no row passes: dispatch fails closed on it with the
// oracle-safe message (M2), and 403ing here on a miss would make the trigger
// route an existence probe for stored names. The denial for a REAL row uses one
// message for both the out-of-agency and the unmembered case, so the 403 does
// not say which the caller hit.
func (s *Server) requireRefEntityAgency(w http.ResponseWriter, r *http.Request, id auth.Identity,
	b runref.Binding, effScope string, runAgencies []string) bool {

	permitted, err := s.refEntityPermitted(r.Context(), id, b, effScope, runAgencies)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	if permitted {
		return true
	}
	s.denyEntityAgency(w, r, id, auth.PermManageEnvVars, auth.AllScopes,
		"cannot attach "+string(b.Kind)+" "+b.Name+": it belongs to a department you do not "+
			"have "+auth.PermManageEnvVars+" on")
	return false
}

// refEntityPermitted is requireRefEntityAgency's transport-free core, shared with
// the /references/validate pre-check so the dialog and the trigger cannot answer
// differently (RF-4/M6).
func (s *Server) refEntityPermitted(ctx context.Context, id auth.Identity,
	b runref.Binding, effScope string, runAgencies []string) (bool, error) {

	var joinTable, joinCol string
	switch b.Kind {
	case runref.KindKey:
		joinTable, joinCol = "ssh_credential_agencies", "credential_id"
	case runref.KindSecret:
		joinTable, joinCol = "secret_agencies", "secret_id"
	case runref.KindVar:
		joinTable, joinCol = "env_var_agencies", "env_var_id"
	default:
		// An unrecognised kind reaches no entity, so there is nothing to authorize —
		// and it is rejected as invalid_binding upstream before it ever gets here.
		return true, nil
	}
	// ⚠️ THE one predicate, called rather than copied (RA-17). This site used to carry
	// a hand-transcribed duplicate of runref.lookupScoped's SQL, with a comment
	// promising it "mirrors runref.lookupScoped exactly" — a promise no comment can
	// keep. Phase E would have broken it silently: the copy has no owner tier, so with
	// two departments' rows present this gate would authorize one row while dispatch
	// injected the other, and the check would be a check in name only.
	entityID, found, err := runref.LookupEntityID(ctx, s.db, b.Kind, b.Name, effScope, runAgencies)
	if errors.Is(err, runref.ErrAmbiguousReference) {
		// Ambiguous ⇒ dispatch will refuse this run anyway (RA-17 fails closed). Let
		// the attach through so the operator meets ONE clear failure — the dispatch
		// ambiguity message, which tells them what to do — rather than a 403 implying
		// they lack a permission they actually hold.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !found {
		// A name that resolves to no row passes: dispatch fails closed on it with the
		// oracle-safe message (M2), and 403ing on a miss would make the trigger route
		// an existence probe for stored names.
		return true, nil
	}

	permitted, deniedAgency, err := s.entityAgencyPermitted(ctx, id, auth.PermManageEnvVars,
		joinTable, joinCol, entityID)
	if err != nil {
		return false, err
	}
	if permitted {
		return true, nil
	}
	if deniedAgency == "" {
		// ⚠️ A GLOBAL entity is allowed here, unlike on the settings routes,
		// and the asymmetry is deliberate. RB-Q14 makes shared infrastructure
		// unrestricted-only for "writes and reveal" and says in the same breath that
		// "reads and RUN-TIME CONSUMPTION are unchanged" — and attaching a reference
		// is consumption: the value is injected into a run whose scope the actor
		// already holds, and dispatch would resolve that same shared row for that
		// same run if the JOB had declared it. Nothing new becomes reachable.
		//
		// Applying the write rule here instead would also have been a cliff rather
		// than a boundary: migration 670 backfills NO membership, so in the field
		// almost every secret, variable and key is unmembered, and a restricted
		// operator would lose per-run references entirely until an operator worked
		// through the whole catalogue.
		return true, nil
	}
	return false, nil
}

func (s *Server) denyEntityAgency(w http.ResponseWriter, r *http.Request, id auth.Identity, perm, agency, message string) {
	if s.auth != nil {
		s.auth.AuditDenied(r, id.Email, "insufficient_permission", perm,
			auditDetails(r, "agency-scoped denial on "+agency+": "+message))
	}
	httpx.Fail(w, http.StatusForbidden, "forbidden", message)
}

// ownerAgencyFor picks the OWNER for a newly created secret / variable / SSH key
// from the effective agency set requireCreationAgencies returned (RA-15, Phase E).
//
// Ownership is deliberately SINGLE-VALUED — that is what lets uniqueness stay a
// plain DB fact rather than a property of a many-to-many join (§2.6). So:
//
//	exactly one agency  ⇒ that agency owns the row (the departmental case, and —
//	                      when the one agency is Global — shared infrastructure,
//	                      a global administrator's deliberate statement)
//	several             ⇒ Global owns it, VISIBLE only to those agencies via
//	                      membership
//
// The last case is the interesting one. A creator who names several departments is
// saying "these departments share this", which is exactly what a Global-owned row
// with a narrowed member list means — whereas picking one of them as owner would
// invent an ownership claim they never made, and silently give that department's
// runs precedence over the others'. Membership still narrows who can reach it.
// (LR-54 retires the several case: one agency per secret, variable and key.)
//
// Never empty since migration 1220: "unowned" is "Global's", with an id.
func ownerAgencyFor(agencyIDs []string) string {
	if len(agencyIDs) == 1 {
		return agencyIDs[0]
	}
	return agencyid.Global
}
