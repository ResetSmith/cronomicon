package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"github.com/ResetSmith/cronomicon/internal/watchspec"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountJobCompose owns the in-app Job composition write surface (A11 / v20 Phase 3).
//
// An cronomicon-source Job is a DB-authored binding of a (Git) Script × Schedule(s) ×
// Scope × execution options — no Git round-trip. It enqueues runs through the same
// scheduler/executor seam as a Git job; only its origin differs. Git-source jobs are
// read-only here (mutations return 409 — they round-trip through the Git publish flow).
//
// Routes (all behind requireCompose — the FIRST server-side authz enforcement):
//
//	POST   /api/v1/jobs            create an cronomicon-source job
//	PUT    /api/v1/jobs/{jobId}    edit an cronomicon-source job (409 on git rows)
//	DELETE /api/v1/jobs/{jobId}    delete an cronomicon-source job (409 on git rows)
func (s *Server) mountJobCompose(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/jobs", s.requireCompose(http.HandlerFunc(s.createJob)))
	mux.Handle("PUT /api/v1/jobs/{jobId}", s.requireCompose(http.HandlerFunc(s.updateJob)))
	mux.Handle("DELETE /api/v1/jobs/{jobId}", s.requireCompose(http.HandlerFunc(s.deleteJob)))
}

// requireCompose gates the in-app authoring (A11 Compose) endpoints for the two
// surfaces whose objects CAN be agency-bound: jobs and workflows (AF-2,
// the af2-compose-permission plan). It was RequireRole("admin") from v20
// (decision Q-5) until AF-1 built the missing half — every job now states its
// agency, so "may this actor author HERE" is finally answerable and the verb can be
// data like the other six permissions.
//
// This gate answers only "may you compose SOMEWHERE" (CanAnywhere semantics, the
// same as every other requirePerm route). It is NEVER sufficient on its own: the
// per-object check happens in the handler, where the scope is known, via
// requireComposeScope below. A route that reaches a definition and does not call
// that is a hole.
//
// 🔴 RB-30 — THE OLD GATE WAS LOAD-BEARING FOR A DECISION MADE ELSEWHERE, AND
// THAT LOAD IS NOW CARRIED BY requireComposeScope + requireComposeAdmin.
//
// RB-26 requires an interactive caller to bind a scope before running an unscoped
// job. A SCHEDULED fire has no caller, so RB-Q11(c) leaves it unbound and
// general-pool — safe only because authoring a schedule required admin. Inline
// schedules are authored through THIS endpoint, so widening it is exactly the move
// RB-30 warned about. Two things keep the bypass unreachable:
//
//  1. An All-scoped (unscoped) job is admin-only — requireComposeScope routes the
//     empty scope through Identity.CanUnbound, which only an unrestricted grant
//     satisfies. A compose-granted non-admin cannot create one, edit one, or move a
//     job to or from All, so they can never attach a schedule to an unbound job.
//  2. The SHARED-object authoring surfaces (schedule-defs, calendars, reactions,
//     revisions/recycle-bin) stay on requireComposeAdmin — see its note.
//
// TestScheduleDefsAdminOnly_GuardsUnscopedSchedulingBypass still guards (2), and
// TestComposeGrantCannotAuthorAllScopedJobs guards (1). See
// the rbac-update plan RB-Q11 / RB-30.
func (s *Server) requireCompose(next http.Handler) http.Handler {
	return s.requirePerm(auth.PermCompose, func(p rolePermissions) bool { return p.Compose })(next)
}

// requireComposeAdmin keeps the ORIGINAL admin-only gate for the authoring surfaces
// AF-2 deliberately did not widen: schedule-defs, calendars, reactions, and the
// revisions/recycle-bin. Every one of them acts on an object with NO scope of its
// own — a reusable schedule template, a global working calendar, a reaction that
// attaches to a definition by name, a bin spanning every definition — so
// "agency-bound" has no meaning for them yet, and handing them to a departmental
// composer would be a cross-agency escalation, not a delegation: retiming another
// department's schedule, editing a shared calendar, attaching a reaction that
// TRIGGERS someone else's job, or restoring/purging anything from anyone's bin.
//
// Widening any of these is its own decision with its own agency model to design
// first. Until then this is the same RequireRole("admin") those routes always had,
// named so it reads as a choice rather than as the one that was forgotten.
func (s *Server) requireComposeAdmin(next http.Handler) http.Handler {
	return s.auth.RequireSession(s.auth.RequireCSRF(s.auth.RequireRole("admin", next)))
}

// requireComposeScope is the per-object half of the compose gate (the Q-C work the
// old requireCompose note promised). Call it in the handler once the object's scope
// is known; it reports whether the caller may proceed and, on refusal, has already
// written the 403 and the audit row naming the permission and the scope.
//
// The empty scope is NOT "no restriction" here — it is the All pool, and it routes
// through CanUnbound so only an unrestricted grant passes (RB-30 item 1, and the
// same rule RB-26 applies to running an unscoped job). Every other scope asks
// Can(compose, scope), which is per-grant: a FIN composer passes for a FIN scope
// and fails for a Tax one, rather than passing because they hold compose somewhere.
func (s *Server) requireComposeScope(w http.ResponseWriter, r *http.Request, id auth.Identity, scope string) bool {
	if strings.TrimSpace(scope) == "" {
		// 🔴 AllScopes, NOT "". Can(perm, "") deliberately preserves the Q-F7 rule
		// that an empty scope is permitted for EVERYONE (identity.go says so), so
		// asking it here would hand every departmental composer the All pool — the
		// exact bypass this branch exists to close, and one this passed until
		// TestComposeGrantCannotAuthorAllScopedJobs caught it. Can(perm, AllScopes)
		// is the strict reading: it demands a grant that reaches everywhere, which
		// is identical to CanUnbound and is what "may you author in All" means.
		// requireCan also renders this target as "*" in the audit row and message.
		return s.requireCan(w, r, id, auth.PermCompose, auth.AllScopes)
	}
	return s.requireCan(w, r, id, auth.PermCompose, scope)
}

var composeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type composeScheduleEntry struct {
	Name string            `json:"name"`
	Cron string            `json:"cron"`
	Env  map[string]string `json:"env,omitempty"`
	// StartAt/EndAt are the optional activation window (AW-5): RFC3339 bounds on
	// when this entry's cron may fire. For a ref expansion they are copied from
	// the referenced first-class schedule, keeping the runtime row self-contained.
	StartAt *string `json:"startAt,omitempty"`
	EndAt   *string `json:"endAt,omitempty"`
	// Interval is the anchored-interval mode ("7d", "36h"), mutually exclusive
	// with Cron and requiring StartAt as its phase anchor (Phase 2). A ref
	// expansion copies it from the referenced schedule like cron/env/window.
	Interval *string `json:"interval,omitempty"`
	// SkipCalendars / OnlyCalendars are the working-calendar bindings (CAL-5).
	// skip suppresses a fire landing on any listed calendar's day; only suppresses
	// a fire that does NOT land on one. A ref expansion copies them down with
	// cron/env/window so the runtime row stays self-contained.
	SkipCalendars []string `json:"skipCalendars,omitempty"`
	OnlyCalendars []string `json:"onlyCalendars,omitempty"`
	// SourceRef is the first-class schedule this entry was expanded from (D1c —
	// schedule-builder.md); empty for an inline entry. Internal-only (not part of
	// the request contract); resolveComposeSchedules sets it for ref expansions so
	// the definition_schedules.source_ref column is recorded for precise propagation.
	SourceRef string `json:"-"`
}

// jobComposeInput is the request body for create/update (the JobComposeInput schema).
type jobComposeInput struct {
	Name      string `json:"name"`
	ScriptRef string `json:"scriptRef"`
	// AF-1 — a POINTER because absent and explicitly-global must be told apart.
	// Empty string is a real, deliberate choice ("All": no scope, visible to
	// everyone, the storage convention NULL); an ABSENT field is an author who
	// never decided, which is the accident this band exists to stop. Anything
	// that omits the key gets a 422 naming the All option rather than silently
	// creating an agencyless job. Read through scopeOf() below, never directly.
	Scope        *string                `json:"scope"`
	TargetHost   string                 `json:"targetHost"`
	Env          map[string]string      `json:"env,omitempty"` // job-level env (JC10); merged beneath schedule + per-run override env at run time
	Executor     string                 `json:"executor"`      // optional override; empty ⇒ script default
	ScheduleRefs []string               `json:"scheduleRefs"`
	Schedules    []composeScheduleEntry `json:"schedules"` // inline schedules
	Enabled      *bool                  `json:"enabled"`
	Requestable  bool                   `json:"requestable"`
	// SL — soft deadlines. Warn only; timeout_seconds remains the only thing
	// that kills a run.
	WarnAfterSeconds int    `json:"warnAfterSeconds"`
	MustFinishBy     string `json:"mustFinishBy"` // 'HH:MM' in the app zone
	// ET-D — file-arrival watches. Declaring one IS this job's opt-in for being
	// started by a file; `requestable` gates the token API, a different surface.
	Watch             []watchspec.Watch `json:"watch"`
	TimeoutSeconds    int               `json:"timeoutSeconds"`
	Retries           int               `json:"retries"`
	BackoffSeconds    int               `json:"backoffSeconds"`  // WB-R1 job-level default
	ContinueOnError   bool              `json:"continueOnError"` // WB-R1 job-level default
	ConcurrencyPolicy string            `json:"concurrencyPolicy"`
	ConcurrencyKey    string            `json:"concurrencyKey"`
	// JR-Q5 — per-job run-input enforcement: "warn" (default) | "block". Anything
	// unrecognized normalizes to "warn" (gitlab.NormalizePromptEnforcement).
	PromptEnforcement string   `json:"promptEnforcement,omitempty"`
	Description       string   `json:"description"`
	Tags              []string `json:"tags"`
	// RT-2 — the DECLARED runner pin: runs of this job may be claimed only by a
	// runner carrying this tag. Sibling of Executor, and git's equivalent field is
	// spec.runner_tag. The operator's live override is NOT here — it is a separate
	// column with its own PUT (RT-Q7), because this upsert is a full replace and
	// would otherwise clobber an override every time the job is edited.
	RunnerTag string              `json:"runnerTag,omitempty"`
	Prompts   []gitlab.PromptSpec `json:"prompts,omitempty"`  // UDV1 — declared prompt variables surfaced in the Run dialog
	Requires  []string            `json:"requires,omitempty"` // §5/RX.13 — requirement tokens (e.g. vault) that claim-gate this job to capable runners
	// CA Phase B — declarative "connect as" identity (username + stored-credential
	// LABEL, names only; CA-Q2). Setting or changing the credential needs
	// ManageEnvVars (CA-Q1 — binding key material to a job is a grant, the same
	// rule as declared reference bindings); clearing it does not.
	SSHUser       string `json:"sshUser,omitempty"`
	SSHCredential string `json:"sshCredential,omitempty"`
	// RA-12 (Phase B) — the bare NAME of a Secrets row supplying this job's Ansible
	// become password. A name, never a value; the password stays in the Secrets
	// catalogue and is resolved at dispatch, then delivered to the runner as a 0600
	// file. Setting it is a grant over stored secret material, so it carries the
	// same ManageEnvVars rule as sshCredential (CA-Q1).
	BecomePasswordSecret string `json:"becomePasswordSecret,omitempty"`
	// RP-14 — env-var NAMES (never values, D1) the runner agent resolves from ITS
	// OWN environment into a local-toolchain child process (RX.9). Git YAML has
	// carried this as spec.env_passthrough since protocol v3; without it here an
	// in-app-composed ansible job could never reach a runner-local variable.
	// Meaningless for ssh-family runs — their remote env is built entirely from
	// the manifest, so there is no server-side environment to pass through.
	EnvPassthrough []string `json:"envPassthrough,omitempty"`
}

// resolvedScript carries the denormalized executable fields (Decision 7) copied
// from the referenced Git Script onto the cronomicon job row.
type composeScript struct {
	runType, command, script, scriptPath, executor, contentHash string
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in jobComposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !composeNameRe.MatchString(in.Name) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid job name (want "+composeNameRe.String()+")")
		return
	}
	// R2-5 — per-agency uniqueness: the name must be free within every agency
	// the scope maps to (the All pool overlaps everything). A binned sibling
	// still holds its name inside its own pools. Refusals are GENERIC except in
	// the one case the caller can already see: an exact same-scope binned row,
	// where the recycle-bin hint is actionable rather than an oracle.
	if in.Scope == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "scope is required (use \"\" for All)")
		return
	}
	conflict, cerr := execspec.NamePoolConflict(r.Context(), s.db, "jobs", "cronomicon", in.Name, in.scopeOf(), "")
	if cerr != nil {
		httpx.Fail500(w, s.log, "db_error", cerr)
		return
	}
	if conflict {
		msg := execspec.NamePoolRefusal
		var sameScopeBinned int
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name=? AND COALESCE(scope,'')=? AND deleted_at IS NOT NULL`,
			in.Name, in.scopeOf()).Scan(&sameScopeBinned)
		if sameScopeBinned > 0 {
			msg = "a job with this name is in the recycle bin — restore or purge it to reuse the name"
		}
		httpx.Fail(w, http.StatusConflict, "conflict", msg)
		return
	}
	s.writeComposedJob(w, r, in, id.Email, true, db.NewID())
}

func (s *Server) updateJob(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	jobID := r.PathValue("jobId")
	existing := s.fetchJobByID(r, jobID)
	if existing == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if existing.Source != "cronomicon" {
		httpx.Fail(w, http.StatusConflict, "conflict", "only cronomicon-source jobs are editable in-app; git jobs round-trip through the publish flow")
		return
	}
	if existing.DeletedAt != nil {
		httpx.Fail(w, http.StatusConflict, "conflict", "this job is in the recycle bin — restore it before editing")
		return
	}
	// AF-2 — the scope the job is LEAVING. writeComposedJob checks the one it is
	// arriving at; both must pass. Only the incoming check would let a FIN
	// composer take over an All or Tax job by rewriting its scope to FIN, and only
	// the existing check would let them push a FIN job out into All.
	if !s.requireComposeScope(w, r, id, deref(existing.Scope)) {
		return
	}
	var in jobComposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = existing.Name // name is immutable on edit (it's the identity)
	// R2-5 — a scope EDIT can move the job into an agency where its name is
	// taken; checked against every sibling but itself.
	if in.Scope != nil {
		conflict, cerr := execspec.NamePoolConflict(r.Context(), s.db, "jobs", "cronomicon", in.Name, in.scopeOf(), existing.UID)
		if cerr != nil {
			httpx.Fail500(w, s.log, "db_error", cerr)
			return
		}
		if conflict {
			httpx.Fail(w, http.StatusConflict, "conflict", execspec.NamePoolRefusal)
			return
		}
	}
	s.writeComposedJob(w, r, in, id.Email, false, existing.UID)
}

func (s *Server) deleteJob(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	jobID := r.PathValue("jobId")
	existing := s.fetchJobByID(r, jobID)
	if existing == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	if existing.Source != "cronomicon" {
		httpx.Fail(w, http.StatusConflict, "conflict", "only cronomicon-source jobs are deletable in-app")
		return
	}
	// AF-2 — deleting is authoring: same per-scope authority as editing.
	if !s.requireComposeScope(w, r, id, deref(existing.Scope)) {
		return
	}
	// RX-24 — refuse while reactions WATCH this job, unless forced. Its own code,
	// not "conflict": the git-source refusal above is also a 409, and only this
	// one is clearable with ?force=true.
	if msg, err := s.reactionDeleteGuard(r, "job", existing.Source, existing.Name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	} else if msg != "" {
		httpx.Fail(w, http.StatusConflict, ErrCodeReactionsWatching, msg)
		return
	}
	// RH — SOFT delete. The row is stamped, not removed, so restoring it is one
	// UPDATE that loses nothing: its definition_schedules, paused_jobs, tags,
	// reactions and entity code all stay exactly as they were. The cost is that
	// the AFTER DELETE triggers do not fire (an UPDATE never fires them), so the
	// bindings survive and every execution path must filter deleted_at IS NULL —
	// which is where the scheduler, the pending promoter and the reactor now do.
	//
	// entitycode.MarkDeleted is deliberately NOT called here. A binned definition
	// has not gone anywhere and must keep its log folder; the code retires at
	// purge time (purgeDefinition), which is also where the real DELETE fires the
	// triggers.
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(r.Context(),
		`UPDATE jobs SET deleted_at=?, deleted_by=?
		  WHERE (uid = ? OR (? = '' AND source='cronomicon' AND name = ?)) AND deleted_at IS NULL`,
		now, id.Email, existing.UID, existing.UID, existing.Name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "this job is already in the recycle bin")
		return
	}
	// The tombstone records what the definition looked like when it was binned,
	// so the history reads as a complete story even after a purge.
	if err := snapshotRevision(r.Context(), tx, revKindJob, "cronomicon", existing.Name, existing.UID, id.Email, revActionDeleted,
		map[string]any{"deletedAt": now, "deletedBy": id.Email}); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditDefinitionDelete(r.Context(), id.Email, "job", "Jobs", existing.Name, existing.Scope)
	s.forceScheduleReload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// scopeOf reads the validated scope. Safe only AFTER writeComposedJob's nil check
// (AF-1): every caller runs downstream of it, and a nil here would mean the guard
// was bypassed rather than that the job is global — so it collapses to "" (All)
// instead of panicking, matching what the storage layer does with an empty scope.
func (in jobComposeInput) scopeOf() string {
	if in.Scope == nil {
		return ""
	}
	return *in.Scope
}

// writeComposedJob validates the binding, denormalizes the referenced script, and
// upserts the cronomicon job + its definition_schedules in one transaction.
func (s *Server) writeComposedJob(w http.ResponseWriter, r *http.Request, in jobComposeInput, actor string, isCreate bool, uid string) {
	if d := strings.TrimSpace(in.MustFinishBy); d != "" && !cronutil.ValidDeadline(d) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"mustFinishBy must be a wall-clock time as HH:MM (24-hour), in the application timezone")
		return
	}
	if in.WarnAfterSeconds < 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "warnAfterSeconds must not be negative")
		return
	}
	// ET-D: unlike the Git path, this caller is a human who can be told — so a
	// bad watch is a 422 rather than an advisory warn.
	watches := watchspec.Normalize(in.Watch)
	if verrs := watchspec.Validate(watches); len(verrs) > 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", strings.Join(verrs, "; "))
		return
	}
	if strings.TrimSpace(in.ScriptRef) == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "scriptRef is required")
		return
	}
	// AF-1 — the agency assignment must be a CHOICE. A job's scope decides which
	// agencies can see it (scopeWhereFragment filters restricted callers to their
	// granted scopes, and a NULL scope passes for everyone), so a job that is
	// global merely because nobody filled the field is invisible policy. Both
	// answers are accepted — a named scope, or "" for a deliberate All — and only
	// SILENCE is refused. The message names the escape so this never reads as
	// "global jobs are banned".
	if in.Scope == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			`scope is required: name a scope, or send "" to place this job in All (global — visible to every agency)`)
		return
	}
	// AF-2 — may this actor author INTO the scope being written? The route gate
	// only established that they compose somewhere. Reached by both create and
	// edit, so it covers the INCOMING scope on each; updateJob separately checks
	// the scope the job is leaving, because passing only this one would let a
	// departmental composer capture an out-of-reach job by rewriting its scope.
	actorID, _ := auth.IdentityFrom(r.Context())
	if !s.requireComposeScope(w, r, actorID, in.scopeOf()) {
		return
	}
	// Resolve + denormalize the referenced Git Script (Decision 7).
	var sc composeScript
	var command, script, scriptPath, executor sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT run_type, command, script, script_path, executor, content_hash FROM scripts WHERE name=?`, in.ScriptRef).
		Scan(&sc.runType, &command, &script, &scriptPath, &executor, &sc.contentHash)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "scriptRef does not resolve to any script: "+in.ScriptRef)
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	sc.command, sc.script, sc.scriptPath = command.String, script.String, scriptPath.String
	sc.executor = executor.String
	if in.Executor != "" { // per-job executor override
		if in.Executor != "ssh" && in.Executor != "runner" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid executor (want ssh|runner)")
			return
		}
		sc.executor = in.Executor
	}

	// TG-4 — an ansible job's target host is passed verbatim to `--limit`, and
	// AnsibleLimit REFUSES (drops) any name carrying a pattern metacharacter. A
	// dropped pin means no --limit at all, i.e. the run silently widens to the full
	// inventory. Reject it here, at authoring time, where the message can be
	// actionable — mirroring the identical trigger-boundary check on per-run host
	// subsets (execution_mount.go). The manifest 409 backs this up for git-synced
	// and pre-existing rows, which never pass through this handler.
	if sc.runType == "ansible" && in.TargetHost != "" && !execspec.ValidName(in.TargetHost) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"target host "+in.TargetHost+" contains an ansible pattern character; use a plain host name (operators use ansibleLimit for raw --limit patterns at trigger time)")
		return
	}

	// RP-14 — env_passthrough: NAMES only, POSIX env-name charset, and never an
	// CRONOMICON_* name (those are Cronomicon's own injected references, not the
	// runner's environment — the ValidateOperatorEnv rule, same rationale).
	// Local-toolchain run types only: an ssh-family run has no server-side
	// environment to pass through, so accepting it there would be a silent no-op.
	{
		clean := make([]string, 0, len(in.EnvPassthrough))
		for _, n := range in.EnvPassthrough {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if !execspec.ValidEnvName(n) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"env passthrough name "+n+" is not a valid environment-variable name (letters, digits and '_', not starting with a digit)")
				return
			}
			if envref.HasCronomiconPrefix(n) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"env passthrough name "+n+" is reserved: CRONOMICON_* names are references Cronomicon injects, not variables read from the runner's environment")
				return
			}
			clean = append(clean, n)
		}
		if len(clean) > 0 && execspec.SupportedRunType(sc.runType) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"envPassthrough applies only to local-toolchain run types (ansible, terraform); a "+sc.runType+" run's environment is built entirely from its manifest")
			return
		}
		in.EnvPassthrough = clean
	}

	// CA-9 — the declarative "connect as" identity, validated at authoring time
	// with the same rules as the per-run override (CA-2): an identity-capable
	// run type (RP-6 — ssh-family or ansible), conservative username charset,
	// and the credential label must name a stored credential. Unlike the
	// advisory Git-sync path (a sync must not wedge a repo), the Composer can be
	// actionable, so all three are hard 422s.
	in.SSHUser = strings.TrimSpace(in.SSHUser)
	in.SSHCredential = strings.TrimSpace(in.SSHCredential)
	if in.SSHUser != "" || in.SSHCredential != "" {
		if !execspec.IdentityCapableRunType(sc.runType) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"sshUser/sshCredential cannot be applied to "+sc.runType+" jobs; terraform authenticates through its providers rather than SSH")
			return
		}
		if in.SSHUser != "" && !execspec.ValidSSHUser(in.SSHUser) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"sshUser must be a plain username (letters, digits, '.', '-', '_'; starting with a letter or '_'; at most 32 chars)")
			return
		}
	}
	if in.SSHCredential != "" {
		credID, found, err := sshkeys.IDByLabel(r.Context(), s.db, in.SSHCredential)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		if !found {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"no SSH credential with label "+in.SSHCredential)
			return
		}
		// CA-Q1 — binding a stored key to a job is a grant over key material.
		// Gated only when the value is SET or CHANGED, so a compose-only editor
		// resending an unchanged job (the JC1 full-replace PUT) is not locked
		// out of unrelated edits; clearing the credential shrinks the grant and
		// needs nothing extra.
		var current sql.NullString
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT ssh_credential FROM jobs WHERE uid=?`, uid).Scan(&current)
		if current.String != in.SSHCredential {
			id, _ := auth.IdentityFrom(r.Context())
			// RF-4: CanAnywhere via the RB-1 seam (requirePerm's exact semantics).
			if !id.CanAnywhere(auth.PermManageEnvVars) {
				if s.auth != nil {
					s.auth.AuditDenied(r, id.Email, "insufficient_permission", "manageEnvVars",
						auditDetails(r, "binding an SSH credential to a job needs the Manage Env Vars permission"))
				}
				httpx.Fail(w, http.StatusForbidden, "forbidden",
					"binding an SSH credential to a job requires the Manage Env Vars permission")
				return
			}
			// RB-32 (RF-4): departmental half — the key's owning agency (RB-Q14 for
			// unmembered keys). Compose is admin-only today, but a scope-restricted
			// admin is a supported control and this is the check that keeps them
			// inside their department's key material.
			if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars,
				"ssh_credential_agencies", "credential_id", credID, "SSH key") {
				return
			}
		}
	}

	// RA-21 — the become password is a grant over secret material, exactly like
	// sshCredential above (CA-Q1): naming a Secret here has the runner write that
	// value to a file and hand it to `ansible-playbook --become-password-file`.
	//
	// 🔴 v0.57.3 shipped this field with NO check of any kind — no verb, no agency,
	// no run type, no existence — while its OpenAPI text claimed it "carries the
	// same ManageEnvVars rule as sshCredential". A documented-but-absent gate is
	// worse than a missing one: an auditor reading the contract concludes the
	// surface is protected. This is that gate.
	//
	// Git sync is deliberately NOT gated the same way (RA-Q19): sync warns and
	// stores, because failing a sync would take every other job in the repo down
	// with it. Repo write already implies job authorship — that is the sync trust
	// model, not a hole in this check.
	in.BecomePasswordSecret = strings.TrimSpace(in.BecomePasswordSecret)
	if in.BecomePasswordSecret != "" {
		// Run type first: the flag reaches ansible-playbook only, and a become
		// password on a bash job is silently inert — the shape of misconfiguration
		// that looks like it worked.
		if sc.runType != "ansible" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"becomePasswordSecret applies only to ansible jobs, not "+sc.runType+" jobs")
			return
		}
		// Resolve through the SHARED predicate (§12.3's lesson): the row authorized
		// here must be the row dispatch will inject. A hand-rolled "SELECT id FROM
		// secrets WHERE key=?" would authorize one department's row while the
		// injector delivered another's the moment two departments own the key.
		//
		// The run's agency snapshot is scope-derived, so the job's own scope gives
		// the same candidate set a run of this job would see.
		scopeAgencies, aerr := execspec.ScopeAgencies(r.Context(), s.db, in.scopeOf())
		if aerr != nil {
			httpx.Fail500(w, s.log, "db_error", aerr)
			return
		}
		secretID, found, lerr := runref.LookupEntityID(r.Context(), s.db,
			runref.KindSecret, in.BecomePasswordSecret, in.scopeOf(), scopeAgencies)
		switch {
		case errors.Is(lerr, runref.ErrAmbiguousReference):
			// RA-17's fail-closed arm, surfaced at authoring time where it is
			// actionable, rather than as a 409 on somebody's 3am run.
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"more than one Secret named "+in.BecomePasswordSecret+
					" is owned by the departments of this job's scope; a run could not choose between them")
			return
		case lerr != nil:
			httpx.Fail500(w, s.log, "db_error", lerr)
			return
		case !found:
			// Also catches the unscoped-job-with-a-department-owned-secret case: an
			// unbound run carries an empty agency snapshot, so an owned row resolves
			// for nobody. Better refused here than queued forever (RA-24).
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"no Secret named "+in.BecomePasswordSecret+" is available to this job's scope")
			return
		}
		// Gated only when SET or CHANGED, matching sshCredential: a compose-only
		// editor resending an unchanged job (the JC1 full-replace PUT) must not be
		// locked out of unrelated edits, and clearing the field shrinks the grant.
		var currentBecome sql.NullString
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT become_password_secret FROM jobs WHERE uid=?`, uid).Scan(&currentBecome)
		if currentBecome.String != in.BecomePasswordSecret {
			id, _ := auth.IdentityFrom(r.Context())
			// RF-4: CanAnywhere via the RB-1 seam (requirePerm's exact semantics).
			if !id.CanAnywhere(auth.PermManageEnvVars) {
				if s.auth != nil {
					s.auth.AuditDenied(r, id.Email, "insufficient_permission", "manageEnvVars",
						auditDetails(r, "binding a become password to a job needs the Manage Env Vars permission"))
				}
				httpx.Fail(w, http.StatusForbidden, "forbidden",
					"binding a become password to a job requires the Manage Env Vars permission")
				return
			}
			// RB-32 (RF-4): the departmental half, against the OWNING agency of the
			// row that actually resolved. RB-Q14 governs an unmembered secret —
			// unrestricted-only, since "belongs to no department" carries no authority.
			if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars,
				"secret_agencies", "secret_id", secretID, "secret") {
				return
			}
		}
	}

	// Resolve schedule entries: inline + referenced first-class schedules (A10a).
	entries, verr := s.resolveComposeSchedules(r.Context(), in)
	if verr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
		return
	}

	// Reserved-namespace guard (W4, N-D1): operator-authored env may not define any
	// CRONOMICON_* key — neither the job-level env nor any (inline or reusable)
	// schedule env. Absolute, no carve-outs.
	if err := envref.ValidateOperatorEnv(in.Env); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	for _, e := range entries {
		if err := envref.ValidateOperatorEnv(e.Env); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
	}

	enabled := 1
	if in.Enabled != nil && !*in.Enabled {
		enabled = 0
	}
	// QP: an unknown policy from the compose API is a 422 rather than a silent
	// coercion — unlike the Git path, this caller is a human who can be told.
	// Empty means "unset", which is Allow.
	concPolicy := strings.TrimSpace(in.ConcurrencyPolicy)
	if concPolicy == "" {
		concPolicy = cronutil.PolicyAllow
	}
	if !cronutil.ValidPolicy(concPolicy) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"concurrencyPolicy must be one of "+strings.Join(cronutil.ConcurrencyPolicies, ", "))
		return
	}
	legacyMirror := ""
	if len(entries) > 0 {
		legacyMirror = entries[0].Cron
	}
	tagsJSON, _ := json.Marshal(in.Tags)
	var envJobJSON any // job-level env (JC10); NULL when empty, mirroring the inline-schedule env handling below
	if len(in.Env) > 0 {
		b, _ := json.Marshal(in.Env)
		envJobJSON = string(b)
	}
	requestable := 0
	if in.Requestable {
		requestable = 1
	}
	continueOnErr := 0
	if in.ContinueOnError {
		continueOnErr = 1
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()

	// Upsert the cronomicon job row (source='cronomicon'); denormalized executable fields
	// come from the script so the scheduler/execspec/serializers read jobs.* unchanged.
	// prompts_json (UDV1) shares gitlab.MarshalPrompts with the git sync path so the
	// serialization is byte-identical regardless of source; always sent (even '[]') so
	// the full-replace PUT preserves it under the JC1 resend rather than stripping it.
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO jobs(name, source, run_type, description, scope, target_host, schedule, tags,
		                 enabled, timeout_seconds, retries, backoff_seconds, continue_on_error, requestable,
		                 warn_after_seconds, must_finish_by, watch_json,
		                 concurrency_policy, concurrency_key,
		                 command, script, script_path, executor, script_ref, content_hash,
		                 created_by, created_at, last_modified_by, last_modified_at, env_json, prompts_json,
		                 requires_json, prompt_enforcement, ssh_user, ssh_credential, env_passthrough,
		                 become_password_secret, runner_tag, uid)
		VALUES(?, 'cronomicon', ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(uid) DO UPDATE SET
			run_type=excluded.run_type, description=excluded.description, scope=excluded.scope,
			target_host=excluded.target_host, schedule=excluded.schedule, tags=excluded.tags,
			enabled=excluded.enabled, timeout_seconds=excluded.timeout_seconds, retries=excluded.retries,
			backoff_seconds=excluded.backoff_seconds, continue_on_error=excluded.continue_on_error,
			requestable=excluded.requestable,
			warn_after_seconds=excluded.warn_after_seconds, must_finish_by=excluded.must_finish_by,
			watch_json=excluded.watch_json,
			concurrency_policy=excluded.concurrency_policy,
			concurrency_key=excluded.concurrency_key, env_json=excluded.env_json, prompts_json=excluded.prompts_json,
			command=excluded.command, script=excluded.script, script_path=excluded.script_path,
			executor=excluded.executor, script_ref=excluded.script_ref, content_hash=excluded.content_hash,
			last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at,
			requires_json=excluded.requires_json, prompt_enforcement=excluded.prompt_enforcement,
			ssh_user=excluded.ssh_user, ssh_credential=excluded.ssh_credential,
			become_password_secret=excluded.become_password_secret,
			env_passthrough=excluded.env_passthrough,
			runner_tag=excluded.runner_tag`,
		in.Name, sc.runType, nullStrIf(in.Description), nullStrIf(in.scopeOf()), nullStrIf(in.TargetHost),
		nullStrIf(legacyMirror), string(tagsJSON), enabled, in.TimeoutSeconds, in.Retries, in.BackoffSeconds, continueOnErr, requestable,
		nullIfZeroInt(in.WarnAfterSeconds), nullStrIf(strings.TrimSpace(in.MustFinishBy)), watchspec.Marshal(watches),
		concPolicy, nullStrIf(in.ConcurrencyKey),
		nullStrIf(sc.command), nullStrIf(sc.script), nullStrIf(sc.scriptPath), nullStrIf(sc.executor),
		in.ScriptRef, sc.contentHash, actor, now, actor, now, envJobJSON, gitlab.MarshalPrompts(in.Prompts),
		gitlab.MarshalRequires(in.Requires), gitlab.NormalizePromptEnforcement(in.PromptEnforcement),
		nullStrIf(in.SSHUser), nullStrIf(in.SSHCredential), gitlab.MarshalEnvPassthrough(in.EnvPassthrough),
		nullStrIf(in.BecomePasswordSecret),
		// RT-2 — the declared pin. Since v1.3.5 (mig. 1090) it is the only
		// job-level pin there is: the Composer and the YAML spec are the two
		// places a durable pin is authored, and a per-run pin at trigger time is
		// the only thing that outranks it.
		nullStrIf(gitlab.NormalizeRunnerTag(in.RunnerTag)),
		// R2-5 — the identity is the conflict target now: a create inserts a
		// fresh uid, an edit collides on the existing one and lands in the DO
		// UPDATE arm. (source, name) stopped being unique for cronomicon rows, so
		// it can no longer be what an edit converges on.
		uid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// LU-6: allocate the log-folder code inside this transaction, so a code and
	// the row it names commit or roll back together. Deliberately NOT next to the
	// changelog block below — that runs post-commit on s.db and would leave a
	// committed job with no code if it failed.
	if _, err := entitycode.Allocate(r.Context(), tx, entitycode.KindJob, "cronomicon", in.Name, uid); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// RH — the revision snapshot commits with the write it describes. Placed here
	// beside entitycode.Allocate for the same reason that one is: the changelog
	// block below runs POST-commit on s.db, and a snapshot written there could
	// fail and leave a committed definition with no record of what it replaced.
	action := revActionUpdated
	if isCreate {
		action = revActionCreated
	}
	if err := snapshotRevision(r.Context(), tx, revKindJob, "cronomicon", in.Name, uid, actor, action, in); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// Replace the cronomicon-source schedule bindings (source-scoped, like sync's git path).
	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM definition_schedules WHERE owner_kind='job' AND owner_uid=?`, uid); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for i, e := range entries {
		var envJSON any
		if len(e.Env) > 0 {
			b, _ := json.Marshal(e.Env)
			envJSON = string(b)
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, env, position, source_ref, start_at, end_at, interval, skip_calendars, only_calendars,
				owner_uid, schedule_uid)
			VALUES('cronomicon','job',?,?,?,?,?,?,?,?,?,?,?,
				?,
				(SELECT s.uid FROM schedules s WHERE s.name = ?
				   AND (SELECT COUNT(*) FROM schedules s2 WHERE s2.name = s.name) = 1))`,
			in.Name, e.Name, e.Cron, envJSON, i, nullStrIf(e.SourceRef),
			windowArg(e.StartAt), windowArg(e.EndAt), windowArg(e.Interval),
			nullStrIf(calendar.MarshalNames(e.SkipCalendars)), nullStrIf(calendar.MarshalNames(e.OnlyCalendars)),
			uid, nullStrIf(e.SourceRef)); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	auditAction := "Created"
	if !isCreate {
		auditAction = "Updated"
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Jobs", auditAction, in.Name, "Cronomicon job "+strings.ToLower(auditAction))
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Jobs", Action: auditAction, Target: in.Name,
	})
	s.forceScheduleReload(r.Context())

	jr := s.fetchJobByName(r, in.Name, "cronomicon")
	status := http.StatusOK
	if isCreate {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, jr)
}

// resolveComposeSchedules merges inline schedules + referenced first-class
// schedules into a deduped entry list, validating crons and ref existence.
func (s *Server) resolveComposeSchedules(ctx context.Context, in jobComposeInput) ([]composeScheduleEntry, string) {
	seen := map[string]bool{}
	out := []composeScheduleEntry{}
	for _, e := range in.Schedules {
		name := strings.TrimSpace(e.Name)
		if !composeNameRe.MatchString(name) {
			return nil, "invalid schedule entry name: " + e.Name
		}
		if seen[strings.ToLower(name)] {
			return nil, "duplicate schedule name: " + name
		}
		seen[strings.ToLower(name)] = true
		startAt, endAt, werr := parseWindowPair(e.StartAt, e.EndAt)
		if werr != nil {
			return nil, "schedule " + name + ": " + werr.Error()
		}
		// One mode check for cron / interval / once (Phase 2), shared with the
		// schedule-def API and the Git YAML path.
		interval := strings.TrimSpace(deref(e.Interval))
		spec := cronutil.Spec{
			Cron: strings.TrimSpace(e.Cron), Interval: interval,
			Window: cronutil.NewWindow(windowBound(nullStringOf(startAt)), windowBound(nullStringOf(endAt))),
		}
		if serr := spec.Validate(); serr != nil {
			return nil, "schedule " + name + ": " + serr.Error()
		}
		skipCals, onlyCals, cerr := s.validateCalendarBinding(ctx, e.SkipCalendars, e.OnlyCalendars)
		if cerr != "" {
			return nil, "schedule " + name + ": " + cerr
		}
		out = append(out, composeScheduleEntry{
			Name: name, Cron: strings.TrimSpace(e.Cron), Env: e.Env,
			StartAt: startAt, EndAt: endAt, Interval: nullableOf(interval),
			SkipCalendars: skipCals, OnlyCalendars: onlyCals,
		})
	}
	for _, ref := range in.ScheduleRefs {
		var cron string
		var envJSON, refStart, refEnd, refInterval, refSkip, refOnly sql.NullString
		err := s.db.QueryRowContext(ctx,
			`SELECT cron, env, start_at, end_at, interval, skip_calendars, only_calendars
			   FROM schedules WHERE name=? ORDER BY source LIMIT 1`, ref).
			Scan(&cron, &envJSON, &refStart, &refEnd, &refInterval, &refSkip, &refOnly)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "scheduleRef does not resolve to any schedule: " + ref
		}
		if err != nil {
			return nil, "schedule lookup failed: " + err.Error()
		}
		if seen[strings.ToLower(ref)] {
			continue // inline entry of the same name wins
		}
		seen[strings.ToLower(ref)] = true
		var env map[string]string
		if envJSON.Valid && envJSON.String != "" {
			_ = json.Unmarshal([]byte(envJSON.String), &env)
		}
		// SourceRef links this entry to the first-class schedule (D1c) for precise
		// schedule-edit propagation; the activation window is copied down with
		// cron/env (AW-5) so the scheduler never joins back to `schedules`.
		refStartAt, refEndAt := windowStrings(refStart, refEnd)
		// The reusable-policy story (§2.5): a schedule named
		// "weeknights-non-holiday" carries its calendar binding, and binding the ref
		// to forty jobs gives all forty the policy from one place.
		//
		// Re-validated rather than trusted: the catalog row was validated when it
		// was authored, but a calendar can be force-deleted afterwards (which
		// deliberately leaves the name dangling), and expanding that stale binding
		// onto a new job would mint an entry that can never fire through a path
		// that never saw the refusal.
		refSkipCals, refOnlyCals, cerr := s.validateCalendarBinding(ctx,
			calendar.ParseNames(refSkip.String), calendar.ParseNames(refOnly.String))
		if cerr != "" {
			return nil, "scheduleRef " + ref + ": " + cerr
		}
		out = append(out, composeScheduleEntry{
			Name: ref, Cron: cron, Env: env, SourceRef: ref,
			StartAt: refStartAt, EndAt: refEndAt, Interval: nullableOf(refInterval.String),
			SkipCalendars: refSkipCals, OnlyCalendars: refOnlyCals,
		})
	}
	return out, ""
}

// fetchJobByName fetches a job by (name, source) — the disjoint dual-source key.
func (s *Server) fetchJobByName(r *http.Request, name, source string) *jobRow {
	var rowid int64
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT rowid FROM jobs WHERE source=? AND name=?`, source, name).Scan(&rowid); err != nil {
		return nil
	}
	return s.fetchJobByID(r, strconv.FormatInt(rowid, 10))
}

// auditDefinitionDelete writes the change_log + activity pair for an in-app
// definition delete (LU-13).
//
// It exists because both delete handlers used to emit an activity row carrying
// only Kind/Actor/Category/Action/Target, and each of those omissions has a
// visible consequence:
//
//   - No Summary. It is one of the three columns the server-side `?q=` filter
//     searches (job_name, workflow_name, summary — execution_mount.go's
//     listActivity) and one of five the client filter checks, and it is the only
//     descriptive line the Activity card renders. With it NULL the deleted
//     entity's name was unsearchable and the card had no body text at all.
//   - No JobName/WorkflowName. These drive the card's title, so a deletion
//     rendered with the literal word "config" as its headline — the entity's
//     name appeared nowhere, because `target` is not searched or rendered.
//   - No Outcome. The row was invisible to success/failure filtering and drew
//     with a neutral border and no status badge.
//
// Target and Action are kept as well as Summary: settings.writeActivity folds
// them into the summary and leaves both NULL, the compose mounts did the
// inverse. Writing all of them is the union of the two conventions and is what
// every reader — search, filter, card, export — actually wants.
//
// entityKind selects which name column is filled. The rowid deliberately is NOT
// recorded: it is about to be destroyed by this very delete, and SQLite reuses
// and reassigns rowids, so a stored one would eventually point at an unrelated
// row. The name is the stable handle every other table already keys on.
func (s *Server) auditDefinitionDelete(ctx context.Context, actor, entityKind, category, name string, scope *string) {
	summary := "Deleted " + name
	// change_log keeps its original details text rather than the summary: with
	// action="Deleted" and target=<name> already on the row, repeating them in
	// details adds nothing, while "Cronomicon <kind> deleted" records the one thing
	// the other columns don't — that this was an in-app definition, not a git one.
	_ = workflow.InsertChangeLog(ctx, s.db, actor, category, "Deleted", name, "Cronomicon "+entityKind+" deleted")
	p := workflow.ActivityParams{
		Kind:     "config",
		Outcome:  "success",
		Actor:    actor,
		Category: category,
		Action:   "Deleted",
		Target:   name,
		Summary:  summary,
	}
	if scope != nil {
		p.Scope = *scope
	}
	if entityKind == "workflow" {
		p.WorkflowName = name
	} else {
		p.JobName = name
	}
	_ = workflow.EmitActivity(ctx, s.db, p)
}

func (s *Server) forceScheduleReload(ctx context.Context) {
	if s.scheduleForceReload != nil {
		s.scheduleForceReload(ctx)
	}
}

func nullStrIf(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullIfZeroInt maps a zero to SQL NULL so an omitted optional integer stores as
// "unset". warn_after_seconds = 0 would otherwise mean "warn immediately".
func nullIfZeroInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}
