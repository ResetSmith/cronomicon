package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runref"
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
			label+" belongs to no agency, so it is shared by every department; only an "+
				"unrestricted operator may change or reveal it")
		return false
	}
	s.denyEntityAgency(w, r, id, perm, deniedAgency,
		"you do not have "+perm+" on the agency that owns this "+label)
	return false
}

// entityAgencyPermitted is the transport-free core of requireEntityAgency, for
// call sites that are not a whole route — the per-run reference/credential
// checks inside runJob and the compose path (RF-4), where the denial response is
// shaped by the caller. deniedAgency is "" for the empty-membership (RB-Q14)
// case and the first owning agency otherwise; it is meaningful only when
// permitted is false.
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
		// The global-infrastructure case (RB-Q14). CanAgency("") already demands an
		// unrestricted grant, so this is one call rather than a special case.
		return id.CanAgency(perm, ""), "", nil
	}
	for _, a := range agencies {
		if id.CanAgency(perm, a) {
			return true, "", nil
		}
	}
	return false, agencies[0], nil
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

	// ⚠️ The predicate is CanAgency(perm, ""), not Unrestricted(). Unrestricted() is
	// permission-BLIND — it asks only whether some grant reaches every scope, and
	// UnionGrantScopes collapses to ["*"] if ANY grant does. A user who is an
	// unrestricted VIEWER and a Tax ADMIN reads as unrestricted, would be waved
	// through with no agencyIds, and would then be refused by requireEntityAgency on
	// the very next request — because the empty-membership branch demands an
	// unrestricted grant CARRYING THE PERMISSION, which their viewer grant does not.
	// That is precisely the lockout this rule exists to prevent, so the two must ask
	// the same question.
	if !id.CanAgency(perm, "") && len(agencyIDs) == 0 {
		// RA-9: inherit, when "their department" has exactly one answer. The inherited
		// id still goes through the existence + entitlement loop below, so inheritance
		// can never place a row somewhere an explicit request could not.
		if held := id.AgenciesFor(perm); len(held) == 1 {
			agencyIDs = held
		} else {
			msg := "your access is department-scoped: assign this " + label +
				" to at least one of your agencies (agencyIds) so your department keeps ownership of it"
			if len(held) > 1 {
				// Naming the departments makes this actionable — the actor holds the
				// permission on all of them, so this reveals nothing they cannot list.
				msg = "you hold " + perm + " on more than one department, so this " + label +
					" cannot inherit one: name the owning agency explicitly in agencyIds (" +
					strings.Join(held, ", ") + ")"
			}
			httpx.Fail(w, http.StatusUnprocessableEntity, "agency_required", msg)
			return nil, false
		}
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
		// ⚠️ The UNMEMBERED entity is allowed here, unlike on the settings routes,
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
//	exactly one agency  ⇒ that agency owns the row (the departmental case)
//	none                ⇒ unowned: shared infrastructure, an unrestricted admin's
//	                      deliberate statement, and every pre-Phase-E row
//	several             ⇒ unowned, but VISIBLE to all of them via membership
//
// The last case is the interesting one. A creator who names several departments is
// saying "these departments share this", which is exactly what an unowned row with
// multi-department membership means — whereas picking one of them as owner would
// invent an ownership claim they never made, and silently give that department's
// runs precedence over the others'. Membership still narrows who can reach it.
func ownerAgencyFor(agencyIDs []string) string {
	if len(agencyIDs) == 1 {
		return agencyIDs[0]
	}
	return ""
}
