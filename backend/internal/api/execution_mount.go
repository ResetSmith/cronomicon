package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/envmerge"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sortparam"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
	"github.com/ResetSmith/cronomicon/internal/watchspec"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountExecution is owned by B5 (scheduler, run lifecycle, workflow engine).
//
// Routes implemented:
//
//	GET  /api/v1/jobs
//	GET  /api/v1/jobs/{jobId}
//	POST /api/v1/jobs/{jobId}/run       (RequireCSRF)
//	POST /api/v1/jobs/{jobId}/pause     (RequireCSRF)
//	POST /api/v1/jobs/{jobId}/resume    (RequireCSRF)
//	POST /api/v1/jobs/{jobId}/kill      (RequireCSRF)
//	PUT  /api/v1/job-tags/{jobId}       (RequireCSRF; operator-owned tags)
//	PUT  /api/v1/job-annotation/{jobId} (RequireCSRF; operator-owned annotation)
//	GET  /api/v1/workflows
//	GET  /api/v1/workflows/{workflowId}
//	PATCH /api/v1/workflows/{workflowId} (RequireCSRF)
//	POST /api/v1/workflows/{workflowId}/trigger (RequireCSRF)
//	PUT  /api/v1/workflow-tags/{workflowId} (RequireCSRF; operator-owned tags)
//	PUT  /api/v1/workflow-annotation/{workflowId} (RequireCSRF; operator-owned annotation)
//	GET  /api/v1/workflow-runs
//	GET  /api/v1/workflow-runs/{traceId}
//	GET  /api/v1/runs
//	GET  /api/v1/runs/{traceId}
//	GET  /api/v1/activity
//	GET  /api/v1/activity/actors
//	GET  /api/v1/change-log
//
// mountExecution returns the workflow engine it started, so slices mounted
// later can share the SAME instance. That sharing is load-bearing: the engine
// holds each running walk's cancel func in memory, so a second instance could
// trigger runs it cannot then soft-cancel.
func (s *Server) mountExecution(mux *http.ServeMux) *workflow.Engine {
	eng := workflow.New(s.db, s.log).WithShutdownWG(s.shutdownWG)
	eng.Start(s.runCtx)

	requireSession := s.auth.RequireSession
	requireCSRF := s.auth.RequireCSRF

	// ── Jobs ──────────────────────────────────────────────────────────────────
	mux.Handle("GET /api/v1/jobs",
		requireSession(http.HandlerFunc(s.listJobs)))
	mux.Handle("GET /api/v1/jobs/{jobId}",
		requireSession(http.HandlerFunc(s.getJob)))
	mux.Handle("POST /api/v1/jobs/{jobId}/run",
		requireSession(requireCSRF(http.HandlerFunc(s.runJob))))
	mux.Handle("POST /api/v1/jobs/{jobId}/pause",
		requireSession(requireCSRF(http.HandlerFunc(s.pauseJob))))
	mux.Handle("POST /api/v1/jobs/{jobId}/resume",
		requireSession(requireCSRF(http.HandlerFunc(s.resumeJob))))
	mux.Handle("POST /api/v1/jobs/{jobId}/kill",
		requireSession(requireCSRF(http.HandlerFunc(s.killJob))))
	// Operator-owned tags (tags-support.md D4/D6): SQLite-only, sync-preserved,
	// any logged-in user (session + CSRF).
	mux.Handle("PUT /api/v1/job-tags/{jobId}",
		requireSession(requireCSRF(http.HandlerFunc(s.updateJobTags))))
	// AN-2: the operator annotation (critical / contact / notes). Same storage
	// class and same gate as tags — see annotations_mount.go.
	mux.Handle("PUT /api/v1/job-annotation/{jobId}",
		requireSession(requireCSRF(http.HandlerFunc(s.updateJobAnnotation))))
	// (No runner-pin override route. RT-2 had one; v1.3.5 retired the operator
	// layer entirely — migration 1090. A job's pin is declared on the definition
	// and overridden per RUN at trigger time, and nowhere else.)

	// ── Workflows ─────────────────────────────────────────────────────────────
	mux.Handle("GET /api/v1/workflows",
		requireSession(http.HandlerFunc(s.listWorkflows)))
	mux.Handle("GET /api/v1/workflows/{workflowId}",
		requireSession(http.HandlerFunc(s.getWorkflow)))
	mux.Handle("PATCH /api/v1/workflows/{workflowId}",
		requireSession(requireCSRF(s.patchWorkflow(eng))))
	mux.Handle("POST /api/v1/workflows/{workflowId}/trigger",
		requireSession(requireCSRF(http.HandlerFunc(s.triggerWorkflow(eng)))))
	mux.Handle("PUT /api/v1/workflow-tags/{workflowId}",
		requireSession(requireCSRF(http.HandlerFunc(s.updateWorkflowTags))))
	mux.Handle("PUT /api/v1/workflow-annotation/{workflowId}",
		requireSession(requireCSRF(http.HandlerFunc(s.updateWorkflowAnnotation))))

	// ── Workflow runs ─────────────────────────────────────────────────────────
	mux.Handle("GET /api/v1/workflow-runs",
		requireSession(http.HandlerFunc(s.listWorkflowRuns)))
	mux.Handle("GET /api/v1/workflow-runs/{traceId}",
		requireSession(http.HandlerFunc(s.getWorkflowRun)))
	// WB-S2: soft-cancel a running workflow run (shares the trigger engine instance).
	mux.Handle("POST /api/v1/workflows/runs/{traceId}/cancel",
		requireSession(requireCSRF(http.HandlerFunc(s.cancelWorkflowRun(eng)))))

	// ── Schedules (inventory + projected upcoming runs) ────────────────────────
	mux.Handle("GET /api/v1/schedules",
		requireSession(http.HandlerFunc(s.listSchedules)))
	mux.Handle("GET /api/v1/schedules/upcoming",
		requireSession(http.HandlerFunc(s.listUpcomingSchedules)))

	// ── Runs / History ────────────────────────────────────────────────────────
	mux.Handle("GET /api/v1/runs",
		requireSession(http.HandlerFunc(s.listRuns)))
	mux.Handle("GET /api/v1/runs/{traceId}",
		requireSession(http.HandlerFunc(s.getRun)))

	// ── Activity / Change-log ─────────────────────────────────────────────────
	mux.Handle("GET /api/v1/activity",
		requireSession(http.HandlerFunc(s.listActivity)))
	mux.Handle("GET /api/v1/activity/actors",
		requireSession(http.HandlerFunc(s.listActivityActors)))
	mux.Handle("GET /api/v1/change-log",
		requireSession(http.HandlerFunc(s.listChangeLog)))

	return eng
}

// ─────────────────────────────────────────────────────────────────────────────
// Jobs
// ─────────────────────────────────────────────────────────────────────────────

type jobRow struct {
	ID int64 `json:"id"`
	// UID is the definition's stable surrogate identity (AF-4a): assigned when
	// the row is first seen — by sync or by the composer — and preserved across
	// syncs, edits and restores. Unlike ID (the SQLite rowid, a session-local
	// handle that a VACUUM may renumber) and unlike the name (which AF-4b will
	// allow to repeat across agencies), the uid never changes for the life of the
	// row. New integrations should prefer it; ID remains for compatibility.
	UID        string  `json:"uid,omitempty"`
	Name       string  `json:"name"`
	Source     string  `json:"source"`               // git | amadeus (A9)
	SourcePath *string `json:"sourcePath,omitempty"` // repo-relative file path (folder browsing)
	Type       string  `json:"type"`
	Host       *string `json:"host"`
	Scope      *string `json:"scope"`
	Status     string  `json:"status"`
	Schedule   *string `json:"schedule"`
	LastRunAt  *string `json:"lastRunAt"`
	// LastSkippedAt / LastSkipReason (FX-D1) — the newest SUPPRESSED fire, kept
	// apart from LastRunAt so neither can be mistaken for the other. A suppression
	// is evidence a fire was expected and deliberately did not happen; reporting
	// it as the last run made a job that has not executed in weeks look current.
	LastSkippedAt  *string `json:"lastSkippedAt"`
	LastSkipReason *string `json:"lastSkipReason"`
	NextRunAt      *string `json:"nextRunAt"`
	// PendingRunAt is the earliest deferred ad-hoc run parked against this job
	// (AR), nil when none — a run someone scheduled from the Run dialog, as
	// distinct from NextRunAt's standing-schedule projection.
	PendingRunAt   *string  `json:"pendingRunAt,omitempty"`
	LastDurationMs *int64   `json:"lastDurationMs"`
	LastChangedAt  *string  `json:"lastChangedAt"`
	CreatedAt      *string  `json:"createdAt"`
	LastModifiedAt *string  `json:"lastModifiedAt"`
	Tags           []string `json:"tags"`
	Requestable    bool     `json:"requestable"`
	// DeletedAt is the recycle-bin stamp (RH). Non-nil means the definition is
	// binned: hidden from the catalog, never fired, and refused by the run and
	// edit paths until it is restored.
	DeletedAt *string `json:"deletedAt,omitempty"`
	// SL soft deadlines. Echoed for the same Gap-B reason as every other
	// persisted field: the publish builder regenerates the whole YAML from this
	// response, so a field it cannot read is a field a schedule edit strips.
	WarnAfterSeconds *int64  `json:"warnAfterSeconds,omitempty"`
	MustFinishBy     *string `json:"mustFinishBy,omitempty"`
	// Watch — ET-D file-arrival triggers, echoed for the Gap-B reason: the
	// publish builder regenerates the whole YAML from this response.
	Watch         []watchspec.Watch `json:"watch,omitempty"`
	WorkflowID    *int64            `json:"workflowId"`
	QueuedReason  *string           `json:"queuedReason"`
	ScheduleCount int               `json:"scheduleCount,omitempty"` // entry count for the list "+N" badge
	// Agencies is the job's DERIVED agency set (RB-23), computed from its scope
	// via scope_agencies. Read-only and display-only: once access is departmental,
	// "show me my department's jobs" is the first thing anyone asks, and the
	// catalog did not mention agencies at all.
	Agencies []string `json:"agencies,omitempty"`
	// CanRun/CanKill are the caller's PER-ROW authority (RB-24), computed
	// server-side. The /capabilities flags are a flat union — "may trigger
	// SOMEWHERE" — which cannot answer per-row truth: an operator scoped to Finance
	// reports triggerJobs=true and still may not touch a Tax job. Without these the
	// frontend re-implements authorization and drifts, which is exactly the failure
	// mode cited for not gating one route in isolation.
	//
	// Display only. They gate buttons; the run path enforces scope but not the verb
	// until RB-2, and they become departmentally correct when RB-15 makes grants
	// authoritative — at which point these fields already carry the right answer.
	CanRun  bool `json:"canRun"`
	CanKill bool `json:"canKill"`

	// AN-2 — the operator annotation (migration 1060), flattened into this row.
	// Operator-owned and sync-preserved, unlike Description, which is Git-owned
	// and overwritten on every sync: the two coexist and neither writes the
	// other. List rows carry Critical/Contact; the rest is detail-only.
	annotationFields

	// Detail-only fields (omitted from list rows). The builder regenerates the
	// whole YAML from this response, so every persisted field it must echo back
	// is returned here to avoid silent data loss on publish (Gap B). schedules
	// carries the multi-entry list with per-entry next-run projections.
	Description *string `json:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`   // raw enabled flag (detail-only); list rows leave nil. Exact composer prefill (JC9).
	ScriptRef   *string `json:"scriptRef,omitempty"` // B-Git: name of the referenced Script (nil = legacy inline)
	Command     *string `json:"command,omitempty"`
	Script      *string `json:"script,omitempty"`
	ScriptPath  *string `json:"scriptPath,omitempty"`
	Executor    *string `json:"executor,omitempty"`
	// RT-2 — the runner pin. RunnerTag is the declared pin (git-owned for git
	// jobs, Composer-owned for amadeus ones). RunnerTagEffective is the resolved
	// job-level answer the Run dialog prefills from; it is computed server-side
	// so clients cannot implement the precedence three ways, and it stays a
	// separate field even though it now equals RunnerTag — the per-run rung above
	// it is still resolved server-side, and collapsing the two would put the
	// precedence back in the clients. (The operator override that used to sit
	// between them was retired in v1.3.5 — migration 1090.)
	RunnerTag          *string             `json:"runnerTag,omitempty"`
	RunnerTagEffective *string             `json:"runnerTagEffective,omitempty"`
	ConcurrencyPolicy  string              `json:"concurrencyPolicy,omitempty"`
	ConcurrencyKey     *string             `json:"concurrencyKey,omitempty"`
	TimeoutSeconds     *int64              `json:"timeoutSeconds,omitempty"`
	Retries            int                 `json:"retries,omitempty"`
	BackoffSeconds     int                 `json:"backoffSeconds,omitempty"`  // WB-R1 job-level default
	ContinueOnError    bool                `json:"continueOnError,omitempty"` // WB-R1 job-level default
	Schedules          []scheduleEntryResp `json:"schedules,omitempty"`
	Env                map[string]string   `json:"env,omitempty"`     // job-level env (JC10); detail-only, mirrors JobComposeInput.env
	Prompts            []gitlab.PromptSpec `json:"prompts,omitempty"` // declared prompt variables (UDV1); detail-only, mirrors JobComposeInput.prompts
	// JR-Q5 — per-job run-input enforcement: "warn" (default) | "block". Detail-only.
	// Drives the Run dialog's gate (a "block" job hides the "Run anyway" escape) and
	// the server-side 422 in runJob.
	PromptEnforcement string   `json:"promptEnforcement,omitempty"`
	Requires          []string `json:"requires,omitempty"` // requirement tokens (§5/RX.13); detail-only, mirrors JobComposeInput.requires
	// CA Phase B — declarative "connect as" identity (username + stored-credential
	// LABEL, names only). Detail-only: composer prefill + the Run dialog's
	// displayed defaults (CA-11).
	SSHUser       *string `json:"sshUser,omitempty"`
	SSHCredential *string `json:"sshCredential,omitempty"`
	// RP-14 — env-var NAMES the runner resolves from its own environment into a
	// local-toolchain child process (RX.9). Detail-only, mirrors
	// JobComposeInput.envPassthrough so the composer round-trips it.
	EnvPassthrough []string `json:"envPassthrough,omitempty"`
}

// scopeWhereFragment returns a WHERE fragment (and its args) restricting the
// scope column `col` to the actor's readable scopes — GLOBAL (NULL) rows plus its
// granted scopes, or GLOBAL only for an empty grant. An unrestricted actor (or no
// identity) yields "" so the caller adds nothing. It mirrors the inline scope
// filter in listRuns; the SU-2 read-path fix uses it to give listJobs and
// listWorkflowRuns the same gating. `col` is a fixed identifier chosen by the
// caller (never user input), so it is safe to interpolate.
func (s *Server) scopeWhereFragment(r *http.Request, col string) (string, []any) {
	id, hasID := auth.IdentityFrom(r.Context())
	if hasID && id.Unrestricted() {
		return "", nil
	}
	// Fail closed like listRuns: an unrestricted actor short-circuits above; anyone
	// else (restricted, or — unreachable behind RequireSession — no identity) is
	// filtered, with an empty grant collapsing to GLOBAL-only.
	allowed := id.AllowedScopes
	if len(allowed) == 0 {
		return " AND " + col + " IS NULL", nil
	}
	placeholders := strings.Repeat("?,", len(allowed))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(allowed))
	for i, sc := range allowed {
		args[i] = sc
	}
	return " AND (" + col + " IS NULL OR " + col + " IN (" + placeholders + "))", args
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typeFilter := q.Get("type")
	scopeFilter := q.Get("scope")
	statusFilter := q.Get("status")
	tagFilter := tagutil.ParseQuery(q)
	search := q.Get("q")
	page, pageSize := pageParams(q)

	// Base query, with the display status and every list-row aggregate derived IN
	// SQL so the whole page is one round-trip (CC.2 + CC.8). paused_count and the
	// latest-run columns feed the CASE below, so a status= filter applies BEFORE
	// COUNT(*) and LIMIT/OFFSET (deriving status after pagination — the old shape —
	// returned short pages and wrong totals). The latest run is joined once by its
	// rowid, so status/timestamp/duration all come from the same row; this plus the
	// schedule_count subquery replaces the two per-row follow-up queries (latest-run
	// fetch + schedule COUNT) that previously cost up to ~800 extra queries/page.
	inner := `
		SELECT
			j.rowid AS rowid, j.name AS name, j.source AS source, j.run_type AS run_type,
			j.description AS description,
			j.target_host AS target_host, j.scope AS scope,
			j.schedule AS schedule, j.enabled AS enabled,
			j.concurrency_policy AS concurrency_policy, j.tags AS tags,
			j.synced_at AS synced_at, j.created_at AS created_at, j.last_modified_at AS last_modified_at, j.source_path AS source_path,
			j.uid AS uid,
			(SELECT COUNT(*) FROM paused_jobs p WHERE p.source = j.source AND p.owner_kind = 'job' AND p.name = j.name) AS paused_count,
			lr.status AS last_run_status,
			lr.created_at AS last_run_at,
			lr.duration_ms AS last_run_duration_ms,
			ls.created_at AS last_skipped_at,
			ls.queued_reason AS last_skip_reason,
			(SELECT COUNT(*) FROM definition_schedules ds WHERE ds.owner_source = j.source AND ds.owner_kind = 'job' AND ds.owner_name = j.name) AS schedule_count,
			-- RB-23: the job's agencies, DERIVED from its scope. Jobs deliberately
			-- carry no agency column: a second source of truth could disagree with
			-- the first (a job tagged Tax whose scope belongs to Finance) and every
			-- authz decision would then have to pick a winner. The run path already
			-- settled this by snapshotting the derived set onto the run rather than
			-- letting a job carry its own. Display only — no storage, no authoring.
			(SELECT GROUP_CONCAT(a.name, '\x1f')
			   FROM scope_agencies sa
			   JOIN agencies a ON a.id = sa.agency_id
			   JOIN scopes   s ON s.id = sa.scope_id
			  WHERE s.name = j.scope) AS agencies,
			-- AN-2: the annotation's LIST subset — the chip and the contact, not
			-- the notes. A 4KB note per row times a page is a lot of bytes for
			-- something only the expanded view renders; the detail path reads the
			-- rest. COALESCE because the join is outer: no annotation is the
			-- common case, and it must arrive as "not critical", not as NULL.
			COALESCE(an.critical, 0) AS critical,
			COALESCE(an.contact, '') AS contact
		FROM jobs j
		-- Joined on the uid, never the name: two amadeus jobs may share a name
		-- (R2-5), and a name join would show one twin the other's chip.
		LEFT JOIN annotations an ON an.owner_kind = 'job' AND an.owner_uid = j.uid
		-- FX-D1: the newest EXECUTED run, not the newest ROW. A calendar veto, a
		-- Forbid refusal, a queue-full skip and a missed-fire marker are all real
		-- rows in this table with status='skipped' (they exist so a suppression is
		-- provable), but none of them ran. Taking the newest row regardless made a
		-- job that was suppressed this morning report "Last run: 02:00" with no
		-- duration, and -- because 'skipped' matches neither arm of the status CASE
		-- below -- derive to idle, so a job that succeeded yesterday dropped out
		-- of ?status=success and out of its own count.
		LEFT JOIN runs lr ON lr.rowid = (
			SELECT r.rowid FROM runs r
			WHERE r.job_source = j.source AND r.job_name = j.name
			  AND r.status <> 'skipped'
			ORDER BY r.created_at DESC, r.rowid DESC LIMIT 1)
		-- The suppression is not discarded, only separated: the newest one rides
		-- alongside so the row can say "last fire suppressed" without pretending
		-- that fire was a run.
		LEFT JOIN runs ls ON ls.rowid = (
			SELECT r.rowid FROM runs r
			WHERE r.job_source = j.source AND r.job_name = j.name
			  AND r.status = 'skipped'
			ORDER BY r.created_at DESC, r.rowid DESC LIMIT 1)
		WHERE j.deleted_at IS NULL -- RH: the recycle bin is not the catalog
	`
	var args []any

	if typeFilter != "" {
		inner += " AND j.run_type = ?"
		args = append(args, typeFilter)
	}
	if scopeFilter != "" {
		inner += " AND j.scope = ?"
		args = append(args, scopeFilter)
	}
	if search != "" {
		inner += " AND j.name LIKE ?"
		args = append(args, "%"+search+"%")
	}
	if frag, fargs := tagFilter.SQLFilter("j.tags"); frag != "" {
		inner += " AND " + frag
		args = append(args, fargs...)
	}
	// SU-2: restrict the listing to the actor's readable scopes. This INTERSECTS
	// with any explicit ?scope= above (it does not replace it), so a restricted
	// actor requesting an out-of-scope ?scope= simply gets no rows.
	if frag, fargs := s.scopeWhereFragment(r, "j.scope"); frag != "" {
		inner += frag
		args = append(args, fargs...)
	}

	// Display status, mirroring deriveJobStatus (paused > latest-run state > idle).
	derived := `SELECT *, CASE
			WHEN enabled = 0 OR paused_count > 0 THEN 'paused'
			WHEN last_run_status IN ('running', 'queued', 'success') THEN last_run_status
			WHEN last_run_status IN ('failure', 'killed', 'danger', 'warning') THEN 'danger'
			ELSE 'idle'
		END AS status FROM (` + inner + `)`

	statusWhere := ""
	if statusFilter != "" {
		statusWhere = " WHERE status = ?"
		args = append(args, statusFilter)
	}

	// A failed count must fail the request: a silent total=0 next to a full
	// page makes the frontend pager hide every other page.
	var total int
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM ("+derived+")"+statusWhere, args...).Scan(&total); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	query := `SELECT
			rowid, name, source, run_type, description,
			target_host, scope, schedule, enabled,
			concurrency_policy, tags,
			synced_at, created_at, last_modified_at, source_path,
			uid,
			last_run_at, last_run_duration_ms, last_skipped_at, last_skip_reason, schedule_count, agencies,
			critical, contact, status
		FROM (` + derived + `)` + statusWhere + ` ORDER BY name LIMIT ? OFFSET ?`
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	// Drain the result set fully BEFORE running any per-row follow-up query.
	// Holding the outer rows iterator open while issuing nested queries would
	// pin a connection and deadlock under a constrained pool (see db.maxOpenConns).
	type jobBase struct {
		rowid                 int64
		name, source, runType string
		description           sql.NullString
		targetHost            sql.NullString
		scope                 sql.NullString
		schedule              sql.NullString
		enabled               int
		concPolicy            string
		tags                  string
		syncedAt              sql.NullString
		createdAt             sql.NullString
		lastModifiedAt        sql.NullString
		sourcePath            sql.NullString
		uid                   sql.NullString
		lastRunAt             sql.NullString
		lastSkippedAt         sql.NullString
		lastSkipReason        sql.NullString
		lastDurationMs        sql.NullInt64
		scheduleCount         int
		agencies              sql.NullString
		critical              int
		contact               string
		status                string
	}
	// RB-24: the caller, resolved once for the whole page rather than per row.
	// An unauthenticated request cannot reach here (the route is session-gated), so
	// the zero Identity is a fail-closed default rather than a real case.
	actor, _ := auth.IdentityFrom(r.Context())
	var bases []jobBase
	for rows.Next() {
		var j jobBase
		if err := rows.Scan(&j.rowid, &j.name, &j.source, &j.runType,
			&j.description,
			&j.targetHost, &j.scope, &j.schedule, &j.enabled,
			&j.concPolicy, &j.tags, &j.syncedAt,
			&j.createdAt, &j.lastModifiedAt, &j.sourcePath,
			&j.uid,
			&j.lastRunAt, &j.lastDurationMs, &j.lastSkippedAt, &j.lastSkipReason,
			&j.scheduleCount, &j.agencies, &j.critical, &j.contact, &j.status); err != nil {
			continue
		}
		bases = append(bases, j)
	}
	rows.Close()

	// FX-E8 — nextRunAt, batched for the page like the workflows list does it
	// (CC.9: one query for every row, drained before any compute). The field has
	// been documented on the job row since the spec was written and assigned only
	// on the DETAIL path, so every list consumer read a promise the list never
	// kept. Computed through the full Spec (cron, interval, window) rather than
	// bare cronutil.Next — the projection must not show a phantom fire outside an
	// entry's window, nor nothing at all for an interval-mode entry.
	nextRuns := map[[2]string]*string{}
	if len(bases) > 0 {
		names := make([]any, 0, len(bases))
		seen := map[string]bool{}
		for _, j := range bases {
			if !seen[j.name] {
				seen[j.name] = true
				names = append(names, j.name)
			}
		}
		ph := strings.Repeat(",?", len(names))[1:]
		srows, err := s.db.QueryContext(r.Context(), `
			SELECT owner_source, owner_name, cron, COALESCE(interval,''), COALESCE(start_at,''), COALESCE(end_at,'')
			  FROM definition_schedules
			 WHERE owner_kind = 'job' AND owner_name IN (`+ph+`)`, names...)
		if err != nil {
			// The projection degrades to "no next run" for the page rather than
			// failing the list — but silently, it reads as "nothing scheduled",
			// so the failure is at least on record.
			s.log.Warn("jobs list: batch next-run query failed; page shows no projections", "err", err)
		}
		if err == nil {
			type entrySpec struct{ cron, interval, startAt, endAt string }
			specs := map[[2]string][]entrySpec{}
			for srows.Next() {
				var src, nm string
				var e entrySpec
				if srows.Scan(&src, &nm, &e.cron, &e.interval, &e.startAt, &e.endAt) == nil {
					k := [2]string{src, nm}
					specs[k] = append(specs[k], e)
				}
			}
			srows.Close() // drain BEFORE any further queries (SQLite pool rule)
			now := time.Now()
			appLoc := s.appLocation()
			pausedKeys := map[[2]string]bool{}
			for _, j := range bases {
				if j.status == "paused" {
					pausedKeys[[2]string{j.source, j.name}] = true
				}
			}
			for k, es := range specs {
				if pausedKeys[k] {
					continue // matches the detail path: a paused job projects no next run
				}
				entries := make([]scheduleEntryResp, 0, len(es))
				for _, e := range es {
					resp := scheduleEntryResp{}
					win := cronutil.NewWindow(
						windowBound(sql.NullString{String: e.startAt, Valid: e.startAt != ""}),
						windowBound(sql.NullString{String: e.endAt, Valid: e.endAt != ""}))
					spec := cronutil.Spec{Cron: e.cron, Interval: e.interval, Window: win}
					if next, ok := cronutil.NextSpec(spec, now, appLoc); ok {
						resp.NextRunAt = rfc3339Ptr(next)
					}
					entries = append(entries, resp)
				}
				nextRuns[k] = earliestNextRunAt(entries)
			}
		}
	}

	items := []jobRow{}
	for _, j := range bases {
		jr := jobRow{
			ID:     j.rowid,
			Name:   j.name,
			Source: j.source,
			Type:   j.runType,
			Status: j.status,
		}
		if j.description.Valid && j.description.String != "" {
			jr.Description = &j.description.String
		}
		if j.targetHost.Valid {
			jr.Host = &j.targetHost.String
		}
		if j.scope.Valid {
			jr.Scope = &j.scope.String
		}
		if j.schedule.Valid {
			jr.Schedule = &j.schedule.String
		}
		if j.createdAt.Valid {
			jr.CreatedAt = &j.createdAt.String
		}
		if j.lastModifiedAt.Valid {
			jr.LastModifiedAt = &j.lastModifiedAt.String
		}
		if j.sourcePath.Valid {
			jr.SourcePath = &j.sourcePath.String
		}
		jr.UID = j.uid.String
		jr.Tags = tagutil.Parse(j.tags)
		if j.agencies.Valid && j.agencies.String != "" {
			jr.Agencies = strings.Split(j.agencies.String, "\x1f")
		}
		// RB-24: evaluate the caller's authority against THIS row's scope.
		jr.CanRun = actor.Can(auth.PermTriggerJobs, j.scope.String)
		jr.CanKill = actor.Can(auth.PermKillJobs, j.scope.String)
		// AN-2 list subset — Notes/NotesBy/NotesAt stay zero here on purpose.
		jr.Critical = j.critical == 1
		jr.Contact = j.contact

		// Last-run timestamp/duration and the schedule "+N" badge count come from
		// the page query (CC.8) — no per-row follow-up. The last-run status is
		// already folded into the derived status column above.
		if j.lastRunAt.Valid {
			jr.LastRunAt = &j.lastRunAt.String
		}
		if j.lastSkippedAt.Valid {
			jr.LastSkippedAt = &j.lastSkippedAt.String
		}
		if j.lastSkipReason.Valid && j.lastSkipReason.String != "" {
			jr.LastSkipReason = &j.lastSkipReason.String
		}
		if j.lastDurationMs.Valid {
			jr.LastDurationMs = &j.lastDurationMs.Int64
		}
		jr.ScheduleCount = j.scheduleCount
		jr.NextRunAt = nextRuns[[2]string{j.source, j.name}]

		items = append(items, jr)
	}

	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	// SU-2: a scope-restricted actor must not read an out-of-scope job's detail
	// (script body + plaintext env) — the shared gate 404s binned and
	// out-of-scope jobs alike (FX2-F1; killJob is the studied exception: it
	// acts on a run, not the definition).
	jr, ok := s.requireReadableJob(w, r, r.PathValue("jobId"))
	if !ok {
		return
	}
	httpx.JSON(w, http.StatusOK, jr)
}

func (s *Server) fetchJobByID(r *http.Request, jobID string) *jobRow {
	return s.fetchJobDetail(r, "rowid", jobID)
}

// defSource resolves a definition's source ('git'|'amadeus') by its rowid, for
// the source-aware paused_jobs key (migration 170 / Q-G). `table` is a fixed
// identifier chosen by the caller (never user input). Defaults to 'git'.
func (s *Server) defSource(r *http.Request, table, rowid string) string {
	var src sql.NullString
	switch table {
	case "workflows":
		_ = s.db.QueryRowContext(r.Context(), `SELECT source FROM workflows WHERE rowid = ?`, rowid).Scan(&src)
	default:
		_ = s.db.QueryRowContext(r.Context(), `SELECT source FROM jobs WHERE rowid = ?`, rowid).Scan(&src)
	}
	if src.Valid && src.String != "" {
		return src.String
	}
	return "git"
}

// fetchJobDetail returns the full job-detail row (the Gap B field set + the
// multi-entry schedules list), looked up by the given column ("rowid"|"name").
// whereCol is a fixed identifier chosen by the caller, never user input.
func (s *Server) fetchJobDetail(r *http.Request, whereCol, arg string) *jobRow {
	var j struct {
		envJSON                               sql.NullString
		promptsJSON                           sql.NullString
		promptEnforcement                     string
		rowid                                 int64
		name, source, runType                 string
		description                           sql.NullString
		targetHost, scope, schedule           sql.NullString
		enabled                               int
		concPolicy                            string
		concKey                               sql.NullString
		tags                                  string
		command, script, scriptPath, executor sql.NullString
		scriptRef                             sql.NullString
		timeoutSeconds                        sql.NullInt64
		retries                               int
		backoffSeconds                        int
		continueOnError                       int
		createdAt, lastModifiedAt             sql.NullString
		sourcePath                            sql.NullString
		requiresJSON                          sql.NullString
		requestable                           int
		deletedAt                             sql.NullString
		warnAfterSeconds                      sql.NullInt64
		mustFinishBy                          sql.NullString
		watchJSON                             sql.NullString
		envPassthroughJSON                    sql.NullString
		sshUser, sshCredential                sql.NullString
		runnerTag                             sql.NullString
		uid                                   sql.NullString
	}
	err := s.db.QueryRowContext(r.Context(), `
		SELECT rowid, name, source, run_type, description, target_host, scope, schedule, enabled,
		       concurrency_policy, concurrency_key, tags,
		       command, script, script_path, executor, script_ref,
		       timeout_seconds, retries,
		       COALESCE(backoff_seconds,0), COALESCE(continue_on_error,0),
		       created_at, last_modified_at, env_json, source_path, prompts_json, requires_json,
		       COALESCE(prompt_enforcement,'warn'), ssh_user, ssh_credential, env_passthrough,
		       COALESCE(requestable,0), deleted_at, warn_after_seconds, must_finish_by, watch_json,
		       runner_tag, uid
		FROM jobs WHERE `+whereCol+` = ?
	`, arg).Scan(&j.rowid, &j.name, &j.source, &j.runType, &j.description, &j.targetHost, &j.scope, &j.schedule,
		&j.enabled, &j.concPolicy, &j.concKey, &j.tags,
		&j.command, &j.script, &j.scriptPath, &j.executor, &j.scriptRef,
		&j.timeoutSeconds, &j.retries,
		&j.backoffSeconds, &j.continueOnError,
		&j.createdAt, &j.lastModifiedAt, &j.envJSON, &j.sourcePath, &j.promptsJSON, &j.requiresJSON,
		&j.promptEnforcement, &j.sshUser, &j.sshCredential, &j.envPassthroughJSON,
		&j.requestable, &j.deletedAt, &j.warnAfterSeconds, &j.mustFinishBy, &j.watchJSON,
		&j.runnerTag, &j.uid)
	if err != nil {
		return nil
	}

	status := s.deriveJobStatus(r, j.source, j.name, j.enabled, j.schedule.String)
	jr := &jobRow{
		ID:                j.rowid,
		Name:              j.name,
		Source:            j.source,
		Type:              j.runType,
		Status:            status,
		Tags:              tagutil.Parse(j.tags),
		ConcurrencyPolicy: j.concPolicy,
		Retries:           j.retries,
		BackoffSeconds:    j.backoffSeconds,
		ContinueOnError:   j.continueOnError == 1,
		// ET-B/PF-Q15: the external-trigger opt-in. Declared on jobRow since A7
		// but never SELECTed, so it always read false — which meant the composer's
		// preserve-and-resubmit round-trip silently cleared it on every edit. It
		// had to be read before it could be enforced.
		Requestable: j.requestable == 1,
		UID:         j.uid.String,
	}
	// AN-2 — the FULL annotation on the detail path, where the expanded view
	// renders the notes. A follow-up query rather than a join: the row above is
	// read with QueryRowContext, which consumes and closes its result set before
	// returning, so nothing nests inside an open iterator (the SQLite pool rule
	// that governs listJobs' drain-first loop).
	jr.annotationFields = s.loadAnnotation(r.Context(), "job", j.uid.String).fields()
	if j.deletedAt.Valid {
		jr.DeletedAt = &j.deletedAt.String
	}
	if j.warnAfterSeconds.Valid {
		jr.WarnAfterSeconds = &j.warnAfterSeconds.Int64
	}
	if j.mustFinishBy.Valid && j.mustFinishBy.String != "" {
		jr.MustFinishBy = &j.mustFinishBy.String
	}
	if j.watchJSON.Valid {
		if ws, err := watchspec.Parse(j.watchJSON.String); err == nil {
			jr.Watch = ws
		}
	}
	// Raw enabled (detail-only) for an exact composer prefill — distinct from the
	// derived status that collapses disabled with schedule-paused (JC9 / D1).
	enabledBool := j.enabled == 1
	jr.Enabled = &enabledBool
	if j.targetHost.Valid {
		jr.Host = &j.targetHost.String
	}
	if j.sourcePath.Valid {
		jr.SourcePath = &j.sourcePath.String
	}
	if j.scope.Valid {
		jr.Scope = &j.scope.String
	}
	if j.schedule.Valid {
		jr.Schedule = &j.schedule.String
	}
	if j.description.Valid && j.description.String != "" {
		jr.Description = &j.description.String
	}
	if j.concKey.Valid {
		jr.ConcurrencyKey = &j.concKey.String
	}
	if j.timeoutSeconds.Valid {
		jr.TimeoutSeconds = &j.timeoutSeconds.Int64
	}
	if j.command.Valid {
		jr.Command = &j.command.String
	}
	if j.script.Valid {
		jr.Script = &j.script.String
	}
	if j.scriptPath.Valid {
		jr.ScriptPath = &j.scriptPath.String
	}
	if j.executor.Valid {
		jr.Executor = &j.executor.String
	}
	if j.scriptRef.Valid {
		jr.ScriptRef = &j.scriptRef.String
	}
	// RT-2 — the declared pin plus the resolved job-level answer.
	if j.runnerTag.Valid && j.runnerTag.String != "" {
		jr.RunnerTag = &j.runnerTag.String
	}
	if eff := resolveJobRunnerTag(j.runnerTag); eff != "" {
		jr.RunnerTagEffective = &eff
	}
	if j.sshUser.Valid && j.sshUser.String != "" {
		jr.SSHUser = &j.sshUser.String
	}
	if j.sshCredential.Valid && j.sshCredential.String != "" {
		jr.SSHCredential = &j.sshCredential.String
	}
	if j.createdAt.Valid {
		jr.CreatedAt = &j.createdAt.String
	}
	if j.lastModifiedAt.Valid {
		jr.LastModifiedAt = &j.lastModifiedAt.String
	}
	// Job-level env (JC10); detail-only, for an exact composer prefill round-trip.
	if j.envJSON.Valid && j.envJSON.String != "" {
		_ = json.Unmarshal([]byte(j.envJSON.String), &jr.Env)
	}
	// Declared prompt variables (UDV1); detail-only, drives the Run dialog and the
	// composer prefill. Always a valid JSON array (DEFAULT '[]'); unmarshal best-effort.
	if j.promptsJSON.Valid && j.promptsJSON.String != "" {
		_ = json.Unmarshal([]byte(j.promptsJSON.String), &jr.Prompts)
	}
	// JR-Q5 — enforcement mode, normalized on read as well as on write so a row
	// written directly to the DB can't present an unrecognized mode to the UI.
	jr.PromptEnforcement = gitlab.NormalizePromptEnforcement(j.promptEnforcement)
	// Requirement tokens (§5/RX.13); detail-only, drives claim-gating display +
	// composer prefill. Always a valid JSON array (DEFAULT '[]').
	if j.requiresJSON.Valid && j.requiresJSON.String != "" {
		_ = json.Unmarshal([]byte(j.requiresJSON.String), &jr.Requires)
	}
	if j.envPassthroughJSON.Valid && j.envPassthroughJSON.String != "" {
		_ = json.Unmarshal([]byte(j.envPassthroughJSON.String), &jr.EnvPassthrough)
	}

	// FX-D1 — see the list query: the newest EXECUTED run, with the newest
	// suppression carried separately rather than allowed to impersonate one.
	var lastRunAt sql.NullString
	var lastDur sql.NullInt64
	_ = s.db.QueryRowContext(r.Context(), `
		SELECT created_at, duration_ms FROM runs
		WHERE job_source = ? AND job_name = ? AND status <> 'skipped'
		-- rowid breaks the tie, matching the list query: created_at is
		-- second-precision, so two runs in the same second would otherwise let the
		-- list and the detail disagree about which was last.
		ORDER BY created_at DESC, rowid DESC LIMIT 1
	`, j.source, j.name).Scan(&lastRunAt, &lastDur)
	if lastRunAt.Valid {
		jr.LastRunAt = &lastRunAt.String
	}
	if lastDur.Valid {
		jr.LastDurationMs = &lastDur.Int64
	}
	// FX-E3 — queuedReason, assigned at last. The field was declared on this row,
	// documented in the spec with an example ("waiting for terraform-capable
	// runner"), consumed by nothing and WRITTEN by nothing: the runs-level value
	// is real (eight writers) but no reader ever lifted it to the job. The newest
	// still-queued run's reason is the one the catalog can usefully show —
	// "why is this job's run not starting" is a question about the run that is
	// currently not starting.
	var queuedReason sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `
		SELECT queued_reason FROM runs
		WHERE job_source = ? AND job_name = ? AND status = 'queued'
		ORDER BY created_at DESC, rowid DESC LIMIT 1
	`, j.source, j.name).Scan(&queuedReason)
	if queuedReason.Valid && queuedReason.String != "" {
		jr.QueuedReason = &queuedReason.String
	}

	var lastSkipAt, lastSkipReason sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `
		SELECT created_at, COALESCE(queued_reason,'') FROM runs
		WHERE job_source = ? AND job_name = ? AND status = 'skipped'
		ORDER BY created_at DESC, rowid DESC LIMIT 1
	`, j.source, j.name).Scan(&lastSkipAt, &lastSkipReason)
	if lastSkipAt.Valid {
		jr.LastSkippedAt = &lastSkipAt.String
	}
	if lastSkipReason.Valid && lastSkipReason.String != "" {
		jr.LastSkipReason = &lastSkipReason.String
	}

	// Multi-entry schedules + per-definition nextRunAt (nil when paused).
	jr.Schedules = s.loadSchedules(r.Context(), j.source, "job", j.name, status == "paused")
	jr.NextRunAt = earliestNextRunAt(jr.Schedules)
	// AR — surface the earliest parked ad-hoc run so the job row can say
	// "1 scheduled run · Wed 5:00 PM" without a second endpoint.
	// FX-D5 — UNGATED rows only. This field is documented as "the earliest
	// deferred ad-hoc run" and the UI renders it as a future instant, but a row
	// held by a gate (the Queue policy, the recycle bin, a pause) is parked with
	// run_at = now, so including one made the job row advertise a scheduled run
	// in the PAST. It is the same reader-shape /schedules/upcoming was fixed for;
	// gate_kind is what tells a deferral from a hold.
	var pendingAt sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `SELECT MIN(run_at) FROM pending_runs
		WHERE kind = 'job' AND source = ? AND name = ? AND status = 'pending'
		  AND gate_kind IS NULL`,
		j.source, j.name).Scan(&pendingAt)
	if pendingAt.Valid && pendingAt.String != "" {
		jr.PendingRunAt = &pendingAt.String
	}
	return jr
}

// trimNonEmpty trims each entry and drops blanks, so an operator's trailing
// empty row never becomes an empty argv element.
func trimNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Trigger kinds a run handler can be invoked under.
//
// 'webhook' has existed in the runs and workflow_runs CHECK constraints since
// migration 890 with NO producer — it was minted for exactly this and left
// unused, which is why the service-account trigger surface (ET-B) needs no
// schema change to record its provenance.
const (
	triggerKindManual = "manual"
	triggerKindAPI    = "webhook"
)

// runJob is the session-authenticated "Run now" handler.
func (s *Server) runJob(w http.ResponseWriter, r *http.Request) {
	s.runJobWithKind(w, r, triggerKindManual)
}

// runJobWithKind is the single enforcement site for starting a job run,
// parameterized only by the provenance stamped on the resulting run.
//
// ET-B routes a service-account trigger through here rather than reimplementing
// it: every gate below — scope+verb, RB-26/RB-27, unbound references,
// connect-as identity, host/group subsetting, ansible option validation, the
// global cap, Forbid, and declared-input prompt enforcement — must apply
// identically whether a person clicked Run or a monitoring system posted a
// token. The ONLY difference a caller may introduce is triggerKind.
func (s *Server) runJobWithKind(w http.ResponseWriter, r *http.Request, triggerKind string) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}

	jobID := r.PathValue("jobId")
	jr := s.fetchJobByID(r, jobID)
	if jr == nil || jr.DeletedAt != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}

	// Parse optional scope / target-host / executor / env overrides (R5.1, F1).
	var body struct {
		Scope        string   `json:"scope"`
		TargetHost   string   `json:"targetHost"`
		TargetHosts  []string `json:"targetHosts"`  // F2 host subset within the bound scope
		TargetGroups []string `json:"targetGroups"` // M3 group subset within the bound scope
		AnsibleLimit string   `json:"ansibleLimit"` // M3 raw --limit passthrough (ansible/runner only)
		Executor     string   `json:"executor"`     // per-trigger override: ssh|runner
		// RT-2 — per-run pin override, tri-state (RT-Q6): absent/null inherits the
		// job's effective pin, "" runs THIS run unpinned even if the job is pinned
		// (the break-glass case when the tagged runners are all down), "x" pins to x.
		// A *string, not a string, because those three states are not two.
		RunnerTag *string           `json:"runnerTag"`
		Env       map[string]string `json:"env"` // F1 per-run env overrides (plaintext k/v)
		// CA (the ssh-user plan) — per-run "connect as" identity:
		// remote username and/or stored SSH credential LABEL (CA-Q2, names only —
		// key bytes never ride the request). Applied over every resolved target's
		// per-host identity at dispatch; bastion hops unaffected (CA-Q4).
		// ssh-family run types only; picking a credential needs ManageEnvVars
		// (CA-Q1, the V2-11 "reference = material grant" rule).
		SSHUser       string `json:"sshUser"`
		SSHCredential string `json:"sshCredential"`
		// Phase 3 (RP-15) — advanced ansible run options, ansible run types only
		// (422 otherwise, the ansibleLimit gate's rationale: any other toolchain
		// would silently swallow them). Per-run only and envelope-carried: these
		// are the flags an operator reaches for AT trigger time, not standing job
		// properties, so there are no columns and no producer fold.
		// QP — per-trigger claim priority (PF-Q11). Higher goes first; ties break
		// oldest-first. Deliberately per-trigger only: a job-spec default is how
		// one job starves another permanently rather than merely going first now.
		Priority          int               `json:"priority"`
		AnsibleCheck      bool              `json:"ansibleCheck"`      // --check (dry run)
		AnsibleDiff       bool              `json:"ansibleDiff"`       // --diff
		AnsibleTags       []string          `json:"ansibleTags"`       // --tags
		AnsibleSkipTags   []string          `json:"ansibleSkipTags"`   // --skip-tags
		AnsibleVerbosity  int               `json:"ansibleVerbosity"`  // 0–4 ⇒ -v … -vvvv
		AnsibleBecome     bool              `json:"ansibleBecome"`     // --become
		AnsibleBecomeUser string            `json:"ansibleBecomeUser"` // --become-user
		AnsibleExtraVars  map[string]string `json:"ansibleExtraVars"`  // -e k=v (argv-visible; never secrets)
		// T2.4 — run-input audit metadata from the Run dialog. Both are optional and
		// purely descriptive: they record HOW the operator arrived at this run, never
		// what it does. A caller that omits them (curl, a schedule, an older client)
		// behaves exactly as before.
		//
		// PromptAnswers maps a declared input name to where its value came from
		// ("typed" | "default" | "override" | "job env"). PromptAcknowledged is true
		// when the operator ticked "Run anyway" past an unfilled required input — the
		// difference between "someone was warned and proceeded" and "nobody looked",
		// which is the whole point for a run someone requested through a third party.
		PromptAnswers      map[string]string `json:"promptAnswers"`
		PromptAcknowledged bool              `json:"promptAcknowledged"`
		// RV — which of the Run dialog's sections the operator had open before
		// running (the visited-gating record). Audit-only like the prompt
		// metadata: it never changes what the run does, and its absence means
		// only that the caller wasn't the dialog (curl, an older client).
		ReviewedSections []string `json:"reviewedSections"`
		// V2-11 (narrowed to stored-reference ADDITIONS) — attach existing Env Vars
		// rows (Secrets / Variables / SSH Keys, by kind + bare name) to THIS run
		// only, on top of the job's/script's declared bindings. Names only: the
		// dispatch resolver (internal/runref) resolves them under the run's
		// scope/agency rules exactly like declared bindings — values never ride
		// the request or the envelope.
		//
		// RA-4 — `as` is the optional ALIAS: the bare destination name the value is
		// injected under, so a caller can supply THEIR department's credential to a
		// shared job body that reads a fixed AMADEUS_SECRET_<name>. It is a
		// destination only: `name` still selects the row, and it is still resolved
		// under this run's scope and agency snapshot, so an alias can never widen
		// what the caller may attach.
		References []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			As   string `json:"as"`
		} `json:"references"`
		// AR — "When to run": an optional RFC3339 instant deferring this run.
		// Everything else in the body is validated and frozen NOW; at runAt the
		// pending loop promotes the frozen trigger through the normal enqueue
		// seam. Empty ⇒ run immediately (the pre-AR behavior).
		RunAt string `json:"runAt"`
	}
	// Body is optional; tolerate an empty body but reject malformed JSON (F1).
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	// AR — validate the deferral first: everything below is identical for a
	// deferred run EXCEPT the cap/Forbid gates (judged at promotion instead —
	// "the system is busy now" says nothing about tomorrow 5pm).
	runAt, runAtErr := parseRunAt(body.RunAt)
	if runAtErr != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", runAtErr.Error())
		return
	}
	deferred := runAt != ""
	scope := body.Scope
	if scope == "" && jr.Scope != nil {
		scope = *jr.Scope
	}
	targetHost := body.TargetHost
	if targetHost == "" && jr.Host != nil {
		targetHost = *jr.Host
	}
	// TG-4 — the same metachar rule the TargetHosts subset enforces below, applied to
	// the single-host target. For an ansible run this name is passed verbatim to
	// `--limit`, where a pattern metacharacter is silently DROPPED — leaving no limit
	// and widening the run to the full inventory. Reject at the boundary so the
	// operator gets an actionable 422 instead of a run that dispatches and then dies
	// on a manifest 409. Applies to the per-run override and to a job's stored pin
	// alike: both arrive here, and both would land on runs.target_host.
	//
	// Skipped when a raw ansibleLimit is supplied, matching RunLimit's precedence:
	// the passthrough wins outright, the pin is never folded into --limit, so there
	// is nothing to widen. (The manifest's 409 backstop is scoped identically.)
	if jr.Type == "ansible" && targetHost != "" && !execspec.ValidName(targetHost) &&
		strings.TrimSpace(body.AnsibleLimit) == "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"host "+targetHost+" contains an ansible pattern character; use ansibleLimit for raw --limit patterns on ansible runs")
		return
	}

	// Scope-access guard (A5 / F-R, architecture-update.md §8): the write-side
	// mirror of the listRuns read filter, routed through the single-source predicate.
	// An unrestricted actor ("*") reaches every scope; otherwise the effective scope
	// — the job's bound scope, or the per-trigger override — must be granted.
	//
	// RB-26 (v0.56.4) replaced Q-F7 on THIS route. An empty effective scope used to
	// be allowed for everyone, which is the hole through which "scoped to Tax" stopped
	// being true: an unscoped job was triggerable by anyone, dispatched to the general
	// pool, and its declared target_host was validated against nothing. An unscoped
	// job now carries NO AUTHORITY OF ITS OWN — a restricted actor must bind a scope
	// they hold, and only an unrestricted actor may run it unbound.
	if scope == "" {
		// The unbound case. CanUnbound requires BOTH the verb and unrestricted reach,
		// so an unrestricted viewer is denied here too.
		if !id.CanUnbound(auth.PermTriggerJobs) {
			if s.auth != nil {
				// RF-9: the denial names the verb too — it fails for a viewer holding
				// no verb as well as for a restricted actor holding no "*" reach, and
				// the audit row should not make the first case look like the second.
				s.auth.AuditDenied(r, id.Email, "insufficient_scope", auth.AllScopes,
					auditDetails(r, "unscoped job triggered without a bound scope ("+auth.PermTriggerJobs+" required unbound)"))
			}
			// The message has to say what to DO. A bare "forbidden" on a job that
			// worked yesterday is the worst possible shape for a new rule.
			httpx.Fail(w, http.StatusForbidden, "scope_required",
				"this job has no scope of its own; choose a scope you have access to and run it against that")
			return
		}
	} else {
		if !auth.ScopeReadable(id, scope) {
			s.denyScope(w, r, scope, "you do not have access to scope "+scope)
			return
		}
		// RB-2 (FR-M1): the triggerJobs verb, enforced at last. It has been computed
		// and displayed since v1 and checked nowhere, so a viewer with scope access
		// could execute. Sited beside the scope guard deliberately — the two answer
		// different questions (may you SEE this scope / may you DO this verb on it).
		if !s.requireCan(w, r, id, auth.PermTriggerJobs, scope) {
			return
		}
	}

	// RB-27 — the load-bearing prerequisite for RB-26, without which the bind is
	// DECORATIVE. Per-run host subsets (targetHosts[]) are validated against the
	// scope's inventory below, but a job's DECLARED target_host never was: an
	// unscoped job pinned to finance-db-01 and bound to Tax would still reach the
	// Finance host, and the scope the actor was forced to name would mean nothing.
	//
	// Narrow by design: only when a RESTRICTED actor binds a scope to a job that had
	// none. A job with a declared scope had its host authored against that scope, so
	// validating it here would reject configurations that are already live; and an
	// unrestricted actor already reaches every host, so there is nothing to enforce.
	if scope != "" && (jr.Scope == nil || *jr.Scope == "") && targetHost != "" && !id.Unrestricted() {
		members, err := execspec.ScopeHosts(r.Context(), s.db, scope)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		inScope := slices.Contains(members, targetHost)
		if !inScope {
			// Same 422 and the same scope_membership code as the subset path, so the
			// error vocabulary stays uniform across both host-targeting routes.
			httpx.Fail(w, http.StatusUnprocessableEntity, "scope_membership",
				"this job targets host "+targetHost+", which is not a member of scope "+scope+
					"; binding it to that scope would send the run outside it")
			return
		}
	}

	// Per-run reference additions (V2-11, stored-reference additions only) —
	// validated and gated BEFORE any enqueue work. A reference is a grant over
	// stored secret/key material, so attaching one to a run needs the same
	// permission that editing a job's declared bindings does (ManageEnvVars,
	// bindings_mount.go) — otherwise any operator who may trigger a job could
	// route an in-scope secret into a one-off run its author never declared.
	// Additive only: the declared set cannot be shrunk from here. Resolution
	// stays at dispatch (fail-closed, same oracle-safe 409 as declared bindings);
	// the Run dialog pre-checks via POST /references/validate.
	// M3/T3.6 — the effective scope's agency SET. Computed HERE rather than at the
	// enqueue site because RF-4's per-reference check must resolve the same row the
	// dispatch resolver will (runref.lookupScoped takes the run's agencies), and
	// checking a different row than the one that gets injected is a check in name
	// only. Reused verbatim for the run snapshot below — one query, two consumers.
	runAgencies, _ := execspec.ScopeAgencies(r.Context(), s.db, scope)

	// RA-24 — refuse an UNBOUND run whose declared credentials resolve for nobody.
	//
	// An unbound run carries an empty agency snapshot, so an agency-OWNED row (which
	// has membership, and therefore intersects nothing) resolves for no department.
	// Left alone, this run reaches dispatch and dies with a 409 that reads as a
	// permissions problem, or — on a departmentalised fleet — is claimable by no
	// runner and queues forever. Both failures land far from the person who could
	// fix them, and the fix is one word: bind a scope.
	//
	// Precise rather than blunt (see runref/unbound.go): a job binding only GLOBAL
	// rows runs fine unbound and is NOT refused here. Only the bindings that would
	// actually fail are. And only when unbound — a bound run with a missing row keeps
	// today's behaviour, because that row is often created minutes later.
	if scope == "" {
		scriptRef := ""
		if jr.ScriptRef != nil {
			scriptRef = *jr.ScriptRef
		}
		owners := runref.RunOwners(jr.Source, jr.Name, jr.UID, scriptRef)
		blocked, berr := runref.UnboundRunBlocked(r.Context(), s.db, owners, scope, runAgencies)
		if berr != nil {
			httpx.Fail500(w, s.log, "db_error", berr)
			return
		}
		if len(blocked) > 0 {
			httpx.Fail(w, http.StatusUnprocessableEntity, "scope_required", runref.UnboundRefusal(blocked))
			return
		}
	}

	var runRefs []runref.Binding
	if len(body.References) > 0 {
		// RF-4: CanAnywhere is requirePerm's exact semantics through the RB-1 seam
		// (the flat legacy call this replaces predates the seam); the per-entity
		// departmental half (RB-32) is checked below, once each name is validated.
		if !id.CanAnywhere(auth.PermManageEnvVars) {
			if s.auth != nil {
				s.auth.AuditDenied(r, id.Email, "insufficient_permission", "manageEnvVars",
					auditDetails(r, "per-run reference additions need the Manage Env Vars permission"))
			}
			httpx.Fail(w, http.StatusForbidden, "forbidden",
				"adding references to a run requires the Manage Env Vars permission")
			return
		}
		// Same bound as a stored binding set (L9): more is a mistake or a probe.
		if len(body.References) > runref.MaxBindingsPerOwner {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_binding",
				fmt.Sprintf("too many references: %d exceeds the limit of %d", len(body.References), runref.MaxBindingsPerOwner))
			return
		}
		seenRef := map[string]bool{}
		for _, b := range body.References {
			// Mirror ReplaceBindings' rules exactly, through the SAME validator it
			// uses (runref.ValidateBinding): valid kind, the stricter reserved-KEK bar
			// for secret names, the base POSIX-identifier charset for vars/keys — and
			// the same charset applied to the alias, which mints an env key just as a
			// row name does.
			binding := runref.Binding{Kind: runref.Kind(b.Kind), Name: b.Name, As: strings.TrimSpace(b.As)}
			if verr := runref.ValidateBinding(binding); verr != nil {
				httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_binding", verr.Error())
				return
			}
			key := runref.DedupeKey(binding) // RA-4: the same row under two aliases is two bindings
			if seenRef[key] {
				continue
			}
			seenRef[key] = true
			runRefs = append(runRefs, binding)
		}
		// RA-Q2 — two additions landing on one injected key. Rejected here so the
		// operator gets a 422 naming both references at the boundary, rather than a
		// 409 at dispatch after the run is queued. Dispatch re-checks over the FULL
		// union (job + script + these), which is the collision this cannot see.
		if cerr := runref.CheckAliasCollisions(runRefs); cerr != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_binding", cerr.Error())
			return
		}
		// RB-32 (RF-4): the departmental half. Attaching a reference is a grant over
		// the ENTITY, so the actor must hold manageEnvVars on an agency that owns it
		// — the same rule the settings routes enforce — with RB-Q14 for unmembered
		// (global) entities: attaching what every department consumes is
		// unrestricted-only. Names that resolve to no row pass here and fail closed
		// at dispatch (M2 — this check must not become an existence oracle).
		for _, b := range runRefs {
			if !s.requireRefEntityAgency(w, r, id, b, scope, runAgencies) {
				return
			}
		}
	}

	// CA — per-run "connect as" identity, validated at the trigger boundary.
	// RP-6 — the gate is now IdentityCapableRunType, which admits ansible: the
	// override reaches ansible-playbook as connection extra-vars (RP-Q1), which
	// beat inventory-authored identity for every host in the run. terraform
	// still 422s — it authenticates through its providers, not SSH, so the
	// fields could only no-op there. The credential must exist NOW (fail fast
	// for the operator in the dialog); dispatch re-resolves the label and fails
	// the run loudly if it vanishes in between (CA-3).
	body.SSHUser = strings.TrimSpace(body.SSHUser)
	body.SSHCredential = strings.TrimSpace(body.SSHCredential)
	if body.SSHUser != "" || body.SSHCredential != "" {
		if !execspec.IdentityCapableRunType(jr.Type) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"sshUser/sshCredential cannot be applied to "+jr.Type+" runs; terraform authenticates through its providers rather than SSH")
			return
		}
		if body.SSHUser != "" && !execspec.ValidSSHUser(body.SSHUser) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"sshUser must be a plain username (letters, digits, '.', '-', '_'; starting with a letter or '_'; at most 32 chars)")
			return
		}
		if body.SSHCredential != "" {
			// CA-Q1 — selecting a stored key for a run is a grant over stored key
			// material, so it needs the same permission as per-run reference
			// additions (V2-11, above). RF-4: CanAnywhere via the RB-1 seam.
			if !id.CanAnywhere(auth.PermManageEnvVars) {
				if s.auth != nil {
					s.auth.AuditDenied(r, id.Email, "insufficient_permission", "manageEnvVars",
						auditDetails(r, "selecting an SSH credential for a run needs the Manage Env Vars permission"))
				}
				httpx.Fail(w, http.StatusForbidden, "forbidden",
					"selecting an SSH credential for a run requires the Manage Env Vars permission")
				return
			}
			credID, found, err := sshkeys.IDByLabel(r.Context(), s.db, body.SSHCredential)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if !found {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"no SSH credential with label "+body.SSHCredential)
				return
			}
			// RB-32 (RF-4): departmental half — the key's owning agency, RB-Q14 for
			// unmembered keys. This route already discloses label existence above, so
			// unlike the reference loop there is no oracle to protect.
			if !s.requireEntityAgency(w, r, id, auth.PermManageEnvVars,
				"ssh_credential_agencies", "credential_id", credID, "SSH key") {
				return
			}
		}
	}

	// CA-10 — fold the job-spec identity beneath the operator's override
	// (per-field, override wins). Gated by the same IdentityCapableRunType
	// predicate as the validation above (RP-7): a stored identity on a terraform
	// job (possible via Git sync, which warns rather than blocks) is ignored
	// here, mirroring the scheduler and workflow producers. The F3 envelope
	// keeps recording ONLY the operator's values — the job-spec layer is
	// definition, not override.
	effSSHUser, effSSHCred := body.SSHUser, body.SSHCredential
	if execspec.IdentityCapableRunType(jr.Type) {
		if effSSHUser == "" && jr.SSHUser != nil {
			effSSHUser = *jr.SSHUser
		}
		if effSSHCred == "" && jr.SSHCredential != nil {
			effSSHCred = *jr.SSHCredential
		}
	}

	// F2 — a per-run host subset must be a subset of the bound scope's membership
	// (architecture-update.md §5.3). Validate at the trigger boundary against the
	// shared scope_hosts source (never ssh_hosts directly); a non-member is a hard
	// 422. A subset supersedes the single-host override, so clear targetHost to take
	// the subset path in ResolveTargets. Free-form open-host targeting is V2.
	if len(body.TargetHosts) > 0 {
		if scope == "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "scope_membership", "targetHosts requires a scope to validate membership against")
			return
		}
		members, err := execspec.ScopeHosts(r.Context(), s.db, scope)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		memberSet := make(map[string]bool, len(members))
		for _, m := range members {
			memberSet[m] = true
		}
		for _, h := range body.TargetHosts {
			if !memberSet[h] {
				httpx.Fail(w, http.StatusUnprocessableEntity, "scope_membership", "host "+h+" is not a member of scope "+scope)
				return
			}
			// For ansible runs the host name is passed verbatim to `--limit`; an
			// ansible pattern metacharacter (range like web[01:50], IPv6 ':' etc.)
			// would be silently dropped from --limit — broadening the run to the full
			// inventory and desyncing it from the SSH host set. Reject it LOUDLY;
			// operators use ansibleLimit for raw --limit patterns.
			if jr.Type == "ansible" && !execspec.ValidName(h) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "scope_membership",
					"host "+h+" contains an ansible pattern character; use ansibleLimit for raw --limit patterns on ansible runs")
				return
			}
		}
		targetHost = "" // the subset path supersedes the single-host override
	}

	// M3 — group targeting (§8.2). Each target group must be a real group in the
	// scope's PARSED projection. If the projection degraded (could not enumerate
	// members) we REJECT all group targeting, because SSH expands members from the
	// DB and would otherwise dial a different set than ansible's --limit — the
	// two-executor drift execspec forbids. A group subset supersedes targetHost.
	if len(body.TargetGroups) > 0 {
		if scope == "" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "group_membership", "targetGroups requires a scope to validate membership against")
			return
		}
		var projStatus sql.NullString
		_ = s.db.QueryRowContext(r.Context(), `SELECT projection_status FROM scopes WHERE name=?`, scope).Scan(&projStatus)
		if projStatus.Valid && projStatus.String != "" && projStatus.String != "ok" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "group_membership",
				"group targeting unavailable: the inventory projection for scope "+scope+" could not be fully parsed")
			return
		}
		groups, err := execspec.ScopeGroups(r.Context(), s.db, scope)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		groupSet := make(map[string]bool, len(groups))
		for _, g := range groups {
			groupSet[g] = true
		}
		for _, g := range body.TargetGroups {
			if !groupSet[g] {
				httpx.Fail(w, http.StatusUnprocessableEntity, "group_membership", "group "+g+" is not a group in scope "+scope)
				return
			}
		}
		targetHost = "" // group subset supersedes the single-host override
	}

	// R5.1/R5.2 — resolve the executor with precedence
	//   per-trigger override > job spec.executor > global default > capability
	// and reject invalid combinations (ssh + ansible/terraform). With no capable
	// runner registered, an ansible/terraform run still enqueues and sits queued
	// (A6.3 waiting-for-capable-runner) — it is no longer a 422 (R4.3).
	jobSrc := s.defSource(r, "jobs", jobID)
	executor, execErr := resolveExecutor(r.Context(), s.db, jobSrc, jr.Name, jr.Type, body.Executor)
	if execErr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_executor", execErr)
		return
	}

	// RT-2 — resolve the runner pin through the same four-rung precedence for
	// every trigger kind, then enforce RT-Q5.
	runnerTag, tagErr := resolveRunnerTag(r.Context(), s.db, jobSrc, jr.Name, body.RunnerTag)
	if tagErr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_runner_tag", tagErr)
		return
	}
	// The one hard rejection in the band. It tests the RESOLVED executor, never the
	// requested one: a job with executor unset resolves through resolveExecutor's
	// default chain and can land on 'ssh', where the in-process pool claims it and
	// no runner — hence no tag — is ever involved. Silently ignoring the pin there
	// would run the job from the control plane while the operator believed they had
	// confined it to a network segment, which is the failure this whole band exists
	// to prevent. sshexec's claim query deliberately has no pin predicate; this is
	// what keeps a pinned run from ever reaching it.
	if runnerTag != "" && executor == "ssh" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_runner_tag",
			"pinning a runner requires the runner executor; this run resolved to ssh")
		return
	}
	// KB — the same rule for a bound SSH key: the ssh executor connects FROM
	// amadeus and cannot place a key file on the target, so a key-bound run that
	// resolves to ssh is refused here rather than started with an input it will
	// never receive (the executor used to warn and skip). Tests the RESOLVED
	// executor for the reason the block above gives; serves the token trigger
	// too, since it rides this handler.
	{
		scriptRef := ""
		if jr.ScriptRef != nil {
			scriptRef = *jr.ScriptRef
		}
		keys, kerr := runref.KeyBindingsOnSSH(r.Context(), s.db,
			runref.RunOwners(jr.Source, jr.Name, jr.UID, scriptRef), executor)
		if kerr != nil {
			httpx.Fail500(w, s.log, "db_error", kerr)
			return
		}
		if len(keys) > 0 {
			httpx.Fail(w, http.StatusUnprocessableEntity, runref.CodeKeyBindingOnSSH, runref.KeyBindingRefusal(keys))
			return
		}
	}

	// M3 — ansibleLimit is a RAW --limit passthrough for ANSIBLE runs only (the
	// escape hatch for patterns the projection can't model, OD-15). It is honored
	// solely by the ansible toolchain (exec_local appends --limit only for run-type
	// ansible); any other run-type would silently swallow it, so gate on run-type,
	// not just the executor — ansible always runs on a runner. It is exec-safe (each
	// --limit value is a single argv element, no shell), so its content is the
	// operator's responsibility and is not charset-validated.
	if strings.TrimSpace(body.AnsibleLimit) != "" && jr.Type != "ansible" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_executor",
			"ansibleLimit only applies to ansible runs")
		return
	}

	// Phase 3 (RP-15) — advanced ansible options. Same run-type gate and the same
	// reason: only the ansible toolchain consumes them, so any other run-type
	// would accept the field and silently drop it.
	ansOpts := execspec.AnsibleOpts{
		Check:      body.AnsibleCheck,
		Diff:       body.AnsibleDiff,
		Tags:       trimNonEmpty(body.AnsibleTags),
		SkipTags:   trimNonEmpty(body.AnsibleSkipTags),
		Verbosity:  body.AnsibleVerbosity,
		Become:     body.AnsibleBecome,
		BecomeUser: strings.TrimSpace(body.AnsibleBecomeUser),
		ExtraVars:  body.AnsibleExtraVars,
	}
	if ansOpts.Any() {
		if jr.Type != "ansible" {
			httpx.Fail(w, http.StatusUnprocessableEntity, "invalid_executor",
				"the advanced ansible options (check/diff/tags/verbosity/become/extra-vars) only apply to ansible runs")
			return
		}
		// Tags become argv elements in a comma-joined list, so a comma or a
		// pattern metachar inside one would silently re-partition the selection —
		// the same class of bug ValidName guards for host/group names.
		for _, t := range append(append([]string{}, ansOpts.Tags...), ansOpts.SkipTags...) {
			if !execspec.ValidName(t) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"ansible tag "+t+" contains a character that would re-split the tag list; use plain tag names")
				return
			}
		}
		if ansOpts.Verbosity < 0 || ansOpts.Verbosity > 4 {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"ansibleVerbosity must be 0–4 (-v … -vvvv)")
			return
		}
		if ansOpts.BecomeUser != "" && !execspec.ValidSSHUser(ansOpts.BecomeUser) {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
				"ansibleBecomeUser must be a plain username (letters, digits, '.', '-', '_'; starting with a letter or '_'; at most 32 chars)")
			return
		}
		for k := range ansOpts.ExtraVars {
			// The two identity vars are owned by "connect as". `-e` is the same
			// precedence tier and the LAST occurrence wins, so accepting them here
			// would let a run connect as someone other than what its own audit
			// record and the console claim. (The agent also emits identity last —
			// this refusal is the belt to that braces, and it gives the operator an
			// actionable message instead of a silently-ignored value.)
			if execspec.ReservedAnsibleVar(k) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"extra-var "+k+" is set by the run's Connect as identity; use that field rather than an extra-var")
				return
			}
			if !execspec.ValidEnvName(k) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
					"extra-var name "+k+" is not a valid variable name (letters, digits and '_', not starting with a digit)")
				return
			}
		}
	}

	// FX-C — the admission gates this path was missing, applied through the shared
	// origin policy (scheduler.Evaluate) rather than re-derived here.
	//
	// Two were absent. PAUSE was never checked at all on the job trigger, while
	// the workflow twin did check it — and the machine trigger route delegates to
	// this same handler, so a service account could start a job an operator had
	// explicitly paused. FX-Q1 settles the split: a person clicking Run overrides
	// a pause (the UI confirms first, naming it), a token does not. The global
	// CALENDAR freeze was absent for both: a change freeze that a click walks
	// through is not a freeze (FX-Q2).
	//
	// Deferred runs are exempt from the cap here and judged at promotion — "the
	// system is busy now" says nothing about tomorrow at 5pm — but pause and the
	// freeze are evaluated at BOTH ends, because a run parked while everything was
	// clear must still be stopped if a freeze is declared before it comes due.
	origin := scheduler.OriginManual
	if triggerKind == triggerKindAPI {
		origin = scheduler.OriginToken
	}
	if g := scheduler.Evaluate(r.Context(), s.db, s.log, scheduler.GateRequest{
		Origin: origin, Source: jobSrc, OwnerKind: "job", Name: jr.Name,
	}, s.appLocation()); !g.Allowed {
		// The cap keeps its own status code and message: integrations already
		// distinguish it, and it is transient where the other two are decisions.
		switch g.Gate {
		case "cap", "calendar":
			// Both are TRANSIENT and both are re-judged at the promotion instant, so
			// neither may refuse a run being scheduled for later. Refusing here would
			// mean an operator cannot park the post-freeze cutover run while the
			// freeze is on — the one moment they most need to — even though the
			// promotion gate already guarantees it is re-checked on the day
			// (FX-Q2: "promotion re-evaluates at the promotion instant").
			if !deferred {
				if g.Gate == "cap" {
					httpx.Fail(w, http.StatusConflict, "concurrency_cap", "global concurrency cap reached")
				} else {
					httpx.Fail(w, http.StatusConflict, "suppressed_by_calendar", g.Reason)
				}
				return
			}
		case "pause":
			httpx.Fail(w, http.StatusConflict, "job_paused",
				"this job is paused; resume it before triggering it from the API")
			return
		default:
			// disabled, or any gate added later: refuse, deferred or not. These are
			// catalog facts rather than moments in time, so parking does not help.
			httpx.Fail(w, http.StatusConflict, "run_refused", g.Reason)
			return
		}
	}

	// S16: Forbid check.
	// PP-L8: concKey is only set for Forbid-policy jobs so the partial unique index
	// (uq_runs_active_concurrency) fires only on Forbid runs, not Allow/Replace ones.
	// The key and policy are computed for deferred runs too — they ride the frozen
	// params so promotion can re-judge Forbid — but the 409 applies only NOW.
	var concKey string
	var concPolicy, dbConcKey, dbUID sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `SELECT concurrency_policy, concurrency_key, uid FROM jobs WHERE rowid = ?`, jobID).Scan(&concPolicy, &dbConcKey, &dbUID)
	queueOnConflict := false
	if concPolicy.Valid && cronutil.HoldsGate(concPolicy.String) {
		concKey = cronutil.ConcurrencyKey(dbConcKey.String, dbUID.String, jobSrc, jr.Name)
		if !deferred {
			conflict, err := scheduler.CheckForbid(r.Context(), s.db, concKey)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if conflict {
				// QP: Forbid refuses the trigger; Queue accepts it and parks it.
				// The response differs accordingly — a 409 for "this will not
				// happen", a 202 for "this will happen later" — because an
				// integration that retries on 409 must not retry on a queued run.
				if concPolicy.String != cronutil.PolicyQueue {
					httpx.Fail(w, http.StatusConflict, "concurrency_conflict",
						"a run for this concurrency key is already active (Forbid policy)")
					return
				}
				queueOnConflict = true
			}
		}
	}

	// F1/F3 — assemble the per-run env to inject and the override envelope to
	// persist (architecture-update.md §4/§6). A manual run carries no schedule env,
	// so the override env IS the run env (env_json); the envelope records only what
	// the operator actually overrode, for audit/reproducibility. Plaintext only
	// (Q-F1); the executor's env injector re-validates key syntax.
	// Job-level env (JC10/JC11) is the BASE; the per-run override wins per key
	// (Q-JC9). A manual run has no schedule layer, so this merge is the whole
	// effective env_json. The override_json envelope below still records ONLY the
	// operator's override — job-env is not added there, so it stays un-redacted like
	// schedule env (JC12). NULL job-env + no override ⇒ nil ⇒ env_json NULL (R2).
	// Reserved-namespace guard (W4, N-D1): a per-run override may not define any
	// AMADEUS_* key. Enforced at ingest, BEFORE the merge, so a reserved key can
	// never reach env_json.
	if err := envref.ValidateOperatorEnv(body.Env); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	eff := envmerge.Merge(jr.Env, body.Env)
	var envJSON string
	if eff != nil {
		b, _ := json.Marshal(eff)
		envJSON = string(b)
	}
	override := map[string]any{}
	if len(body.Env) > 0 {
		override["env"] = body.Env
	}
	if body.Scope != "" {
		override["scope"] = body.Scope
	}
	if body.Executor != "" {
		override["executor"] = body.Executor
	}
	// RT-2 — record the per-trigger pin override, including the empty string. ""
	// here is not "nothing to record": it is the operator explicitly running a
	// pinned job unpinned, which is exactly the decision an audit of a run wants
	// to see. Only a nil (absent) field writes nothing, keeping an ordinary run's
	// envelope byte-identical to a pre-RT-2 one.
	if body.RunnerTag != nil {
		override["runnerTag"] = *body.RunnerTag
	}
	if len(body.TargetHosts) > 0 {
		override["hosts"] = body.TargetHosts
	}
	if len(body.TargetGroups) > 0 {
		override["groups"] = body.TargetGroups
	}
	if strings.TrimSpace(body.AnsibleLimit) != "" {
		override["ansibleLimit"] = body.AnsibleLimit
	}
	// Phase 3 (RP-15) — one envelope key per SET option, under the same names as
	// execspec.AnsibleOpts' json tags so the writer here and the reader at
	// dispatch cannot drift. Unset options write nothing, keeping a plain run's
	// envelope byte-identical to a pre-Phase-3 one.
	if ansOpts.Check {
		override["ansibleCheck"] = true
	}
	if ansOpts.Diff {
		override["ansibleDiff"] = true
	}
	if len(ansOpts.Tags) > 0 {
		override["ansibleTags"] = ansOpts.Tags
	}
	if len(ansOpts.SkipTags) > 0 {
		override["ansibleSkipTags"] = ansOpts.SkipTags
	}
	if ansOpts.Verbosity > 0 {
		override["ansibleVerbosity"] = ansOpts.Verbosity
	}
	if ansOpts.Become {
		override["ansibleBecome"] = true
	}
	if ansOpts.BecomeUser != "" {
		override["ansibleBecomeUser"] = ansOpts.BecomeUser
	}
	if len(ansOpts.ExtraVars) > 0 {
		override["ansibleExtraVars"] = ansOpts.ExtraVars
	}
	// CA — names only (JC12): the username and the credential LABEL, never material.
	if body.SSHUser != "" {
		override["sshUser"] = body.SSHUser
	}
	if body.SSHCredential != "" {
		override["sshCredential"] = body.SSHCredential
	}
	// V2-11 — record the per-run reference ADDITIONS, names only (the JC12 rule:
	// the envelope is un-redacted audit metadata, so a value must never enter it).
	// Dispatch reads this back via runref.OverrideBindings and unions it with the
	// declared binding set.
	if len(runRefs) > 0 {
		refs := make([]map[string]string, 0, len(runRefs))
		for _, b := range runRefs {
			// RA-7 — BOTH names, never just the destination. With aliasing several
			// departments' rows land on one key, so an envelope recording only `as`
			// could not answer whose credential this run was given.
			entry := map[string]string{"kind": string(b.Kind), "name": b.Name}
			if b.As != "" {
				entry["as"] = b.As
			}
			refs = append(refs, entry)
		}
		override["references"] = refs
	}
	// UDV4/UDV6 — warn-only declared-prompt check. A required prompt whose key is
	// absent (or blank) in the effective merged env (job-env base + per-run answers)
	// is recorded in the override envelope under "promptWarnings" so History can show
	// the run proceeded with a required variable unfilled. For a `warn` job (the
	// default, and every job that predates Phase 3) this NEVER blocks the run — UDV4
	// is preserved exactly for anything that hasn't opted in.
	//
	// JR-Q1 — a scope Env Vars row of the same bare name does NOT satisfy a prompt and
	// must not suppress the warning. Env Vars reach a run only via an explicit reference
	// binding, under the derived AMADEUS_VAR_<name> key (internal/runref); nothing
	// publishes a bare `NAME` into the child env. `eff` is therefore the whole truth
	// here, and the Run dialog's satisfied-check mirrors it exactly. (The dialog used to
	// claim scope-level satisfaction and contradicted this record — fixed 2026-07-24.)
	if len(jr.Prompts) > 0 {
		var unfilled []string
		for _, p := range jr.Prompts {
			if !p.Required {
				continue
			}
			if v, ok := eff[p.Name]; !ok || strings.TrimSpace(v) == "" {
				unfilled = append(unfilled, p.Name)
			}
		}
		if len(unfilled) > 0 {
			override["promptWarnings"] = unfilled
			// JR-Q5/JR-Q10 — opt-in hard enforcement. A `block` job refuses the run
			// while a declared required input has no value, and refuses it for EVERY
			// caller: the Run dialog, curl, a schedule, a workflow step. That reach is
			// the entire reason the flag exists — no client-side gate can cover those
			// paths.
			//
			// Deliberately NOT escapable by body.PromptAcknowledged. An escapable block
			// is just `warn` with extra steps, and an admin who opts a job in is saying
			// this run must be impossible without a correct value — not "ask twice".
			// The Run dialog hides its "Run anyway" affordance for these jobs rather
			// than offering an escape the server will reject.
			if gitlab.NormalizePromptEnforcement(jr.PromptEnforcement) == gitlab.EnforceBlock {
				httpx.Fail(w, http.StatusUnprocessableEntity, "prompt_required",
					"required run input(s) have no value: "+strings.Join(unfilled, ", "))
				return
			}
		}
	}
	// T2.4 — the operator-supplied audit metadata, recorded verbatim alongside the
	// server-derived warnings above. Kept separate from promptWarnings on purpose:
	// promptWarnings is what the SERVER observed about the effective env and cannot be
	// spoofed away by a client, while these two describe the operator's intent. Only
	// keys the job actually declares are retained, so a caller can't stuff the envelope.
	if len(body.PromptAnswers) > 0 && len(jr.Prompts) > 0 {
		declared := make(map[string]bool, len(jr.Prompts))
		for _, p := range jr.Prompts {
			declared[p.Name] = true
		}
		answers := map[string]string{}
		for name, src := range body.PromptAnswers {
			if declared[name] {
				answers[name] = src
			}
		}
		if len(answers) > 0 {
			override["promptAnswers"] = answers
		}
	}
	// RV — audit-only, filtered to the dialog's known section names so the
	// envelope can't accumulate caller-invented keys (it is displayed verbatim).
	if len(body.ReviewedSections) > 0 {
		// RU-8 — "when-to-run" is additive: the section has existed since AR but
		// was never recorded, so a run that kept the default timing looked the
		// same as one where nobody considered it. Older clients simply never
		// send it, and runs stored before this list grew display verbatim.
		known := map[string]bool{"variables": true, "where-it-runs": true, "targets": true, "method": true, "advanced": true, "when-to-run": true, "confirmation": true}
		var reviewed []string
		for _, sec := range body.ReviewedSections {
			if known[sec] {
				reviewed = append(reviewed, sec)
			}
		}
		if len(reviewed) > 0 {
			override["reviewedSections"] = reviewed
		}
	}
	if body.PromptAcknowledged {
		override["promptAcknowledged"] = true
	}
	// AR — provenance for a deferred run: the audit trail distinguishes
	// "clicked at 5pm" from "scheduled at 10am for 5pm". Names and instants
	// only, per the envelope's un-redacted rule.
	if deferred {
		override["scheduledFor"] = runAt
		override["scheduledBy"] = id.Email
	}
	var overrideJSON string
	if len(override) > 0 {
		b, _ := json.Marshal(override)
		overrideJSON = string(b)
	}

	// M3/T3.6 — the effective scope's agency SET is snapshotted onto the run. This is
	// what claimRun intersects against runner membership (hard-isolation dispatch),
	// and it is frozen: re-homing the scope later cannot retarget a queued run. It is
	// resolved once, above, where RF-4's reference check also needs it.
	params := scheduler.EnqueueParams{
		JobName:        jr.Name,
		JobSource:      jobSrc,
		RunType:        jr.Type,
		Scope:          scope,
		TargetHost:     targetHost,
		TriggerKind:    triggerKind,
		TriggeredBy:    id.Email,
		ConcurrencyKey: concKey,
		Executor:       executor,
		EnvJSON:        envJSON,
		OverrideJSON:   overrideJSON,
		SSHUser:        effSSHUser,
		SSHCredential:  effSSHCred,
		AgenciesJSON:   execspec.MarshalAgencies(runAgencies),
		Priority:       body.Priority,
		// RT-2 — the manual trigger is the one producer with a per-run rung, so it
		// always speaks explicitly, even to say "unpinned". &runnerTag, never
		// nil: passing nil here would make the enqueue re-resolve the job's pin
		// and quietly undo an operator's per-run unpin.
		RunnerTag: &runnerTag,
	}

	// AR — park a deferred run instead of enqueueing it. The frozen params ARE
	// this fully-validated trigger; the pending loop replays them through the
	// same enqueue seam at runAt. Policy rides along so promotion can re-judge
	// Forbid without a jobs-table join.
	if deferred {
		if concPolicy.Valid {
			params.Policy = concPolicy.String
		}
		pendingID, err := scheduler.InsertPendingRun(r.Context(), s.db, "job", jr.Name, jobSrc, scope, runAt, id.Email, &params)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Scheduled", jr.Name, "Ad-hoc run scheduled for "+runAt)
		httpx.JSON(w, http.StatusAccepted, map[string]any{
			"pending":     true,
			"id":          pendingID,
			"jobName":     jr.Name,
			"jobSource":   jobSrc,
			"runAt":       runAt,
			"scheduledBy": id.Email,
		})
		return
	}

	// QP: the gate was held and the policy says wait. Park the fully-validated
	// trigger — every check above has already run — and answer 202 with where it
	// sits in line, so the caller can tell "later" from "no".
	if queueOnConflict {
		params.Policy = cronutil.PolicyQueue
		queued, qerr := scheduler.TryQueue(r.Context(), s.db, params)
		if qerr != nil {
			httpx.Fail500(w, s.log, "db_error", qerr)
			return
		}
		if !queued {
			httpx.Fail(w, http.StatusConflict, "queue_full",
				"the concurrency queue for this job is full (cap "+strconv.Itoa(scheduler.QueueCap)+")")
			return
		}
		_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Queued", jr.Name,
			"Run queued behind an active run")
		httpx.JSON(w, http.StatusAccepted, map[string]any{
			"queued":         true,
			"jobName":        jr.Name,
			"jobSource":      jobSrc,
			"concurrencyKey": concKey,
			"queueDepth":     scheduler.QueueDepth(r.Context(), s.db, concKey),
			"queuedBy":       id.Email,
		})
		return
	}

	traceID, err := scheduler.EnqueueRunWithID(r.Context(), s.db, params)
	if err != nil {
		// PP-L8: partial unique index catches a concurrent Forbid-policy race that
		// slipped past the explicit check above. Surface it as a 409 Conflict.
		if scheduler.IsForbidConflict(err) {
			httpx.Fail(w, http.StatusConflict, "concurrency_conflict", "a run for this concurrency key is already active (Forbid policy)")
			return
		}
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// Write change log. The sentence names the provenance so the audit trail
	// distinguishes a click from a service-account POST without a join.
	triggerNote := "Manual run triggered"
	if triggerKind == triggerKindAPI {
		triggerNote = "Run triggered via the API by " + id.Email
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Triggered", jr.Name, triggerNote)

	run := s.fetchRunByID(r, traceID)
	httpx.JSON(w, http.StatusAccepted, run)
}

func (s *Server) pauseJob(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	jobID := r.PathValue("jobId")
	jr := s.fetchJobByID(r, jobID)
	if jr == nil || jr.DeletedAt != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}

	// SU-2: pause returns the full job detail (script body + plaintext env) AND
	// mutates the job — gate it on the owner scope, 404 out-of-scope (no existence
	// oracle, matching getJob). Without this a restricted actor could exfiltrate an
	// out-of-scope job's detail (and pause it) via this route, bypassing getJob.
	jobScope := ""
	if jr.Scope != nil {
		jobScope = *jr.Scope
	}
	if !auth.ScopeReadable(id, jobScope) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	// RB-2: pausing suppresses a job's scheduled runs, so it is a run-suppression
	// action and takes killJobs — the same verb as stopping one that is already
	// going. The 404 above and the 403 here are deliberately different: 404 hides
	// the existence of an out-of-scope job (no oracle), while a caller who can
	// already SEE this job learns plainly that they lack the verb.
	if !s.requireCan(w, r, id, auth.PermKillJobs, jobScope) {
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)
	src := s.defSource(r, "jobs", jobID)
	_, err := s.db.ExecContext(r.Context(), `
		INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid)
		VALUES (?, 'job', ?, ?, ?, NULLIF(?, ''))
		ON CONFLICT(owner_kind, owner_uid) DO UPDATE SET paused_by=excluded.paused_by, paused_at=excluded.paused_at
	`, src, jr.Name, id.Email, now, jr.UID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Paused", jr.Name, "Scheduled runs paused")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: id.Email, Category: "Jobs", Action: "Paused", Target: jr.Name,
	})

	jr.Status = "paused"
	httpx.JSON(w, http.StatusOK, jr)
}

func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	jobID := r.PathValue("jobId")
	jr := s.fetchJobByID(r, jobID)
	if jr == nil || jr.DeletedAt != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}

	// SU-2: resume returns the full job detail (script body + plaintext env) AND
	// mutates the job — gate on the owner scope, 404 out-of-scope (matching getJob).
	jobScope := ""
	if jr.Scope != nil {
		jobScope = *jr.Scope
	}
	if !auth.ScopeReadable(id, jobScope) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	// RB-2: pausing suppresses a job's scheduled runs, so it is a run-suppression
	// action and takes killJobs — the same verb as stopping one that is already
	// going. The 404 above and the 403 here are deliberately different: 404 hides
	// the existence of an out-of-scope job (no oracle), while a caller who can
	// already SEE this job learns plainly that they lack the verb.
	if !s.requireCan(w, r, id, auth.PermKillJobs, jobScope) {
		return
	}

	src := s.defSource(r, "jobs", jobID)
	_, err := s.db.ExecContext(r.Context(),
		`DELETE FROM paused_jobs WHERE owner_kind = 'job' AND (owner_uid = ? OR (owner_uid IS NULL AND source = ? AND name = ?))`, jr.UID, src, jr.Name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Resumed", jr.Name, "Scheduled runs resumed")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: id.Email, Category: "Jobs", Action: "Resumed", Target: jr.Name,
	})

	jr.Status = s.deriveJobStatus(r, src, jr.Name, 1, func() string {
		if jr.Schedule != nil {
			return *jr.Schedule
		}
		return ""
	}())
	httpx.JSON(w, http.StatusOK, jr)
}

func (s *Server) killJob(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	jobID := r.PathValue("jobId")
	jr := s.fetchJobByID(r, jobID)
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}

	// RX-4 — the disposition. Stopping a run and saying what it MEANT are two
	// different acts: `status` becomes the outcome, `killed_by` stays the proof a
	// human ended it (RX-5). Default 'killed' — the unclassified stop — so a
	// body-less call records the same status and the same activity outcome it
	// always did. All four values are already legal `runs.status` entries, so
	// this needs no migration.
	var body struct {
		Outcome string `json:"outcome"`
	}
	// Body is optional; tolerate an empty body but reject malformed JSON (F1).
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	disposition := body.Outcome
	if disposition == "" {
		disposition = "killed"
	}
	switch disposition {
	case "killed", "failure", "success", "warning":
	default:
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed",
			"outcome must be one of killed, failure, success, warning")
		return
	}

	// Find the active run FIRST — the authorization target is the RUN, not the job.
	//
	// RB-2: a run carries its own frozen scope (runs.scope), which can differ from
	// its job's: runJob accepts a per-trigger scope override, and under RB-26 an
	// unscoped job is deliberately bound to one at trigger time. Authorizing on
	// jobs.scope would let an actor who holds the job's bound scope terminate a run
	// that was launched into a DIFFERENT one — and after RB-26 it would be worse,
	// because an unscoped job's runs each carry the scope its triggerer chose while
	// the job itself carries none.
	var traceID, currentStatus string
	var runScopeRaw, startedAtRaw, runnerNameRaw sql.NullString
	// AA-2: the runner's DISPLAY name rides along so the run-end row this
	// handler writes carries `runner_name` like every other run-end row. A
	// stopped run is still a run that ran somewhere, and without this the
	// AA filter's "everything runner X ran" would silently exclude exactly
	// the runs an operator intervened in. LEFT JOIN: an SSH-executor run has
	// no runner_id, and a queued run has not been claimed yet — both stay NULL.
	err := s.db.QueryRowContext(r.Context(), `
		SELECT r.id, r.status, r.scope, r.started_at, rn.name
		FROM runs r LEFT JOIN runners rn ON rn.id = r.runner_id
		WHERE r.job_source = ? AND r.job_name = ? AND r.status IN ('queued','running')
		ORDER BY r.created_at DESC LIMIT 1
	`, jr.Source, jr.Name).Scan(&traceID, &currentStatus, &runScopeRaw, &startedAtRaw, &runnerNameRaw)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusConflict, "no_active_run", "job has no active run")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	runScope := runScopeRaw.String

	// PP-L2 scope guard + RB-2 verb guard, both against the RUN's effective scope.
	// A genuinely unbound run (system-global, or a scheduled fire of an unscoped job
	// — RB-Q11(c)) stays unrestricted-only, mirroring the trigger side.
	if runScope == "" {
		if !id.CanUnbound(auth.PermKillJobs) {
			if s.auth != nil {
				s.auth.AuditDenied(r, id.Email, "insufficient_scope", auth.AllScopes,
					auditDetails(r, "kill of an unbound run ("+auth.PermKillJobs+" required unbound)"))
			}
			httpx.Fail(w, http.StatusForbidden, "forbidden",
				"this run is not bound to a scope; only an unrestricted operator may stop it")
			return
		}
	} else {
		if !auth.ScopeReadable(id, runScope) {
			s.denyScope(w, r, runScope, "you do not have access to scope "+runScope)
			return
		}
		if !s.requireCan(w, r, id, auth.PermKillJobs, runScope) {
			return
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Reconcile to terminal, with the operator's disposition as the status.
	//
	// RX-6 — the write is status-guarded and checks RowsAffected, mirroring the
	// workflow finalize guard (engine.go). Without it, a run that reached a
	// terminal state between the SELECT above and this write was silently
	// overwritten as killed — a live bug, and one the disposition makes worse
	// (it would rewrite a real outcome as an asserted one). A run that finished
	// under us is not an error the operator caused, but it IS a different world
	// than the one they clicked in, so it 409s rather than reporting success.
	//
	// `exit_code` is deliberately untouched: the kill is only QUEUED below via
	// action_queue, so the process may still be alive. A disposition is an
	// assertion about intent, not an observation of an exit — synthesising an
	// exit 0 for a Success disposition would be a lie. The UI reads
	// "Success · stopped by operator@…", which is honest about both halves.
	// duration_ms is computed here rather than left NULL. Before RX-6 a stopped
	// run sometimes acquired a duration by accident — the runner's late final log
	// would overwrite the row, supplying one along with the status it was
	// clobbering. The guard correctly ends that, so the duration has to be
	// written on purpose or History shows a blank Duration for every stopped run.
	// A queued run that never started stays NULL: it has no elapsed time.
	var durationVal any
	if startedAtRaw.Valid && startedAtRaw.String != "" {
		if started, perr := time.Parse(time.RFC3339, startedAtRaw.String); perr == nil {
			if d := time.Now().UTC().Sub(started).Milliseconds(); d > 0 {
				durationVal = d
			}
		}
	}

	res, err := s.db.ExecContext(r.Context(), `
		UPDATE runs SET status = ?, killed_by = ?, completed_at = ?,
		    duration_ms = COALESCE(duration_ms, ?)
		WHERE id = ? AND status IN ('queued','running')
	`, disposition, id.Email, now, durationVal, traceID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "already_terminal",
			"this run finished before it could be stopped")
		return
	}

	// Queue kill signal for B4 runner poll delivery.
	_, _ = s.db.ExecContext(r.Context(), `
		INSERT INTO action_queue (run_id, op, created_at) VALUES (?, 'kill', ?)
	`, traceID, now)

	// Emit run-end activity with killedBy (S6 — actor from session). RX-7: the
	// outcome follows the disposition rather than being hardcoded 'failure' —
	// History and the activity trail must agree with the status that was written.
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind:       "run-end",
		Outcome:    dispositionOutcome(disposition),
		Actor:      id.Email,
		RunnerName: runnerNameRaw.String,
		JobName:    jr.Name,
		JobID:      jr.ID,
		TraceID:    traceID,
		Scope: func() string {
			if jr.Scope != nil {
				return *jr.Scope
			}
			return ""
		}(),
		KilledBy: id.Email,
	})

	_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Jobs", "Killed", jr.Name,
		"Run "+traceID+" stopped, recorded as "+disposition)

	run := s.fetchRunByID(r, traceID)
	httpx.JSON(w, http.StatusAccepted, run)
}

// dispositionOutcome maps a kill disposition to its run-end activity outcome
// (RX-7). It is deliberately NOT the identity function: an unclassified stop
// keeps writing 'failure', exactly as it did before dispositions existed, so
// the default path stays byte-for-byte unchanged and `killed` remains in the
// danger family the History filters and health rollup already put it in
// (RX-Q7). Only a disposition the operator actually chose moves the outcome.
// The activity row's KilledBy records the human on every path regardless.
func dispositionOutcome(disposition string) string {
	if disposition == "killed" {
		return "failure"
	}
	return disposition
}

// updateJobTags sets a job's user-authored tags (tags-support.md D6). Tags are
// SQLite-only and survive Git syncs (upsertJobs no longer writes them). The job is
// addressed by integer rowid, so the write is source-agnostic. Full replace of the
// tag set; reuses the shared normalizeTags helper + caps (scripts_mount.go). Gate is
// session + CSRF only (D4: any logged-in user) — wired in mountExecution.
func (s *Server) updateJobTags(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	jobID := r.PathValue("jobId")

	// Decode/normalize/UPDATE + RowsAffected==0 → 404 is the shared skeleton (CC.12).
	if _, ok := s.writeTagsUpdate(w, r, "jobs", "rowid = ?", "job not found", jobID); !ok {
		return
	}

	// Re-read for the response and the human-readable audit target (rowid → name).
	jr := s.fetchJobByID(r, jobID)
	if jr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "job not found")
		return
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Jobs", "Updated", jr.Name, "Job tags updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Jobs", Action: "Updated", Target: jr.Name,
	})
	httpx.JSON(w, http.StatusOK, jr)
}

// ─────────────────────────────────────────────────────────────────────────────
// Workflows
// ─────────────────────────────────────────────────────────────────────────────

type workflowRow struct {
	ID int64 `json:"id"`
	// UID — the stable surrogate identity (AF-4a); see jobRow.UID.
	UID         string   `json:"uid,omitempty"`
	Name        string   `json:"name"`
	Source      string   `json:"source"`               // git | amadeus (A9)
	SourcePath  *string  `json:"sourcePath,omitempty"` // repo-relative file path (folder browsing)
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Status      string   `json:"status"`
	Schedule    *string  `json:"schedule"`
	Disabled    bool     `json:"disabled"`
	LastRunAt   *string  `json:"lastRunAt"`
	// LastSkippedAt / LastSkipReason — the workflow twin of the job fields
	// (FX-D1). Written at the same time deliberately: the last three defects in
	// this area were all a guard applied to one twin and not the other.
	LastSkippedAt  *string `json:"lastSkippedAt"`
	LastSkipReason *string `json:"lastSkipReason"`
	NextRunAt      *string `json:"nextRunAt"`
	CreatedAt      *string `json:"createdAt"`
	LastModifiedAt *string `json:"lastModifiedAt"`
	// DeletedAt — the recycle-bin stamp (RH); see jobRow.DeletedAt.
	DeletedAt *string         `json:"deletedAt,omitempty"`
	Steps     []workflow.Step `json:"steps"`
	// RB-23 (completed in RF-17): the agencies this workflow reaches, DERIVED —
	// via each constituent job's scope, since a workflow has no scope of its own.
	// The Jobs catalog got this column in v0.56.3 and its workflow sibling was
	// missed, which left the two halves of the same catalog answering "whose is
	// this?" differently. Display and search only: no storage, no authoring, and
	// no bearing on authorization (that is workflowScopesPermit, per constituent
	// scope).
	Agencies  []string            `json:"agencies,omitempty"`
	Schedules []scheduleEntryResp `json:"schedules,omitempty"`
	Layout    json.RawMessage     `json:"layout,omitempty"` // WC-P7 advisory canvas node positions ({nodeId:{x,y}}); nil ⇒ auto-layout
	// AN-2 — the operator annotation; see jobRow's copy of this comment. Same
	// shape, same rules, same struct, so the twins cannot answer differently.
	annotationFields
}

// wfAgencyInput is one workflow's identity plus its raw steps, the only inputs
// workflowAgencies needs. It exists because listWorkflows' row struct is
// function-local.
//
// R2F-3 re-keyed this from (source, name) to the UID. Under R2-5 two workflows
// may share a name within a source, so the old key COLLIDED: both twins got
// whichever set was written last, and the Agency column — the very thing that
// tells them apart — could name the wrong department's.
type wfAgencyInput struct{ uid, stepsJSON string }

// workflowAgencies resolves the DERIVED agency set of each workflow on a page
// (RB-23's workflow half, RF-17).
//
// A workflow has no scope of its own — it fans out to jobs that each have one —
// so its agencies are the UNION over its constituent jobs' scopes. That is the
// same walk workflowScopesPermit authorizes over, which is why a workflow
// spanning two departments reports both rather than picking one: "whose is this?"
// and "who may run it?" should not have different answers.
//
// One query for the whole page, keyed by (source, name). Job-name resolution
// deliberately ignores the A11 per-step source override: this is a display hint,
// and a name that resolves to a different source still belongs to the same
// department in every real deployment. Authorization does the precise walk.
func (s *Server) workflowAgencies(r *http.Request, in []wfAgencyInput) map[string][]string {
	out := map[string][]string{}
	if len(in) == 0 {
		return out
	}
	// Flatten every page workflow's steps once, collecting the job names to ask about.
	names := make([]any, 0, len(in))
	seen := map[string]bool{}
	perWF := make(map[string][]string, len(in))
	for _, wf := range in {
		if wf.uid == "" {
			continue // no identity to key on; the column simply stays empty
		}
		steps, err := workflow.ParseSteps(wf.stepsJSON)
		if err != nil {
			continue
		}
		jobs := workflow.FlattenSteps(steps)
		perWF[wf.uid] = jobs
		for _, jn := range jobs {
			if !seen[jn] {
				seen[jn] = true
				names = append(names, jn)
			}
		}
	}
	if len(names) == 0 {
		return out
	}

	ph := strings.Repeat(",?", len(names))[1:]
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT j.name, a.name
		FROM jobs j
		JOIN scopes s          ON s.name = j.scope
		JOIN scope_agencies sa ON sa.scope_id = s.id
		JOIN agencies a        ON a.id = sa.agency_id
		WHERE j.name IN (`+ph+`)`, names...)
	if err != nil {
		return out // display-only: a failure here costs a column, never the page
	}
	byJob := map[string][]string{}
	for rows.Next() {
		var jobName, agency string
		if rows.Scan(&jobName, &agency) == nil {
			byJob[jobName] = append(byJob[jobName], agency)
		}
	}
	rows.Close()

	for key, jobs := range perWF {
		set := map[string]bool{}
		var list []string
		for _, jn := range jobs {
			for _, a := range byJob[jn] {
				if !set[a] {
					set[a] = true
					list = append(list, a)
				}
			}
		}
		if len(list) > 0 {
			sort.Strings(list) // stable rendering; the map iteration above is not
			out[key] = list
		}
	}
	return out
}

// workflowAgenciesByUID loads the named workflows' step graphs in one query and
// resolves each one's derived agency set (R2F-3). Display-only: a failure costs
// a badge, never the page.
func (s *Server) workflowAgenciesByUID(r *http.Request, uids []string) map[string][]string {
	if len(uids) == 0 {
		return map[string][]string{}
	}
	args := make([]any, len(uids))
	for i, u := range uids {
		args[i] = u
	}
	ph := strings.Repeat(",?", len(uids))[1:]
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT uid, steps FROM workflows WHERE uid IN (`+ph+`)`, args...)
	if err != nil {
		return map[string][]string{}
	}
	in := make([]wfAgencyInput, 0, len(uids))
	for rows.Next() {
		var uid string
		var steps sql.NullString
		if rows.Scan(&uid, &steps) == nil {
			in = append(in, wfAgencyInput{uid: uid, stepsJSON: steps.String})
		}
	}
	rows.Close()
	return s.workflowAgencies(r, in)
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := q.Get("q")
	tagFilter := tagutil.ParseQuery(q)
	page, pageSize := pageParams(q)

	// The list-row aggregates are derived in SQL so the page is one round-trip
	// (CC.9): paused_count replaces the per-row isWorkflowDisabled query, and the
	// latest workflow_run is joined once by rowid for the Last Run timestamp +
	// Result status (was a per-row workflowLastRun query). Next Run still needs a
	// cron evaluation in Go, so it is batched into a single query below. The COUNT
	// and page query share one filter slice (CC.14).
	// RH: both strings carry the filter — a total counted over a different row
	// set than the page it describes is its own bug.
	cq := `SELECT COUNT(*) FROM workflows WHERE deleted_at IS NULL`
	query := `SELECT w.rowid, w.name, w.source, COALESCE(w.description,''), w.steps, w.schedule, w.enabled,
			w.created_at, w.last_modified_at, w.source_path, w.tags, w.uid,
			(SELECT COUNT(*) FROM paused_jobs p WHERE p.source = w.source AND p.owner_kind = 'workflow' AND p.name = w.name) AS paused_count,
			lr.created_at AS last_run_at,
			lr.status AS last_run_status,
			ls.created_at AS last_skipped_at,
			ls.queued_reason AS last_skip_reason,
			-- AN-2, the workflow half of the jobs-list join: chip + contact only,
			-- outer-joined on the uid (never the name), COALESCEd so "no
			-- annotation" arrives as "not critical" rather than NULL.
			COALESCE(an.critical, 0) AS critical,
			COALESCE(an.contact, '') AS contact
		FROM workflows w
		LEFT JOIN annotations an ON an.owner_kind = 'workflow' AND an.owner_uid = w.uid
		-- FX-D1, the workflow twin. recordSkippedWorkflowFire writes 'skipped' rows
		-- here for exactly the same reasons the job side does, so the same defect
		-- applied: a suppressed fire impersonated the last run. Fixed on both sides
		-- together -- a guard on one twin and not the other is how the last three
		-- of these survived review.
		LEFT JOIN workflow_runs lr ON lr.rowid = (
			SELECT r.rowid FROM workflow_runs r
			WHERE r.workflow_source = w.source AND r.workflow_name = w.name
			  AND r.status <> 'skipped'
			ORDER BY r.created_at DESC, r.rowid DESC LIMIT 1)
		LEFT JOIN workflow_runs ls ON ls.rowid = (
			SELECT r.rowid FROM workflow_runs r
			WHERE r.workflow_source = w.source AND r.workflow_name = w.name
			  AND r.status = 'skipped'
			ORDER BY r.created_at DESC, r.rowid DESC LIMIT 1)
		WHERE w.deleted_at IS NULL`
	var args []any
	if search != "" {
		cq += " AND name LIKE ?"
		query += " AND w.name LIKE ?"
		args = append(args, "%"+search+"%")
	}
	if frag, fargs := tagFilter.SQLFilter("w.tags"); frag != "" {
		cq += " AND " + frag
		query += " AND " + frag
		args = append(args, fargs...)
	}
	var total int
	_ = s.db.QueryRowContext(r.Context(), cq, args...).Scan(&total)

	query += " ORDER BY w.name LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	// Drain before the batched schedules lookup below (see listJobs note).
	type wfBase struct {
		rowid                     int64
		name, source, desc        string
		stepsJSON                 string
		schedule                  sql.NullString
		enabled                   int
		createdAt, lastModifiedAt sql.NullString
		sourcePath                sql.NullString
		tagsJSON                  string
		uid                       sql.NullString
		pausedCount               int
		lastRunAt                 sql.NullString
		lastSkippedAt             sql.NullString
		lastSkipReason            sql.NullString
		lastRunStatus             sql.NullString
		critical                  int
		contact                   string
	}
	var bases []wfBase
	for rows.Next() {
		var wf wfBase
		if err := rows.Scan(&wf.rowid, &wf.name, &wf.source, &wf.desc, &wf.stepsJSON, &wf.schedule, &wf.enabled,
			&wf.createdAt, &wf.lastModifiedAt, &wf.sourcePath, &wf.tagsJSON, &wf.uid,
			&wf.pausedCount, &wf.lastRunAt, &wf.lastRunStatus,
			&wf.lastSkippedAt, &wf.lastSkipReason, &wf.critical, &wf.contact); err != nil {
			continue
		}
		bases = append(bases, wf)
	}
	rows.Close()

	// Batched Next Run (CC.9): one query for every schedule entry of the page's
	// workflows, replacing the per-row loadSchedules call. earliestNextRunAt is
	// reused so the comparison semantics are identical; a paused workflow gets no
	// next-run (mirroring loadSchedules(paused=true)). Runs after the outer
	// iterator is drained, so it does not nest inside an open result set.
	nextRuns := map[[2]string]*string{}
	if len(bases) > 0 {
		names := make([]any, 0, len(bases))
		seen := map[string]bool{}
		paused := map[[2]string]bool{}
		for _, wf := range bases {
			if !seen[wf.name] {
				seen[wf.name] = true
				names = append(names, wf.name)
			}
			if wf.pausedCount > 0 {
				paused[[2]string{wf.source, wf.name}] = true
			}
		}
		// FX-F1 — the FULL spec, not the cron column alone. This query used to
		// select `cron` only and project through bare cronutil.Next, which made
		// this list the one surface in the app disagreeing with the engine: an
		// interval- or once-mode entry showed NO next run at all (Next("") fails),
		// and a windowed cron showed a PHANTOM fire before its window opened and
		// after it expired. The workflow DETAIL projects through loadSchedules and
		// was right, so list and detail disagreed about the same entry — the kind
		// of disagreement an operator resolves by trusting whichever is wrong.
		ph := strings.Repeat(",?", len(names))[1:]
		srows, err := s.db.QueryContext(r.Context(), `
			SELECT owner_source, owner_name, cron, COALESCE(interval,''), COALESCE(start_at,''), COALESCE(end_at,'')
			FROM definition_schedules
			WHERE owner_kind = 'workflow' AND owner_name IN (`+ph+`)
			ORDER BY owner_source, owner_name, position, name`, names...)
		if err != nil {
			s.log.Warn("workflows list: batch next-run query failed; page shows no projections", "err", err)
		}
		if err == nil {
			type entrySpec struct{ cron, interval, startAt, endAt string }
			specs := map[[2]string][]entrySpec{}
			for srows.Next() {
				var src, nm string
				var e entrySpec
				if srows.Scan(&src, &nm, &e.cron, &e.interval, &e.startAt, &e.endAt) == nil {
					k := [2]string{src, nm}
					specs[k] = append(specs[k], e)
				}
			}
			srows.Close()
			now := time.Now()
			appLoc := s.appLocation()
			for k, es := range specs {
				if paused[k] {
					continue
				}
				entries := make([]scheduleEntryResp, 0, len(es))
				for _, e := range es {
					resp := scheduleEntryResp{}
					win := cronutil.NewWindow(
						windowBound(sql.NullString{String: e.startAt, Valid: e.startAt != ""}),
						windowBound(sql.NullString{String: e.endAt, Valid: e.endAt != ""}))
					spec := cronutil.Spec{Cron: e.cron, Interval: e.interval, Window: win}
					if next, ok := cronutil.NextSpec(spec, now, appLoc); ok {
						resp.NextRunAt = rfc3339Ptr(next)
					}
					entries = append(entries, resp)
				}
				nextRuns[k] = earliestNextRunAt(entries)
			}
		}
	}

	// RF-17 — the derived agency set per workflow, batched exactly like Next Run
	// above (one query for the whole page, after the outer iterator is drained, so
	// nothing nests inside an open result set — the SQLite pool rule).
	//
	// A workflow has no scope, so its agencies are the UNION of its constituent
	// jobs' — the same walk workflowScopesPermit authorizes over, which is why a
	// workflow spanning two departments shows both rather than picking one.
	wfInputs := make([]wfAgencyInput, 0, len(bases))
	for _, wf := range bases {
		wfInputs = append(wfInputs, wfAgencyInput{uid: wf.uid.String, stepsJSON: wf.stepsJSON})
	}
	wfAgencies := s.workflowAgencies(r, wfInputs)

	items := []workflowRow{}
	for _, wf := range bases {
		steps, _ := workflow.ParseSteps(wf.stepsJSON)
		wr := workflowRow{
			ID:          wf.rowid,
			UID:         wf.uid.String,
			Name:        wf.name,
			Source:      wf.source,
			Description: wf.desc,
			Tags:        tagutil.Parse(wf.tagsJSON),
			Steps:       steps,
			Agencies:    wfAgencies[wf.uid.String],
			Status:      "idle",
		}
		// AN-2 list subset — notes are detail-only (see jobRow's twin).
		wr.Critical = wf.critical == 1
		wr.Contact = wf.contact
		if wf.schedule.Valid {
			wr.Schedule = &wf.schedule.String
		}
		if wf.createdAt.Valid {
			wr.CreatedAt = &wf.createdAt.String
		}
		if wf.lastModifiedAt.Valid {
			wr.LastModifiedAt = &wf.lastModifiedAt.String
		}
		if wf.sourcePath.Valid {
			wr.SourcePath = &wf.sourcePath.String
		}
		// LB12/CC.9: Disabled, Last Run (timestamp + Result status) and Next Run
		// all come from the batched page query plus the single schedules query
		// above, replacing three per-row lookups (isWorkflowDisabled,
		// workflowLastRun, loadSchedules).
		wr.Disabled = wf.pausedCount > 0
		if wf.lastRunAt.Valid {
			wr.LastRunAt = &wf.lastRunAt.String
			if wf.lastRunStatus.Valid {
				if st := mapRunStatus(wf.lastRunStatus.String); st != "" {
					wr.Status = st
				}
			}
		}
		// FX-D1 — the newest SUPPRESSION, kept apart from the newest run so the
		// evidence survives without impersonating one.
		if wf.lastSkippedAt.Valid {
			wr.LastSkippedAt = &wf.lastSkippedAt.String
		}
		if wf.lastSkipReason.Valid && wf.lastSkipReason.String != "" {
			wr.LastSkipReason = &wf.lastSkipReason.String
		}
		wr.NextRunAt = nextRuns[[2]string{wf.source, wf.name}]
		items = append(items, wr)
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getWorkflow(w http.ResponseWriter, r *http.Request) {
	wfID := r.PathValue("workflowId")
	wr := s.fetchWorkflowByID(r, wfID)
	if wr == nil || wr.DeletedAt != nil {
		// RH/FX-A4: a binned workflow reads as absent, exactly as getJob treats a
		// binned job. fetchWorkflowByID deliberately returns binned rows for the
		// recycle bin's own use; nothing routes the bin through THIS handler, so
		// the leniency only ever served to make a binned workflow look live.
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	httpx.JSON(w, http.StatusOK, wr)
}

// workflowScopesReadable is the scope guard a workflow write has to pass, shared
// by triggerWorkflow (WB-S1/D1) and patchWorkflow (FX-19).
//
// A workflow fans out to many scoped jobs, so the check is over EVERY constituent
// job rather than over the workflow as a whole — a workflow has no scope of its
// own. An unrestricted actor ("*") skips it; everyone else must reach each
// constituent scope, matching runJob.
//
// It writes the failure response itself and reports whether the caller may
// proceed, so a handler that forgets to check the result cannot silently continue.
func (s *Server) workflowScopesReadable(w http.ResponseWriter, r *http.Request, eng *workflow.Engine, id auth.Identity, wr *workflowRow) bool {
	return s.workflowScopesPermit(w, r, eng, id, wr, auth.PermTriggerJobs)
}

// workflowScopesPermit is workflowScopesReadable plus the RB-2 verb check, applied
// to EVERY constituent scope — a workflow may not be triggered by someone who holds
// the verb on only some of the jobs it fans out to.
//
// ⚠️ The `id.Unrestricted()` fast path that used to sit at the top is GONE, and its
// absence is the point: unrestricted describes scope REACH, not authority. An
// unrestricted viewer reaches every scope and holds no verb at all, so returning
// early for them would have skipped the very check this function now exists to make.
// The cost is one JobScopes query for unrestricted actors that previously skipped it.
//
// An empty constituent scope (an unscoped job inside a workflow) is unrestricted-only,
// mirroring RB-26 on the single-job path — a workflow must not be a way to run an
// unscoped job without binding one.
func (s *Server) workflowScopesPermit(w http.ResponseWriter, r *http.Request, eng *workflow.Engine, id auth.Identity, wr *workflowRow, perm string) bool {
	scopes, err := eng.JobScopes(r.Context(), wr.Steps, wr.Source)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return false
	}
	// RF-6 baseline: an EMPTY scope list must not be a free pass. It happens when
	// no constituent job resolves (a workflow referencing jobs that no longer
	// exist), and before this the loop below simply did not execute — so the route
	// authorized on nothing. Holding the verb somewhere is the floor; the loop
	// then enforces it per scope.
	if !id.CanAnywhere(perm) {
		if s.auth != nil {
			s.auth.AuditDenied(r, id.Email, "insufficient_permission", perm,
				auditDetails(r, "workflow operation requires "+perm))
		}
		httpx.Fail(w, http.StatusForbidden, "forbidden",
			"insufficient permissions: "+perm+" is required")
		return false
	}
	for _, sc := range scopes {
		if sc == "" {
			if !id.CanUnbound(perm) {
				if s.auth != nil {
					s.auth.AuditDenied(r, id.Email, "insufficient_scope", auth.AllScopes,
						auditDetails(r, "workflow contains an unscoped job ("+perm+" required unbound)"))
				}
				httpx.Fail(w, http.StatusForbidden, "scope_required",
					"this workflow includes a job with no scope; only an unrestricted operator may run it")
				return false
			}
			continue
		}
		if !auth.ScopeReadable(id, sc) {
			s.denyScope(w, r, sc, "you do not have access to scope "+sc)
			return false
		}
		if !s.requireCan(w, r, id, perm, sc) {
			return false
		}
	}
	return true
}

// patchWorkflow pauses/resumes a workflow (its `disabled` flag is a paused_jobs
// row, exactly as a job's is).
//
// FX-19 — this route used to be requireSession + requireCSRF and NOTHING else: no
// role check and, worse, no scope check, so any authenticated user could pause
// any workflow in the system and suppress someone else's schedule. Its two
// siblings both guard — triggerWorkflow over every constituent scope (WB-S1/D1),
// pauseJob/resumeJob via auth.ScopeReadable (SU-2) — and this one was missed by
// both sweeps.
//
// The verb is KILL JOBS, not triggerJobs (RF-Q1, the RBAC-fixes plan):
// pausing is run suppression, exactly like pauseJob/resumeJob, and v0.56.4
// accidentally wired triggerJobs here — which let a role holding
// triggerJobs-but-not-killJobs pause a workflow while being unable to pause the
// jobs inside it.
func (s *Server) patchWorkflow(eng *workflow.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		wfID := r.PathValue("workflowId")
		wr := s.fetchWorkflowByID(r, wfID)
		if wr == nil || wr.DeletedAt != nil {
			// RH: pausing a binned workflow is meaningless — it reads as absent.
			httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
			return
		}
		if !s.workflowScopesPermit(w, r, eng, id, wr, auth.PermKillJobs) {
			return
		}

		var body struct {
			Disabled bool `json:"disabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
			return
		}

		now := time.Now().UTC().Format(time.RFC3339)
		wfSrc := s.defSource(r, "workflows", wfID)
		if body.Disabled {
			_, _ = s.db.ExecContext(r.Context(), `
			INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid)
			VALUES (?, 'workflow', ?, ?, ?, NULLIF(?, ''))
			ON CONFLICT(owner_kind, owner_uid) DO UPDATE SET paused_by=excluded.paused_by, paused_at=excluded.paused_at
		`, wfSrc, wr.Name, id.Email, now, wr.UID)
		} else {
			_, _ = s.db.ExecContext(r.Context(),
				`DELETE FROM paused_jobs WHERE owner_kind = 'workflow' AND (owner_uid = ? OR (owner_uid IS NULL AND source = ? AND name = ?))`, wr.UID, wfSrc, wr.Name)
		}

		action := "Enabled"
		if body.Disabled {
			action = "Disabled"
		}
		_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Workflows", action, wr.Name, "")
		_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
			Kind: "config", Actor: id.Email, Category: "Workflows", Action: action, Target: wr.Name,
		})

		wr.Disabled = body.Disabled
		httpx.JSON(w, http.StatusOK, wr)
	}
}

func (s *Server) triggerWorkflow(eng *workflow.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		wfID := r.PathValue("workflowId")
		wr := s.fetchWorkflowByID(r, wfID)
		if wr == nil || wr.DeletedAt != nil {
			// RH: a binned workflow reads as absent, exactly as a binned job does in
			// getJob/runJob. fetchWorkflowByID deliberately returns deleted rows so the
			// recycle bin can read them, so every path that ACTS on the definition
			// refuses one here.
			httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
			return
		}
		if wr.Disabled {
			httpx.Fail(w, http.StatusConflict, "workflow_disabled", "workflow is disabled")
			return
		}

		// WB-S1 / D1, shared with patchWorkflow (FX-19).
		if !s.workflowScopesReadable(w, r, eng, id, wr) {
			return
		}

		// AR — optional deferral: a body with runAt parks this trigger as a
		// pending run instead of firing it. The body is optional (the plain
		// Trigger button sends none) and malformed JSON is rejected like runJob's.
		var body struct {
			RunAt string `json:"runAt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && err != io.EOF {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
			return
		}
		runAt, runAtErr := parseRunAt(body.RunAt)
		if runAtErr != nil {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", runAtErr.Error())
			return
		}

		// FX-C — the fleet-wide calendar freeze binds workflows too (FX-Q2).
		//
		// Without this the workflow trigger is a ONE-CLICK BYPASS of the gate this
		// band just added to the job trigger: a frozen job refuses, and a workflow
		// wrapping that same job fans out normally. It is the producer with the
		// largest blast radius, so leaving it exempt would invert the whole point.
		//
		// PAUSE is deliberately NOT relaxed here the way it is for a job click. The
		// job exemption is justified by the Run dialog naming the pause in a
		// confirmation (FX-Q1); the workflow trigger has no such dialog, and an
		// override nobody was told about is exactly what FX-Q1 refused to grant the
		// token route. So a paused workflow stays refused above, by wr.Disabled.
		//
		// The freeze is skipped for a DEFERRED trigger for the same reason the job
		// path skips it: promotion re-judges at the instant that matters.
		if runAt == "" {
			if g := scheduler.Evaluate(r.Context(), s.db, s.log, scheduler.GateRequest{
				Origin: scheduler.OriginManual, Source: wr.Source, OwnerKind: "workflow", Name: wr.Name,
			}, s.appLocation()); !g.Allowed && g.Gate == "calendar" {
				httpx.Fail(w, http.StatusConflict, "suppressed_by_calendar", g.Reason)
				return
			}
		}

		if runAt != "" {
			// RB-31: the deferred WORKFLOW row carries scope "" and nil params —
			// unlike the job path, which freezes the bound scope and the whole
			// EnqueueParams. That is correct rather than an oversight: a workflow has
			// no scope of its own (it fans out to jobs that each have one), so there
			// is nothing to freeze, and the engine re-resolves each step's scope at
			// fire time. Authorization is not skipped — workflowScopesPermit ran
			// above, over EVERY constituent scope, before this branch was reached.
			//
			// RB-Q12: like every pending run, this fires with the authorization it
			// was created under. Revoking the creator's grants does NOT cancel it;
			// the recourse is cancelling the parked row.
			pendingID, err := scheduler.InsertPendingRun(r.Context(), s.db, "workflow", wr.Name, wr.Source, "", runAt, id.Email, nil)
			if err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Workflows", "Scheduled", wr.Name, "Ad-hoc workflow run scheduled for "+runAt)
			httpx.JSON(w, http.StatusAccepted, map[string]any{
				"pending":      true,
				"id":           pendingID,
				"workflowName": wr.Name,
				"runAt":        runAt,
				"scheduledBy":  id.Email,
			})
			return
		}

		// WB-S1: honor the global concurrency cap at the workflow-trigger boundary —
		// the same gate runJob applies — so a workflow can't start a fan-out while the
		// system is already at capacity. (Deferred runs are judged at promotion.)
		maxConcurrent := s.maxConcurrent(r)
		var active int
		_ = s.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM runs WHERE status IN ('queued','running')`).Scan(&active)
		if active >= maxConcurrent {
			httpx.Fail(w, http.StatusConflict, "concurrency_cap", "global concurrency cap reached")
			return
		}

		result, err := eng.Trigger(r.Context(), workflow.TriggerParams{
			WorkflowName:   wr.Name,
			WorkflowSource: wr.Source,
			WorkflowID:     wr.ID,
			Steps:          wr.Steps,
			TriggeredBy:    id.Email,
		})
		if err != nil {
			httpx.Fail500(w, s.log, "trigger_failed", err)
			return
		}

		_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Workflows", "Triggered", wr.Name, "workflow run "+result.TraceID)

		// Build and return a WorkflowRun response.
		now := time.Now().UTC().Format(time.RFC3339)
		resp := map[string]any{
			"traceId":      result.TraceID,
			"workflowId":   wr.ID,
			"workflowName": wr.Name,
			"status":       "running",
			"triggeredBy":  id.Email,
			"startedAt":    now,
			"completedAt":  nil,
			"durationMs":   nil,
			"jobTraceIds":  result.JobTraceIDs,
		}
		httpx.JSON(w, http.StatusAccepted, resp)
	}
}

func (s *Server) fetchWorkflowByID(r *http.Request, wfID string) *workflowRow {
	var wf struct {
		rowid                     int64
		name, source, desc        string
		stepsJSON                 string
		schedule                  sql.NullString
		enabled                   int
		createdAt, lastModifiedAt sql.NullString
		sourcePath                sql.NullString
		tagsJSON                  string
		layoutJSON                sql.NullString
		deletedAt                 sql.NullString
		uid                       sql.NullString
	}
	err := s.db.QueryRowContext(r.Context(), `
		SELECT rowid, name, source, COALESCE(description,''), steps, schedule, enabled, created_at, last_modified_at, source_path, tags, layout_json, deleted_at, uid
		FROM workflows WHERE rowid = ?
	`, wfID).Scan(&wf.rowid, &wf.name, &wf.source, &wf.desc, &wf.stepsJSON, &wf.schedule, &wf.enabled,
		&wf.createdAt, &wf.lastModifiedAt, &wf.sourcePath, &wf.tagsJSON, &wf.layoutJSON, &wf.deletedAt, &wf.uid)
	if err != nil {
		return nil
	}
	steps, _ := workflow.ParseSteps(wf.stepsJSON)
	wr := &workflowRow{
		ID:          wf.rowid,
		UID:         wf.uid.String,
		Name:        wf.name,
		Source:      wf.source,
		Description: wf.desc,
		Tags:        tagutil.Parse(wf.tagsJSON),
		Steps:       steps,
		Status:      "idle",
		Disabled:    s.isWorkflowDisabled(r, wf.source, wf.name),
	}
	// AN-2 — the full annotation on the detail path; see fetchJobDetail's twin
	// for why this is a follow-up query rather than a join.
	wr.annotationFields = s.loadAnnotation(r.Context(), "workflow", wf.uid.String).fields()
	if wf.schedule.Valid {
		wr.Schedule = &wf.schedule.String
	}
	if wf.createdAt.Valid {
		wr.CreatedAt = &wf.createdAt.String
	}
	if wf.lastModifiedAt.Valid {
		wr.LastModifiedAt = &wf.lastModifiedAt.String
	}
	if wf.sourcePath.Valid {
		wr.SourcePath = &wf.sourcePath.String
	}
	if wf.layoutJSON.Valid && wf.layoutJSON.String != "" {
		wr.Layout = json.RawMessage(wf.layoutJSON.String)
	}
	if wf.deletedAt.Valid {
		wr.DeletedAt = &wf.deletedAt.String
	}
	// LB12: Last Run timestamp + Result status from the most-recent workflow_runs
	// row (NextRunAt is already populated from the schedules below).
	if at, st := s.workflowLastRun(r.Context(), wf.source, wf.name); at != nil {
		wr.LastRunAt = at
		if st != "" {
			wr.Status = st
		}
	}
	wr.Schedules = s.loadSchedules(r.Context(), wf.source, "workflow", wf.name, wr.Disabled)
	wr.NextRunAt = earliestNextRunAt(wr.Schedules)
	return wr
}

// updateWorkflowTags sets a workflow's user-authored tags (tags-support.md). Tags
// are SQLite-only (migration 290) and survive Git syncs (upsertWorkflows omits
// them). Addressed by integer rowid, so source-agnostic. Full replace; reuses the
// shared normalizeTags helper + caps. Gate is session + CSRF only (D4) — wired in
// mountExecution.
func (s *Server) updateWorkflowTags(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	wfID := r.PathValue("workflowId")

	// Decode/normalize/UPDATE + RowsAffected==0 → 404 is the shared skeleton (CC.12).
	if _, ok := s.writeTagsUpdate(w, r, "workflows", "rowid = ?", "workflow not found", wfID); !ok {
		return
	}

	wr := s.fetchWorkflowByID(r, wfID)
	if wr == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Workflows", "Updated", wr.Name, "Workflow tags updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Workflows", Action: "Updated", Target: wr.Name,
	})
	httpx.JSON(w, http.StatusOK, wr)
}

func (s *Server) isWorkflowDisabled(r *http.Request, source, name string) bool {
	// Read-status display check keyed by name and source since migration 170 / Q-G
	// retired the __wf__ prefix.
	var n int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM paused_jobs WHERE source = ? AND owner_kind = 'workflow' AND name = ?`, source, name).Scan(&n)
	return n > 0
}

// workflowLastRun returns the created_at and mapped display status of the most
// recent run for a workflow, source-qualified per A9 (mirrors the jobs/runs
// subquery and scheduleLastRunAt). Returns (nil, "") when the workflow has no
// runs. LB12: the Workflows list/detail serializers never joined workflow_runs,
// so "Last Run" was always empty and "Result" was hardcoded "idle".
func (s *Server) workflowLastRun(ctx context.Context, source, name string) (*string, string) {
	var lastRunAt, lastStatus sql.NullString
	_ = s.db.QueryRowContext(ctx, `
		SELECT created_at, status FROM workflow_runs
		WHERE workflow_source = ? AND workflow_name = ? AND status <> 'skipped'
		ORDER BY created_at DESC LIMIT 1
	`, source, name).Scan(&lastRunAt, &lastStatus)
	var at *string
	if lastRunAt.Valid {
		at = &lastRunAt.String
	}
	status := ""
	if lastStatus.Valid {
		status = mapRunStatus(lastStatus.String)
	}
	return at, status
}

// ─────────────────────────────────────────────────────────────────────────────
// Workflow runs
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) listWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	workflowID := q.Get("workflowId")
	workflowName := q.Get("workflowName")
	workflowSource := q.Get("workflowSource")
	statusFilter := q.Get("status")
	calendarFilter := q.Get("calendar")
	from := q.Get("from")
	to := q.Get("to")
	page, pageSize := pageParams(q)

	base := `FROM workflow_runs WHERE 1=1`
	var args []any
	// R2-1 — the workflow's permanent identity outranks both: unlike the name it
	// cannot come to mean two workflows, and unlike the rowid it is never reused.
	if wfUID := q.Get("workflowUid"); wfUID != "" {
		base += " AND workflow_uid = ?"
		args = append(args, wfUID)
	}
	// WB-D2: prefer the stable (workflow_source, workflow_name) identity (backed by
	// the 270 composite index) over the bare, reusable workflow_id rowid. Falls back
	// to workflow_id for older callers.
	if workflowName != "" {
		base += " AND workflow_name = ?"
		args = append(args, workflowName)
		if workflowSource != "" {
			base += " AND workflow_source = ?"
			args = append(args, workflowSource)
		}
	} else if workflowID != "" {
		base += " AND workflow_id = ?"
		args = append(args, workflowID)
	}
	if statusFilter != "" {
		frag, fargs := workflowStatusFilter(statusFilter)
		base += " AND (" + frag + ")"
		args = append(args, fargs...)
	}
	// CAL-29, workflow half. Workflow skip rows exist only from CAL-32 onward, so
	// unlike the jobs side this table had no prior 'skipped' rows at all.
	if calendarFilter == "*" {
		base += " AND suppressed_by_calendar IS NOT NULL"
	} else if calendarFilter != "" {
		base += " AND suppressed_by_calendar = ?"
		args = append(args, calendarFilter)
	}
	// RX-17 — the same two reaction filters the runs list carries, because a
	// reaction can fire a WORKFLOW and "what did this run trigger?" has to
	// answer across both tables or it answers half the graph.
	if tk := q.Get("triggerKind"); tk != "" {
		base += " AND trigger_kind = ?"
		args = append(args, tk)
	}
	if rt := q.Get("reactedTo"); rt != "" {
		base += " AND reacted_to_run_id = ?"
		args = append(args, rt)
	}
	// RX-17 — pivot to ONE workflow run by trace id, the jobs side's twin. The
	// forward because-of link cannot know which table its upstream lives in
	// (reacted_to_run_id records the id, not the kind), so it offers both and
	// the one that holds the run answers.
	if tr := q.Get("trace"); tr != "" {
		base += " AND id = ?"
		args = append(args, tr)
	}
	if from != "" {
		base += " AND created_at >= ?"
		args = append(args, from)
	}
	if to != "" {
		base += " AND created_at < ?"
		args = append(args, to)
	}
	// SU-2: mirror listRuns' scope filter for consistency with getWorkflowRun (which
	// already gates). workflow_runs.scope exists (migration 030) but current engine
	// inserts leave it NULL, so today this only gates rows that DO carry a scope;
	// NULL (global) rows stay visible to everyone.
	if frag, fargs := s.scopeWhereFragment(r, "scope"); frag != "" {
		base += frag
		args = append(args, fargs...)
	}

	// TS-20: allowlisted ?sort=&order= (see listRuns). A cancelled workflow run
	// stores a CHECK-legal terminal status (migration 260), so the raw-status
	// rank is faithful to what the row carries.
	orderBy, sortErr := sortparam.OrderBy(q, map[string]string{
		"workflow":  "workflow_name",
		"schedule":  "schedule_name",
		"started":   "started_at",
		"completed": "completed_at",
		"user":      "triggered_by",
		"duration":  "duration_ms",
		"status":    sortparam.RunStatusRank,
	}, " ORDER BY created_at DESC", "created_at DESC, id DESC")
	if sortErr != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_sort", sortErr.Error())
		return
	}

	// One filter slice for the COUNT + page query (CC.14 — no parallel cArgs).
	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) "+base, args...).Scan(&total)

	query := "SELECT id, workflow_id, workflow_name, status, triggered_by, started_at, completed_at, created_at, schedule_name, duration_ms, cancelled, queued_reason, suppressed_by_calendar, trigger_kind, reacted_to_run_id, reaction_depth, workflow_uid " + base +
		orderBy + " LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	// Drain before the per-row childTraceIDs lookup (see listJobs note).
	type wfRunBase struct {
		wrid                                           sql.NullInt64
		traceID, wfName, status                        string
		triggeredBy, startedAt, completedAt, createdAt sql.NullString
		scheduleName                                   sql.NullString
		durationMs                                     sql.NullInt64
		cancelled                                      int
		queuedReason, suppressedByCalendar             sql.NullString
		triggerKind                                    sql.NullString
		// RX-17 — a workflow can be reaction-fired too, so it carries the same
		// provenance pair as a job run.
		reactedToRunID sql.NullString
		reactionDepth  sql.NullInt64
		// R2-1 — the workflow's permanent identity, frozen at insert.
		workflowUID sql.NullString
	}
	var bases []wfRunBase
	for rows.Next() {
		var b wfRunBase
		if err := rows.Scan(&b.traceID, &b.wrid, &b.wfName, &b.status, &b.triggeredBy, &b.startedAt, &b.completedAt, &b.createdAt, &b.scheduleName, &b.durationMs, &b.cancelled,
			&b.queuedReason, &b.suppressedByCalendar, &b.triggerKind, &b.reactedToRunID, &b.reactionDepth, &b.workflowUID); err != nil {
			continue
		}
		bases = append(bases, b)
	}
	rows.Close()

	// R2F-3 — the agency set behind a workflow run's name badge. A workflow has
	// no scope of its own, so this is the union over its constituent jobs, keyed
	// by the run's frozen workflow_uid: exactly what the Workflows catalog shows
	// for the same definition, so a run and its workflow cannot disagree about
	// whose it is. Pre-backfill rows (NULL uid) resolve to nothing and never
	// badge — they cannot be attributed to either twin.
	// ONE query for the page's distinct workflows, not one per row (the listJobs
	// correlated-subquery discipline): a page of 50 runs of 5 workflows costs one
	// round trip, and the walk below is then pure map work.
	// A run with no uid contributes nothing: it is pre-backfill history that
	// cannot be attributed to a definition, and it never badges.
	seenWF := map[string]bool{}
	var wfUIDs []string
	for _, b := range bases {
		if u := b.workflowUID.String; u != "" && !seenWF[u] {
			seenWF[u] = true
			wfUIDs = append(wfUIDs, u)
		}
	}
	runAgencies := s.workflowAgenciesByUID(r, wfUIDs)

	items := []map[string]any{}
	for _, b := range bases {
		items = append(items, map[string]any{
			"traceId":      b.traceID,
			"workflowId":   nullInt64Val(b.wrid),
			"workflowUid":  nullStrVal(b.workflowUID),
			"workflowName": b.wfName,
			"agencies":     append([]string{}, runAgencies[b.workflowUID.String]...),
			"status":       workflowDisplayStatus(b.status, b.cancelled),
			"triggeredBy":  nullStrVal(b.triggeredBy),
			"scheduleName": nullStrVal(b.scheduleName),
			"startedAt":    nullStrVal(b.startedAt),
			"completedAt":  nullStrVal(b.completedAt),
			"durationMs":   nullInt64ToPtr(b.durationMs),
			"cancelled":    b.cancelled == 1,
			// A suppressed workflow fire has no children and no duration; the
			// reason is the only thing it carries, so it must reach the UI.
			"statusReason":         nullStrVal(b.queuedReason),
			"suppressedByCalendar": nullStrVal(b.suppressedByCalendar),
			"triggerKind":          nullStrVal(b.triggerKind),
			"reactedToRunId":       nullStrVal(b.reactedToRunID),
			"reactionDepth":        b.reactionDepth.Int64,
			"jobTraceIds":          s.childTraceIDs(r, b.traceID),
		})
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getWorkflowRun(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")

	var wrid sql.NullInt64
	var wfName, status string
	var triggeredBy, startedAt, completedAt, scheduleName, stepsSnapshot sql.NullString
	var durationMs sql.NullInt64
	var cancelled int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT workflow_id, workflow_name, status, triggered_by, started_at, completed_at, schedule_name, duration_ms, steps_snapshot, cancelled
		FROM workflow_runs WHERE id = ?
	`, traceID).Scan(&wrid, &wfName, &status, &triggeredBy, &startedAt, &completedAt, &scheduleName, &durationMs, &stepsSnapshot, &cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow run not found")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// Load the child runs ONCE (drained before any per-row sub-query — the redactor
	// builds query secrets; SQLite pool rule). Everything below derives from them.
	children := s.loadWorkflowChildRuns(r, traceID)

	// Scope access check (FR-H2 IDOR mitigation, A5 fix).
	id, hasID := auth.IdentityFrom(r.Context())
	if hasID && !id.Unrestricted() {
		for _, cr := range children {
			if !auth.ScopeReadable(id, cr.scope) {
				s.denyScope(w, r, cr.scope, "scope access denied")
				return
			}
		}

		if stepsSnapshot.Valid && stepsSnapshot.String != "" {
			steps, err := workflow.ParseSteps(stepsSnapshot.String)
			if err == nil {
				jobNames := workflow.FlattenSteps(steps)
				if len(jobNames) > 0 {
					placeholders := strings.Repeat("?,", len(jobNames))
					placeholders = placeholders[:len(placeholders)-1]
					query := fmt.Sprintf("SELECT DISTINCT COALESCE(scope,'') FROM jobs WHERE name IN (%s)", placeholders)
					args := make([]any, len(jobNames))
					for i, name := range jobNames {
						args[i] = name
					}
					rows, err := s.db.QueryContext(r.Context(), query, args...)
					if err == nil {
						defer rows.Close()
						for rows.Next() {
							var sc string
							if err := rows.Scan(&sc); err == nil && sc != "" {
								if !auth.ScopeReadable(id, sc) {
									s.denyScope(w, r, sc, "scope access denied")
									return
								}
							}
						}
					}
				}
			}
		}
	}
	stats := make(map[string]nodeStat, len(children))
	jobIDs := make([]string, 0, len(children))
	for _, cr := range children {
		stats[cr.id] = nodeStat{status: mapRunStatus(cr.status), durationMs: nullInt64ToPtr(cr.durationMs)}
		jobIDs = append(jobIDs, cr.id)
	}

	// WB-O3: real per-step kinds come from the per-run snapshot (WB-D1); the merged
	// graph reconstructs parallel/branch grouping with live status for StepChain.
	containers := snapshotNodeContainers(stepsSnapshot.String)
	resp := map[string]any{
		"traceId":      traceID,
		"workflowId":   nullInt64Val(wrid),
		"workflowName": wfName,
		"status":       workflowDisplayStatus(status, cancelled),
		"triggeredBy":  nullStrVal(triggeredBy),
		"scheduleName": nullStrVal(scheduleName),
		"startedAt":    nullStrVal(startedAt),
		"completedAt":  nullStrVal(completedAt),
		"durationMs":   nullInt64ToPtr(durationMs),
		"cancelled":    cancelled == 1,
		"jobTraceIds":  jobIDs,
		"steps":        s.workflowRunSteps(r, children, containers),
		"graph":        workflowRunGraph(stepsSnapshot.String, stats),
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// workflowDisplayStatus maps a workflow run's raw status to its display status,
// surfacing a soft-cancelled run (WB-S2) as 'cancelled' even though the raw column
// holds a CHECK-legal terminal ('failure').
func workflowDisplayStatus(raw string, cancelled int) string {
	if cancelled == 1 {
		return "cancelled"
	}
	return mapRunStatus(raw)
}

// cancelWorkflowRun soft-cancels a running workflow run (WB-S2 / D4). It shares the
// trigger engine instance so it can stop the in-flight walk; for runs driven by
// another engine (scheduler), the persisted flag halts the walk between steps.
func (s *Server) cancelWorkflowRun(eng *workflow.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		traceID := r.PathValue("traceId")
		var status string
		var stepsSnapshot sql.NullString
		err := s.db.QueryRowContext(r.Context(), `
			SELECT status, steps_snapshot FROM workflow_runs WHERE id = ?
		`, traceID).Scan(&status, &stepsSnapshot)
		if errors.Is(err, sql.ErrNoRows) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "workflow run not found")
			return
		}
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}

		// Scope access check (FR-H2 IDOR mitigation) + the killJobs verb (RF-6,
		// the RBAC-fixes plan). Cancelling is run suppression — the same
		// act as killJob — and v0.56.4 wired the verb into every execution route
		// EXCEPT this one. Mirrors workflowScopesPermit: the verb is required on
		// every constituent scope, an unbound child is unrestricted-only
		// (CanUnbound), and there is deliberately NO id.Unrestricted() fast path for
		// the verb half — unrestricted describes scope reach, not authority, and an
		// unrestricted viewer holds no verb at all.
		children := s.loadWorkflowChildRuns(r, traceID)
		id, hasID := auth.IdentityFrom(r.Context())
		// Fail CLOSED on a missing identity. The route is mounted behind
		// requireSession so this should be unreachable, but the guard below used to
		// live entirely inside `if hasID` — i.e. no identity meant no checks at all.
		// An authorization guard must not be the thing that trusts an invariant
		// enforced somewhere else.
		if !hasID {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		// Baseline covers the degenerate case (no children yet, no snapshot):
		// a caller with the verb nowhere may cancel nothing.
		if !id.CanAnywhere(auth.PermKillJobs) {
			if s.auth != nil {
				s.auth.AuditDenied(r, id.Email, "insufficient_permission", auth.PermKillJobs,
					auditDetails(r, "cancelling a workflow run requires the Kill Jobs permission"))
			}
			httpx.Fail(w, http.StatusForbidden, "forbidden",
				"cancelling a workflow run requires the Kill Jobs permission")
			return
		}
		for _, cr := range children {
			if cr.scope == "" {
				// An unbound child run (system-global or pre-RB-26): suppressing it
				// is unrestricted-only, matching killJob's unbound branch.
				if !id.CanUnbound(auth.PermKillJobs) {
					if s.auth != nil {
						s.auth.AuditDenied(r, id.Email, "insufficient_scope", auth.AllScopes,
							auditDetails(r, "workflow run includes an unbound child run ("+auth.PermKillJobs+")"))
					}
					httpx.Fail(w, http.StatusForbidden, "forbidden",
						"this workflow run includes a run with no scope; only an unrestricted operator may cancel it")
					return
				}
				continue
			}
			if !auth.ScopeReadable(id, cr.scope) {
				s.denyScope(w, r, cr.scope, "scope access denied")
				return
			}
			if !s.requireCan(w, r, id, auth.PermKillJobs, cr.scope) {
				return
			}
		}

		if stepsSnapshot.Valid && stepsSnapshot.String != "" {
			steps, err := workflow.ParseSteps(stepsSnapshot.String)
			if err == nil {
				jobNames := workflow.FlattenSteps(steps)
				if len(jobNames) > 0 {
					placeholders := strings.Repeat("?,", len(jobNames))
					placeholders = placeholders[:len(placeholders)-1]
					query := fmt.Sprintf("SELECT DISTINCT COALESCE(scope,'') FROM jobs WHERE name IN (%s)", placeholders)
					args := make([]any, len(jobNames))
					for i, name := range jobNames {
						args[i] = name
					}
					rows, err := s.db.QueryContext(r.Context(), query, args...)
					if err == nil {
						defer rows.Close()
						for rows.Next() {
							var sc string
							if err := rows.Scan(&sc); err == nil && sc != "" {
								if !auth.ScopeReadable(id, sc) {
									s.denyScope(w, r, sc, "scope access denied")
									return
								}
								if !s.requireCan(w, r, id, auth.PermKillJobs, sc) {
									return
								}
							}
						}
					}
				}
			}
		}

		// SW: cancel the whole tree. A sub-workflow is its own run with its own
		// walk goroutine, so cancelling only the parent would stop the parent's
		// WAIT and leave the child running to completion, doing work nobody is
		// waiting for. CancelTree recurses parent_workflow_run_id.
		if !eng.CancelTree(r.Context(), traceID) {
			httpx.Fail(w, http.StatusConflict, "conflict", "workflow run is not running (already finished or cancelled)")
			return
		}
		if id, ok := auth.IdentityFrom(r.Context()); ok {
			_ = workflow.InsertChangeLog(r.Context(), s.db, id.Email, "Workflows", "Cancelled", traceID, "workflow run cancelled")
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (s *Server) childTraceIDs(r *http.Request, wfTraceID string) []string {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id FROM runs WHERE workflow_run_id = ? ORDER BY created_at
	`, wfTraceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	if ids == nil {
		ids = []string{}
	}
	return ids
}

// wfChildRun is one child run of a workflow run, loaded for the run-detail
// serializer (the flat timeline + the merged graph statuses + redacted context).
type wfChildRun struct {
	id, jobName, runType, status, scope string
	startedAt, completedAt              sql.NullString
	durationMs                          sql.NullInt64
	envJSON, outputsJSON                sql.NullString
}

// nodeStat is a graph node's live outcome, merged onto the snapshot by node id.
type nodeStat struct {
	status     string
	durationMs any // *int64 or nil, via nullInt64ToPtr
}

// loadWorkflowChildRuns drains the child runs of a workflow run into a slice. It
// MUST drain fully before any per-row sub-query runs (the context redactor builds
// query secrets; SQLite pool rule, see db.maxOpenConns).
func (s *Server) loadWorkflowChildRuns(r *http.Request, wfTraceID string) []wfChildRun {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, job_name, run_type, status, COALESCE(scope,''), started_at, completed_at, duration_ms, env_json, outputs_json
		FROM runs WHERE workflow_run_id = ? ORDER BY created_at
	`, wfTraceID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []wfChildRun
	for rows.Next() {
		var cr wfChildRun
		if err := rows.Scan(&cr.id, &cr.jobName, &cr.runType, &cr.status, &cr.scope,
			&cr.startedAt, &cr.completedAt, &cr.durationMs, &cr.envJSON, &cr.outputsJSON); err != nil {
			continue
		}
		out = append(out, cr)
	}
	return out
}

// workflowRunSteps builds the flat step timeline from pre-loaded child runs. Each
// row carries its real step kind (WB-O3, from the snapshot's node→container map),
// the child trace id (so the UI fetches GET /runs/{id}/log — no new endpoint, WB-O1
// /O4), and a redacted resolved-context snapshot (WB-O1/D5).
func (s *Server) workflowRunSteps(r *http.Request, children []wfChildRun, containers map[string]string) []map[string]any {
	redactors := map[string]*runner.Redactor{} // one per scope, built lazily
	getRedactor := func(scope string) *runner.Redactor {
		if red, ok := redactors[scope]; ok {
			return red
		}
		red, _ := runner.NewRedactor(r.Context(), s.db, s.cfg, scope)
		redactors[scope] = red
		return red
	}
	steps := make([]map[string]any, 0, len(children))
	for i, cr := range children {
		stepType := containers[cr.id]
		if stepType == "" {
			stepType = "job"
		}
		steps = append(steps, map[string]any{
			"id":              cr.id,
			"stepIndex":       i,
			"stepType":        stepType,
			"stepName":        cr.jobName,
			"status":          mapRunStatus(cr.status),
			"jobType":         cr.runType,
			"startedAt":       nullStrVal(cr.startedAt),
			"finishedAt":      nullStrVal(cr.completedAt),
			"durationMs":      nullInt64ToPtr(cr.durationMs),
			"detail":          nil,
			"logFilePath":     nil,
			"contextSnapshot": runContextSnapshot(cr, getRedactor),
		})
	}
	return steps
}

// runContextSnapshot builds a step's resolved context for the run timeline (WB-O1
// /D5): the child run's resolved env plus its captured A12 outputs, both run
// through the existing redaction path (outputs win on key collision). Returns nil
// when there's nothing to show, so the redactor is built only when needed.
func runContextSnapshot(cr wfChildRun, getRedactor func(string) *runner.Redactor) map[string]string {
	env := parseEnvMap(cr.envJSON)
	outputs := parseOutputsJSON(cr.outputsJSON)
	if len(env) == 0 && len(outputs) == 0 {
		return nil
	}
	red := getRedactor(cr.scope)
	redact := func(v string) string {
		if red == nil {
			return "[REDACTED]" // fail-closed, matching redactOutputs
		}
		return string(red.Redact([]byte(v)))
	}
	ctx := make(map[string]string, len(env)+len(outputs))
	for k, v := range env {
		ctx[k] = redact(v)
	}
	for k, v := range outputs {
		ctx[k] = redact(v)
	}
	return ctx
}

// snapshotNodeContainers maps each job node's id to the kind of its TOP-LEVEL
// container step (job|parallel|branch) so the flat run timeline can label real
// step types (WB-O3). Empty map for a pre-240 run with no snapshot.
func snapshotNodeContainers(snapshot string) map[string]string {
	steps, err := workflow.ParseSteps(snapshot)
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	for _, st := range steps {
		kind := st.Type
		if kind == "" {
			kind = "job"
		}
		var ids []string
		collectNodeIDs(st, &ids)
		for _, id := range ids {
			m[id] = kind
		}
	}
	return m
}

// collectNodeIDs appends every job node's id under a step (recursing into branch
// arms and parallel blocks), mirroring the engine's flattenJobNodes ordering.
func collectNodeIDs(st workflow.Step, out *[]string) {
	switch st.Type {
	case "parallel":
		for _, j := range st.Jobs {
			collectNodeIDs(j, out) // PS-1: an arm may be a sequence, not just a leaf
		}
	case "sequence":
		for _, ss := range st.Steps {
			collectNodeIDs(ss, out)
		}
	case "branch":
		for _, arm := range []*workflow.Branch{st.Pass, st.Fail} {
			if arm == nil {
				continue
			}
			for _, as := range arm.Steps {
				collectNodeIDs(as, out)
			}
		}
	default:
		if st.NodeID != "" {
			*out = append(*out, st.NodeID)
		}
	}
}

// workflowRunGraph reconstructs the executed step graph (WB-O3) from the per-run
// snapshot (WB-D1) with each node's live status/duration merged by node id, shaped
// for the frontend StepChain (pass/fail as flat job arrays, condition as {label}).
// Returns nil for a pre-240 run with no snapshot (the UI falls back to the timeline).
func workflowRunGraph(snapshot string, stats map[string]nodeStat) []map[string]any {
	steps, err := workflow.ParseSteps(snapshot)
	if err != nil || len(steps) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(steps))
	for _, st := range steps {
		out = append(out, graphNode(st, stats))
	}
	return out
}

func graphNode(st workflow.Step, stats map[string]nodeStat) map[string]any {
	typ := st.Type
	if typ == "" {
		typ = "job"
	}
	n := map[string]any{"type": typ}
	if st.Name != "" {
		n["name"] = st.Name
	}
	if st.Label != "" {
		n["label"] = st.Label
	}
	switch typ {
	case "parallel":
		jobs := make([]map[string]any, 0, len(st.Jobs))
		for _, j := range st.Jobs {
			jobs = append(jobs, graphNode(j, stats)) // PS-1: arms may be sequences
		}
		n["jobs"] = jobs
	case "sequence":
		steps := make([]map[string]any, 0, len(st.Steps))
		for _, ss := range st.Steps {
			steps = append(steps, graphNode(ss, stats))
		}
		n["steps"] = steps
	case "workflow":
		// SW: rendered as a COLLAPSED node. The child's own steps are not
		// inlined — it has its own run detail to drill into, and inlining would
		// make the parent's canvas grow without bound with someone else's graph.
		n["workflow"] = st.Workflow
		if st.NodeID != "" {
			n["id"] = st.NodeID
		}
	case "branch":
		if st.Condition != nil {
			// label is what StepChain shows; the structured fields let the canvas
			// renderer (WC-R1) rebuild its own condition node without re-deriving.
			n["condition"] = map[string]any{
				"label":    conditionLabel(st.Condition),
				"type":     st.Condition.Type,
				"jobRef":   st.Condition.JobRef,
				"field":    st.Condition.Field,
				"operator": st.Condition.Operator,
				"value":    st.Condition.Value,
			}
		}
		n["pass"] = graphArm(st.Pass, stats)
		n["fail"] = graphArm(st.Fail, stats)
	default:
		applyNodeStat(n, st.NodeID, stats)
	}
	return n
}

// graphArm serializes a branch arm as the FULL nested step sequence (WC-R1):
// each arm element recurses through graphNode, so a parallel or nested branch
// inside an arm keeps its container shape for the canvas renderer instead of
// being flattened to its leaf jobs (the old StepChain-only behavior — StepChain
// still renders these lists; a container element shows as its labelled chip).
func graphArm(b *workflow.Branch, stats map[string]nodeStat) []map[string]any {
	out := []map[string]any{}
	if b == nil {
		return out
	}
	for _, st := range b.Steps {
		out = append(out, graphNode(st, stats))
	}
	return out
}

func applyNodeStat(n map[string]any, id string, stats map[string]nodeStat) {
	if id != "" {
		n["id"] = id
	}
	if cs, ok := stats[id]; ok {
		n["status"] = cs.status
		n["durationMs"] = cs.durationMs
	}
}

// conditionLabel renders a branch condition as a short human string for the graph.
func conditionLabel(c *workflow.Condition) string {
	switch c.Type {
	case "job_status":
		return c.JobRef + " succeeded"
	case "output_match":
		return strings.TrimSpace(c.JobRef + "." + c.Field + " " + c.Operator + " " + c.Value)
	}
	return c.JobRef
}

// ─────────────────────────────────────────────────────────────────────────────
// Runs / History
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	jobFilter := q.Get("job")
	jobUIDFilter := q.Get("jobUid")
	typeFilter := q.Get("type")
	statusFilter := q.Get("status")
	kindFilter := q.Get("kind")
	userFilter := q.Get("user")
	runnerFilter := q.Get("runnerId")
	calendarFilter := q.Get("calendar")
	stoppedFilter := q.Get("stopped")
	triggerKindFilter := q.Get("triggerKind")
	reactedToFilter := q.Get("reactedTo")
	traceFilter := q.Get("trace")
	from := q.Get("from")
	to := q.Get("to")
	page, pageSize := pageParams(q)

	// Scope access filter (A5 fix): an unrestricted actor ("*") sees every run; a
	// restricted actor sees only GLOBAL (scope IS NULL) rows plus its granted scopes;
	// an empty grant (or no identity) sees GLOBAL only. Computed once here, applied to
	// the WHERE clause below.
	id, hasID := auth.IdentityFrom(r.Context())
	unrestricted := hasID && id.Unrestricted()
	var allowedScopes []string
	if hasID {
		allowedScopes = id.AllowedScopes
	}

	base := `FROM runs WHERE 1=1`
	var args []any

	// R2-1 — filter by the job's permanent identity. Preferred over ?job=, which
	// stays a NAME filter forever and therefore answers for every job carrying
	// that name once AF-4b lets two agencies share one. The two compose: passing
	// both narrows to that name AND that identity, which only the intended job
	// satisfies.
	if jobUIDFilter != "" {
		base += " AND job_uid = ?"
		args = append(args, jobUIDFilter)
	}
	if jobFilter != "" {
		base += " AND job_name = ?"
		args = append(args, jobFilter)
	}
	if typeFilter != "" {
		base += " AND run_type = ?"
		args = append(args, typeFilter)
	}
	if statusFilter != "" {
		frag, fargs := dbStatusFilter("status", statusFilter)
		base += " AND " + frag
		args = append(args, fargs...)
	}
	if kindFilter != "" {
		base += " AND kind = ?"
		args = append(args, kindFilter)
	}
	if userFilter != "" {
		base += " AND triggered_by = ?"
		args = append(args, userFilter)
	}
	if runnerFilter != "" {
		base += " AND runner_id = ?"
		args = append(args, runnerFilter)
	}
	// CAL-29 — "show me every run suppressed by federal-holidays this fiscal year".
	// Filters the structured provenance column, NEVER a LIKE against
	// queued_reason: a copy edit to the reason wording must not be able to
	// silently gut the audit query. `?calendar=*` matches any calendar
	// suppression, for "show me everything we skipped on policy grounds".
	if calendarFilter == "*" {
		base += " AND suppressed_by_calendar IS NOT NULL"
	} else if calendarFilter != "" {
		base += " AND suppressed_by_calendar = ?"
		args = append(args, calendarFilter)
	}
	// RX-22 — "stopped by a human", orthogonal to status. `killed_by IS NOT NULL`
	// has always been the truth about operator intervention; it was just never
	// queryable. Dispositions (RX-4) make it load-bearing: a stopped run can now
	// be recorded success/warning/failure, so status alone no longer answers "did
	// a person end this?". Deliberately NOT folded into the status filter — the
	// two compose, so ?status=success&stopped=true reads "runs a human stopped
	// and called success", which is precisely the new state Phase A creates.
	if stoppedFilter == "true" {
		base += " AND killed_by IS NOT NULL"
	} else if stoppedFilter == "false" {
		base += " AND killed_by IS NULL"
	}
	// RX-17 — "show me everything a reaction caused". trigger_kind is the run's
	// own provenance, so this is one predicate rather than a join.
	if triggerKindFilter != "" {
		base += " AND trigger_kind = ?"
		args = append(args, triggerKindFilter)
	}
	// RX-17, the REVERSE direction — "what did this run trigger?". The forward
	// link (a run naming its cause) is a field on the row; the reverse is this
	// query, and it has to exist as its own filter because a run has no list of
	// its children and building one would duplicate a fact the child already
	// owns.
	if reactedToFilter != "" {
		base += " AND reacted_to_run_id = ?"
		args = append(args, reactedToFilter)
	}
	// RX-17 — pivot to ONE run by its trace id. The forward because-of link
	// needs somewhere to go: naming the upstream run without being able to open
	// it makes an operator copy an id into a search box mid-incident. A filter
	// rather than a detail route because History's row already IS the detail —
	// expanding it shows the log.
	if traceFilter != "" {
		base += " AND id = ?"
		args = append(args, traceFilter)
	}
	if from != "" {
		base += " AND created_at >= ?"
		args = append(args, from)
	}
	if to != "" {
		base += " AND created_at < ?"
		args = append(args, to)
	}
	if !unrestricted {
		if len(allowedScopes) > 0 {
			placeholders := strings.Repeat("?,", len(allowedScopes))
			placeholders = placeholders[:len(placeholders)-1]
			base += " AND (scope IS NULL OR scope IN (" + placeholders + "))"
			for _, sc := range allowedScopes {
				args = append(args, sc)
			}
		} else {
			// Empty grant ⇒ GLOBAL only (the A5 fix: empty no longer means "all").
			base += " AND scope IS NULL"
		}
	}

	// TS-20: ?sort=&order= against an allowlist (never interpolated). Status
	// sorts by the worst-first rank the UI uses, not alphabetically; the
	// created_at/id tiebreak keeps LIMIT/OFFSET pages from shearing under
	// equal keys. Default order unchanged.
	orderBy, sortErr := sortparam.OrderBy(q, map[string]string{
		"job":       "job_name",
		"type":      "run_type",
		"executor":  "executor",
		"schedule":  "schedule_name",
		"started":   "started_at",
		"completed": "completed_at",
		"user":      "triggered_by",
		"duration":  "duration_ms",
		"status":    sortparam.RunStatusRank,
	}, " ORDER BY created_at DESC", "created_at DESC, id DESC")
	if sortErr != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_sort", sortErr.Error())
		return
	}

	// One filter slice: the COUNT uses it as-is, then LIMIT/OFFSET are appended for
	// the page query below (CC.14 — was a parallel args/cArgs double-append).
	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) "+base, args...).Scan(&total)

	query := `SELECT id, job_name, run_type, scope, status, queued_reason,
		triggered_by, trigger_kind, killed_by, workflow_run_id,
		started_at, completed_at, duration_ms, exit_code, created_at, schedule_name, kind, executor,
		script_ref, content_hash, job_source, outputs_json, override_json, agencies_json, runner_id,
		suppressed_by_calendar, reacted_to_run_id, reaction_depth, runner_tag, job_uid, log_archived_at ` +
		base + orderBy + " LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	// Drain fully before runToMap's per-row jobId lookup (see listJobs note).
	var raws []runRaw
	for rows.Next() {
		if rr, ok := scanRunRaw(rows); ok {
			raws = append(raws, rr)
		}
	}
	rows.Close()

	items := []map[string]any{}
	caches := s.newRunMapCaches(r.Context(), raws)
	for _, rr := range raws {
		items = append(items, s.runToMap(rr, caches))
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	traceID := r.PathValue("traceId")
	run := s.fetchRunByID(r, traceID)
	if run == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "run not found")
		return
	}

	// Scope access check (FR-H2 IDOR mitigation, A5 fix): an unrestricted actor sees
	// every run; everyone else may read only global runs plus their granted scopes.
	id, hasID := auth.IdentityFrom(r.Context())
	if hasID && !id.Unrestricted() {
		var scope string
		if scVal, ok := run["scope"]; ok && scVal != nil {
			if sStr, ok := scVal.(string); ok {
				scope = sStr
			}
		}
		if !auth.ScopeReadable(id, scope) {
			s.denyScope(w, r, scope, "scope access denied")
			return
		}
	}

	httpx.JSON(w, http.StatusOK, run)
}

func (s *Server) fetchRunByID(r *http.Request, traceID string) map[string]any {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, job_name, run_type, scope, status, queued_reason,
		       triggered_by, trigger_kind, killed_by, workflow_run_id,
		       started_at, completed_at, duration_ms, exit_code, created_at, schedule_name, kind, executor,
		       script_ref, content_hash, job_source, outputs_json, override_json, agencies_json, runner_id,
		       suppressed_by_calendar, reacted_to_run_id, reaction_depth, runner_tag, job_uid, log_archived_at
		FROM runs WHERE id = ?
	`, traceID)
	if err != nil {
		return nil
	}
	var rr runRaw
	var ok bool
	if rows.Next() {
		rr, ok = scanRunRaw(rows)
	}
	rows.Close()
	if !ok {
		return nil
	}
	caches := s.newRunMapCaches(r.Context(), []runRaw{rr})
	caches.detail = true // enable per-run injected-value output masking (H2)
	return s.runToMap(rr, caches)
}

// runRaw holds a single scanned run row before any per-row follow-up query.
type runRaw struct {
	id, jobName, runType, status                  string
	kind                                          string
	scope, queuedReason, triggeredBy, triggerKind sql.NullString
	killedBy, workflowRunID                       sql.NullString
	startedAt, completedAt, createdAt             sql.NullString
	durationMs, exitCode                          sql.NullInt64
	scheduleName                                  sql.NullString
	executor                                      sql.NullString
	scriptRef, contentHash                        sql.NullString // §3.3 run reproducibility snapshot
	jobSource                                     sql.NullString // A9 run origin (NULL ⇒ git)
	outputsJSON                                   sql.NullString // A12 captured inter-job outputs (JSON map)
	overrideJSON                                  sql.NullString // F3 ad-hoc override envelope (JSON object)
	agenciesJSON                                  sql.NullString // T3.6 — the run's frozen agency SET snapshot (mig. 680); "[]" = general pool
	runnerID                                      sql.NullString // UUID of the executing runner; NULL for SSH runs / deregistered runners
	runnerTag                                     sql.NullString // RT-2 the pin frozen at trigger; NULL = unpinned. Intent, not outcome — see runnerID
	// suppressedByCalendar names the working calendar that suppressed this fire
	// (CAL-29). NULL on every run that actually happened. The audit query filters
	// on THIS, never on queued_reason's wording.
	suppressedByCalendar sql.NullString
	// logArchivedAt: when the sweep verified this run's log in the S3 archive
	// tier (SL-1/SL-3); NULL = not archived. History names it on the row.
	logArchivedAt sql.NullString
	// RX-17 — the durable because-of link and the chain position. Lives on the
	// run row rather than only in reaction_deliveries because that log is
	// retention-pruned, and a provenance link that evaporates with it would
	// contradict History owning the permanent trail.
	reactedToRunID sql.NullString
	reactionDepth  sql.NullInt64
	// jobUid is the executed job's permanent identity, frozen at enqueue (R2-1).
	// NULL on rows written before migration 1010 whose job was already gone, and
	// on runs whose job vanished between resolution and insert. It is what
	// History attributes by once two agencies may share a job name.
	jobUID sql.NullString
}

// scanRunRaw scans the current row into a runRaw. It runs NO nested query, so it
// is safe to call while the outer rows iterator is still open.
func scanRunRaw(rows *sql.Rows) (runRaw, bool) {
	var rr runRaw
	if err := rows.Scan(&rr.id, &rr.jobName, &rr.runType, &rr.scope, &rr.status, &rr.queuedReason,
		&rr.triggeredBy, &rr.triggerKind, &rr.killedBy, &rr.workflowRunID,
		&rr.startedAt, &rr.completedAt, &rr.durationMs, &rr.exitCode, &rr.createdAt, &rr.scheduleName,
		&rr.kind, &rr.executor, &rr.scriptRef, &rr.contentHash, &rr.jobSource, &rr.outputsJSON, &rr.overrideJSON, &rr.agenciesJSON, &rr.runnerID,
		&rr.suppressedByCalendar, &rr.reactedToRunID, &rr.reactionDepth, &rr.runnerTag, &rr.jobUID, &rr.logArchivedAt); err != nil {
		return runRaw{}, false
	}
	return rr, true
}

// runMapCaches carries the per-request state runToMap needs: one lazily-built
// redactor per distinct scope (each build envelope-decrypts every stored
// secret, so a 200-row page must not build 200 of them — CC.3) and one batched
// jobs-rowid map replacing the former per-row reverse lookup. Its queries run
// only while no outer rows iterator is open (see db.maxOpenConns).
type runMapCaches struct {
	s         *Server
	ctx       context.Context
	redactors map[string]*runner.Redactor // nil entry ⇒ build failed ⇒ fail closed
	jobIDs    map[string]int64            // runJobKey(source, name) → jobs.rowid
	// detail is set for a single-run detail render. Only then does redactOutputs
	// pay the per-run injected-value resolution (a possible Vault round-trip), so a
	// large runs-list page is never slowed for the rare row that carries outputs
	// (H2 scopes the vault-source unmasking concern to the run-detail API).
	detail bool
	// injectResolver is the lazily-built, Vault-wired resolver used to recover a
	// run's injected secret values for output masking (H2). Built at most once per
	// request; injectResolverBuilt guards the (possibly nil) result.
	injectResolver      *runref.Resolver
	injectResolverBuilt bool
}

// newRunRedactor is a test seam so the CC.3 regression test can count redactor
// builds per request.
var newRunRedactor = runner.NewRedactor

func runJobKey(source, name string) string { return source + "\x00" + name }

// newRunMapCaches batch-loads the jobs-rowid map for a drained page of runs.
// One query for every distinct job name on the page, pair-matched on (source,
// name) below so a run without a matching job row keeps jobId 0, as before.
func (s *Server) newRunMapCaches(ctx context.Context, raws []runRaw) *runMapCaches {
	c := &runMapCaches{s: s, ctx: ctx, redactors: map[string]*runner.Redactor{}, jobIDs: map[string]int64{}}
	if len(raws) == 0 {
		return c
	}
	nameSet := map[string]struct{}{}
	for _, rr := range raws {
		nameSet[rr.jobName] = struct{}{}
	}
	placeholders := make([]string, 0, len(nameSet))
	names := make([]any, 0, len(nameSet))
	for n := range nameSet {
		placeholders = append(placeholders, "?")
		names = append(names, n)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT rowid, source, name FROM jobs WHERE name IN (`+strings.Join(placeholders, ",")+`)`, names...)
	if err != nil {
		// Degrade like the old per-row lookup did (jobId 0), but say so: one
		// failure here blanks jobId for the whole page, not a single row.
		s.log.Warn("runs list: batched jobId lookup failed; jobId will be 0 for this page", "error", err)
		return c
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var source, name string
		if rows.Scan(&id, &source, &name) == nil {
			c.jobIDs[runJobKey(source, name)] = id
		}
	}
	if err := rows.Err(); err != nil {
		s.log.Warn("runs list: batched jobId lookup ended early; some jobIds may be 0", "error", err)
	}
	return c
}

// redactor returns the per-scope redactor, building it at most once per
// request on success. A failed build returns nil (redactOutputs masks that
// row's outputs, fail-closed) but is NOT cached, so the next row retries —
// matching the old per-row build, where a transient error masked only the
// row that hit it rather than the whole page.
func (c *runMapCaches) redactor(scope string) *runner.Redactor {
	if red, ok := c.redactors[scope]; ok {
		return red
	}
	red, err := newRunRedactor(c.ctx, c.s.db, c.s.cfg, scope)
	if err != nil || red == nil {
		c.s.log.Warn("runs list: redactor build failed; masking this row's outputs", "scope", scope, "error", err)
		return nil
	}
	c.redactors[scope] = red
	return red
}

// runToMap turns a scanned run into the API response shape. Its lookups go
// through the per-request caches, whose lazy redactor builds issue queries, so
// it MUST be called after the source rows iterator is closed (otherwise it
// would deadlock against a pinned connection — see db.maxOpenConns).
func (s *Server) runToMap(rr runRaw, caches *runMapCaches) map[string]any {
	manual := rr.triggerKind.Valid && rr.triggerKind.String == "manual"

	// Source-qualified jobId reverse lookup (A9): a NULL job_source (legacy/git run)
	// resolves the git-source job.
	jobSrc := rr.jobSource.String
	if jobSrc == "" {
		jobSrc = "git"
	}
	jobID := caches.jobIDs[runJobKey(jobSrc, rr.jobName)]

	// M4 — read-time stuck-run signal: a queued runner run whose agency has no online
	// eligible runner will wait indefinitely; surface why (unless a stored reason wins).
	// Deliberately context.Background(): the helper conflates a query error with
	// "no online runner", so a canceled request context must not fabricate the reason.
	statusReason := nullStrVal(rr.queuedReason)
	if (!rr.queuedReason.Valid || rr.queuedReason.String == "") &&
		rr.status == "queued" && runExecutor(rr) == "runner" {
		runAgencies := parseAgenciesJSON(rr.agenciesJSON)
		if ok, _ := execspec.AgenciesHaveOnlineRunner(context.Background(), s.db, runAgencies); !ok {
			// No online runner in the run's pool at all — the coarsest cause. T3.7:
			// name the SET, because "no online runner in agency 'DSS'" is actively
			// misleading once a run can require any of several.
			if len(runAgencies) > 0 {
				statusReason = "Waiting: no online runner in " + joinAgencyNames(runAgencies)
			} else {
				statusReason = "Waiting: no online general-pool runner (all runners are agency-bound)"
			}
		} else if eligible, requires, _ := execspec.EligibleOnlineRunnerForRun(context.Background(), s.db, rr.id); !eligible {
			// A runner exists in the pool but none satisfies this run's
			// requirement tokens (or run-type) — name the unmet requirements
			// (§5 requirements-aware stuck-run hint).
			if len(requires) > 0 {
				statusReason = "Waiting: no online runner satisfies this run's requirements [" + strings.Join(requires, ", ") + "]"
			} else {
				statusReason = "Waiting: no online runner supports run-type '" + rr.runType + "'"
			}
		}
	}

	return map[string]any{
		"traceId":      rr.id,
		"jobId":        jobID,
		"jobUid":       nullStrVal(rr.jobUID),
		"jobName":      rr.jobName,
		"jobSource":    jobSrc,
		"type":         rr.runType,
		"outputs":      caches.redactOutputs(rr, parseOutputsJSON(rr.outputsJSON)),
		"overrides":    parseOverrideJSON(rr.overrideJSON),
		"kind":         rr.kind,
		"scope":        nullStrVal(rr.scope),
		"agencies":     parseAgenciesJSON(rr.agenciesJSON),
		"status":       mapRunStatus(rr.status),
		"statusReason": statusReason,
		"executor":     runExecutor(rr),
		// RT-2 — the pin this run was DISPATCHED with, frozen at trigger time.
		// Distinct from runnerId below, which is the runner that actually claimed
		// it: one is intent, the other outcome, and a run that is queued has the
		// first without the second (RT-G6). Empty for an unpinned run.
		"runnerTag":       nullStrVal(rr.runnerTag),
		"manual":          manual,
		"triggeredBy":     nullStrVal(rr.triggeredBy),
		"killedBy":        nullStrVal(rr.killedBy),
		"runnerId":        nullStrVal(rr.runnerID),
		"workflowTraceId": nullStrVal(rr.workflowRunID),
		"scheduleName":    nullStrVal(rr.scheduleName),
		// CAL-29 — surfaced so History can show WHICH calendar suppressed a run,
		// and so the filter's results are self-explaining.
		"suppressedByCalendar": nullStrVal(rr.suppressedByCalendar),
		// RX-17 — "why did this run?" answered by pointing. Null on every run
		// that was not caused by another run, which is every run that existed
		// before reactions.
		// RX-17 — how the run was CAUSED. The ?triggerKind= filter reads this
		// column, so not serving it left the History rows unable to show what the
		// filter had just selected on.
		"triggerKind":    nullStrVal(rr.triggerKind),
		"reactedToRunId": nullStrVal(rr.reactedToRunID),
		"reactionDepth":  rr.reactionDepth.Int64,
		"scriptRef":      nullStrVal(rr.scriptRef),
		"contentHash":    nullStrVal(rr.contentHash),
		"queuedAt":       nullStrVal(rr.createdAt),
		"startedAt":      nullStrVal(rr.startedAt),
		"completedAt":    nullStrVal(rr.completedAt),
		"durationMs":     runDuration(rr),
		"exitCode":       nullInt64ToPtr(rr.exitCode),
		// SL-3 — when the log was verified in the S3 archive tier; null until
		// the sweep gets to it (or forever, on a local backend).
		"logArchivedAt": nullStrVal(rr.logArchivedAt),
	}
}

// runExecutor returns the frozen executor for a run (R5.1). The runs.executor
// column is NOT NULL with a 'ssh' default, so a NULL only arises from
// pre-column historical rows — default those to ssh.
func runExecutor(rr runRaw) string {
	if rr.executor.Valid && rr.executor.String != "" {
		return rr.executor.String
	}
	return "ssh"
}

// runDuration returns the stored duration_ms, or computes it from the started/
// completed timestamps when the stored value is NULL (LB1 defensive fallback so
// historical rows that predate the sshexec duration fix still render an elapsed
// time instead of an em-dash).
func runDuration(rr runRaw) *int64 {
	if rr.durationMs.Valid {
		v := rr.durationMs.Int64
		return &v
	}
	if rr.startedAt.Valid && rr.completedAt.Valid {
		st, e1 := time.Parse(time.RFC3339, rr.startedAt.String)
		ct, e2 := time.Parse(time.RFC3339, rr.completedAt.String)
		if e1 == nil && e2 == nil && !ct.Before(st) {
			ms := ct.Sub(st).Milliseconds()
			return &ms
		}
	}
	return nil
}

// parseOverrideJSON decodes a run's F3 ad-hoc override envelope for the API
// response (nil — JSON null — if absent/invalid). The envelope is operator-supplied
// input, surfaced for audit like the schedule env (§3.7); secret containment is the
// log redactor (§4.3), so it is NOT redacted here — the UI carries the plaintext
// caveat.
func parseOverrideJSON(v sql.NullString) map[string]any {
	if !v.Valid || v.String == "" {
		return nil
	}
	m := map[string]any{}
	if json.Unmarshal([]byte(v.String), &m) != nil {
		return nil
	}
	return m
}

// parseOutputsJSON decodes a run's captured A12 outputs map (nil if absent/invalid).
func parseOutputsJSON(v sql.NullString) map[string]string {
	if !v.Valid || v.String == "" {
		return nil
	}
	m := map[string]string{}
	if json.Unmarshal([]byte(v.String), &m) != nil {
		return nil
	}
	return m
}

// redactOutputs masks secret values in A12 captured outputs at DISPLAY time
// (PP-B4c). The stored outputs_json stays raw because the workflow engine injects
// those values into downstream steps; only the API response is redacted, the same
// way log lines are. Fail-closed: if the redactor can't be built, every value is
// masked rather than leaked. No-op for the common (empty) case, so the per-scope
// redactor is only built when a row actually carries outputs.
//
// H2: the per-scope redactor is built from the STORED-secret global dictionary,
// so a vault-source injected value (absent from that dictionary) would render
// unmasked. On the detail path we additionally resolve THIS run's injected secret
// values and fold them into a one-off redactor. This is defense-in-depth — the
// runner-side refuse (runner/log.go) already keeps an injected value out of
// outputs_json for any run dispatched after the fix — covering legacy rows and
// any residual capture regardless of DEC-2. Confined to detail so a runs-list
// page never pays the resolution's possible Vault round-trip per row.
func (c *runMapCaches) redactOutputs(rr runRaw, outputs map[string]string) map[string]string {
	if len(outputs) == 0 {
		return outputs
	}
	scope := rr.scope.String

	var injected []string
	injectOK := true
	if c.detail {
		injected, injectOK = c.injectedRedactionValues(rr)
	}

	// With injected values, build a one-off redactor seeded with both the per-scope
	// stored secrets AND the injected dictionary (reusing NewRedactor's short-value
	// filtering and longest-first ordering); otherwise reuse the cached per-scope
	// redactor.
	var red *runner.Redactor
	if len(injected) > 0 {
		if r, err := newRunRedactor(c.ctx, c.s.db, c.s.cfg, scope, injected...); err == nil {
			red = r
		}
	} else {
		red = c.redactor(scope)
	}

	// Fail closed: a failed redactor build OR an unresolved injected dictionary
	// (e.g. a Vault outage for a run that DID declare secrets) must not render a
	// possibly-secret value in the clear.
	if red == nil || !injectOK {
		masked := make(map[string]string, len(outputs))
		for k := range outputs {
			masked[k] = "[REDACTED]"
		}
		return masked
	}
	out := make(map[string]string, len(outputs))
	for k, v := range outputs {
		out[k] = string(red.Redact([]byte(v)))
	}
	return out
}

// injectedRedactionValues resolves the run's injected SECRET values (vault-source
// secrets, key material — the values absent from the global stored-secret
// dictionary) so redactOutputs can mask them (H2). It returns (values, ok); ok is
// false only when resolution genuinely failed for a run that declared secrets, so
// the caller can fail closed. A run with injection off or no secret bindings
// returns (nil, true).
func (c *runMapCaches) injectedRedactionValues(rr runRaw) ([]string, bool) {
	res := c.injectedResolver()
	if res == nil {
		return nil, true // injection disabled — nothing to add
	}
	jobSrc := rr.jobSource.String
	if jobSrc == "" {
		jobSrc = "git"
	}
	bindings, err := collectRunBindings(c.ctx, c.s.db, rr.jobName, jobSrc, rr.jobUID.String, rr.scriptRef.String, rr.overrideJSON.String)
	if err != nil {
		return nil, false // kinds unknown → fail closed
	}
	var injectable []runref.Binding
	for _, b := range bindings {
		if b.Kind == runref.KindKey {
			continue // keys aren't delivered as env values (D8 pending)
		}
		injectable = append(injectable, b)
	}
	if len(injectable) == 0 {
		return nil, true
	}
	runAgencies, aerr := runref.RunAgencies(c.ctx, c.s.db, rr.id)
	if aerr != nil {
		return nil, false // snapshot unreadable → fail closed, like the binding read above
	}
	resolved, err := res.Resolve(c.ctx, nil, rr.scope.String, runAgencies, injectable)
	if err != nil {
		return nil, false // e.g. Vault outage → fail closed
	}
	return resolved.Redact, true
}

// injectedResolver lazily builds the Vault-wired resolver used for H2 output
// masking, at most once per request. Returns nil when injection is disabled.
// (Shares the request-scoped secrets service; the process-wide client
// consolidation is H4's concern, not this path's.)
func (c *runMapCaches) injectedResolver() *runref.Resolver {
	if c.injectResolverBuilt {
		return c.injectResolver
	}
	c.injectResolverBuilt = true
	if !c.s.cfg.SecretsInjectionEnabled {
		return nil
	}
	sec := secrets.New(c.s.db, c.s.cfg, c.s.log)
	settings.WireVaultClient(c.ctx, c.s.db, c.s.cfg, sec, c.s.log)
	c.injectResolver = runref.NewResolver(c.s.db, c.s.cfg, sec, c.s.log)
	return c.injectResolver
}

// collectRunBindings enumerates a run's declared reference bindings from its job
// and (when set) its script, deduped by kind+name — mirroring the runner's
// collectReferenceBindings so the API-side injected-value resolution matches
// dispatch.
// R2F-1: jobUID is the run's frozen job identity (runs.job_uid) — the masking
// dictionary must be built from the bindings of the job that actually ran, not
// from the union of every same-named job's.
func collectRunBindings(ctx context.Context, db *sql.DB, jobName, jobSource, jobUID, scriptRef, overrideJSON string) ([]runref.Binding, error) {
	owners := []runref.Owner{{Kind: "job", Source: jobSource, Name: jobName, UID: jobUID}}
	if scriptRef != "" {
		owners = append(owners, runref.Owner{Kind: "script", Name: scriptRef})
	}
	sets := make([][]runref.Binding, 0, len(owners)+1)
	for _, o := range owners {
		bs, err := runref.ListBindings(ctx, db, o)
		if err != nil {
			return nil, err
		}
		sets = append(sets, bs)
	}
	// V2-11 — the operator's per-run additions ride the run's override envelope;
	// they must enter the H2 masking dictionary like a declared binding, or an
	// output echoing an added secret would render in the clear.
	sets = append(sets, runref.OverrideBindings(overrideJSON))
	seen := map[string]bool{}
	var out []runref.Binding
	for _, bs := range sets {
		for _, b := range bs {
			key := runref.DedupeKey(b) // RA-4: kind+name+alias, as at both dispatch seams
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, b)
		}
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Activity
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kindFilter := q.Get("kind")
	actorFilter := q.Get("actor")
	runnerFilter := q.Get("runner")
	scopeFilter := q.Get("scope")
	search := q.Get("q")
	from := q.Get("from")
	to := q.Get("to")
	page, pageSize := pageParams(q)

	base := `FROM activity WHERE 1=1`
	var args []any
	if kindFilter != "" {
		base += " AND kind = ?"
		args = append(args, kindFilter)
	}
	if actorFilter != "" {
		base += " AND actor = ?"
		args = append(args, actorFilter)
	}
	// AA-2 — independent of `actor`, not an alternative spelling of it. The
	// picker sends one or the other (it is a single choice), but both set is a
	// legal, if narrow, query rather than an error.
	if runnerFilter != "" {
		base += " AND runner_name = ?"
		args = append(args, runnerFilter)
	}
	if scopeFilter != "" {
		base += " AND scope = ?"
		args = append(args, scopeFilter)
	}
	if search != "" {
		base += " AND (job_name LIKE ? OR workflow_name LIKE ? OR summary LIKE ?)"
		args = append(args, "%"+search+"%", "%"+search+"%", "%"+search+"%")
	}
	if from != "" {
		base += " AND at >= ?"
		args = append(args, from)
	}
	if to != "" {
		base += " AND at < ?"
		args = append(args, to)
	}

	// One filter slice for the COUNT + page query (CC.14 — no parallel cArgs).
	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) "+base, args...).Scan(&total)

	// R2F-3 — job_uid/workflow_uid (R2-1) let the feed tell two same-named
	// definitions apart, and the agency set (derived from the row's own scope,
	// one correlated subquery rather than a per-row round trip) is what the badge
	// says. A row whose uid is NULL is pre-backfill history that cannot be
	// attributed to either twin; it simply never badges.
	query := `SELECT id, kind, outcome, actor, job_name, job_id, workflow_name, workflow_id,
		target, scope, schedule_file, category, action, summary, details,
		trace_id, commit_sha, repository, branch, duration_ms, killed_by, at,
		job_uid, workflow_uid, runner_name,
		(SELECT GROUP_CONCAT(a.name, '\x1f')
		   FROM scope_agencies sa
		   JOIN agencies a ON a.id = sa.agency_id
		   JOIN scopes   sc ON sc.id = sa.scope_id
		  WHERE sc.name = activity.scope) AS agencies ` +
		base + " ORDER BY at DESC LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var (
			id                          int64
			kind, actor                 string
			outcome, jobName            sql.NullString
			jobID, workflowID           sql.NullInt64
			workflowName, target, scope sql.NullString
			scheduleFile, category      sql.NullString
			action, summary, details    sql.NullString
			traceID, commitSha          sql.NullString
			repository, branch          sql.NullString
			durationMs                  sql.NullInt64
			killedBy, at                sql.NullString
			jobUID, workflowUID         sql.NullString
			runnerName                  sql.NullString
			agencies                    sql.NullString
		)
		// Scan order MUST match the SELECT column order above: outcome precedes
		// actor. (They were previously swapped, which mislabelled every row and
		// dropped rows with a NULL outcome — actor is a non-nullable string.)
		if err := rows.Scan(&id, &kind, &outcome, &actor,
			&jobName, &jobID, &workflowName, &workflowID,
			&target, &scope, &scheduleFile, &category, &action, &summary, &details,
			&traceID, &commitSha, &repository, &branch, &durationMs, &killedBy, &at,
			&jobUID, &workflowUID, &runnerName, &agencies); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"id":           id,
			"kind":         kind,
			"outcome":      nullStrVal(outcome),
			"actor":        actor,
			"at":           nullStrVal(at),
			"jobId":        nullInt64Val(jobID),
			"jobUid":       nullStrVal(jobUID),
			"jobName":      nullStrVal(jobName),
			"agencies":     splitAgencies(agencies),
			"type":         nil,
			"scope":        nullStrVal(scope),
			"traceId":      nullStrVal(traceID),
			"durationMs":   nullInt64ToPtr(durationMs),
			"killedBy":     nullStrVal(killedBy),
			"runnerName":   nullStrVal(runnerName),
			"workflowId":   nullInt64Val(workflowID),
			"workflowUid":  nullStrVal(workflowUID),
			"workflowName": nullStrVal(workflowName),
			"category":     nullStrVal(category),
			"action":       nullStrVal(action),
			"target":       nullStrVal(target),
			"details":      nullStrVal(details),
			"inventory":    nil,
			"summary":      nullStrVal(summary),
			"scheduleFile": nullStrVal(scheduleFile),
			"repository":   nullStrVal(repository),
			"branch":       nullStrVal(branch),
			"commitSha":    nullStrVal(commitSha),
		})
	}
	if items == nil {
		items = []map[string]any{}
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

// listActivityActors serves the option list for the feed's actor filter (AA-2).
//
// Classification lives HERE, not in the client: "does this actor look like a
// person" is one rule and it should have one implementation. Users are actors
// containing '@'; runners come from the runner_name column rather than from
// parsing `actor` (which holds `runner:<id>`, an id that stops resolving once
// the runner is deregistered — the whole reason AA-1 added the column);
// everything else is the fixed system vocabulary.
//
// Window-scoped (AA-Q5): same from/to as the feed, so the picker offers the
// population the feed is showing. A global distinct over the 90-day retention
// would list every runner that ever polled, most of them irrelevant to what is
// on screen. Note this reveals no actor the feed itself would not — the feed is
// not scope-filtered today (an SU-series item, deliberately unchanged here), so
// this endpoint cannot widen an exposure that the rows already carry.
func (s *Server) listActivityActors(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	where := "WHERE 1=1"
	var args []any
	if from := q.Get("from"); from != "" {
		where += " AND at >= ?"
		args = append(args, from)
	}
	if to := q.Get("to"); to != "" {
		where += " AND at < ?"
		args = append(args, to)
	}

	users, runners, system := []string{}, []string{}, []string{}

	rows, err := s.db.QueryContext(r.Context(),
		`SELECT DISTINCT actor FROM activity `+where+` AND actor IS NOT NULL AND actor <> ''
		 ORDER BY actor COLLATE NOCASE`, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(a, "runner:"):
			// An agent-written row. The runner is already offered BY NAME in
			// the runners group, so surfacing this actor too would put a raw
			// `runner:<uuid>` in the picker — the exact unreadable thing the
			// runner_name column was added to replace. Skip it.
		case strings.Contains(a, "@"):
			users = append(users, a)
		default:
			system = append(system, a)
		}
	}
	rows.Close()

	rrows, err := s.db.QueryContext(r.Context(),
		`SELECT DISTINCT runner_name FROM activity `+where+` AND runner_name IS NOT NULL AND runner_name <> ''
		 ORDER BY runner_name COLLATE NOCASE`, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rrows.Close()
	for rrows.Next() {
		var n string
		if err := rrows.Scan(&n); err != nil {
			continue
		}
		runners = append(runners, n)
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"users": users, "runners": runners, "system": system,
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Change Log
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) listChangeLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	category := q.Get("category")
	userFilter := q.Get("user")
	search := q.Get("q")
	from := q.Get("from")
	to := q.Get("to")
	page, pageSize := pageParams(q)

	base := `FROM change_log WHERE 1=1`
	var args []any
	if category != "" {
		base += " AND category = ?"
		args = append(args, category)
	}
	if userFilter != "" {
		base += " AND actor = ?"
		args = append(args, userFilter)
	}
	if search != "" {
		// Include actor so the change-log search box honors its "by … user …"
		// placeholder (PP-H7 review).
		base += " AND (actor LIKE ? OR action LIKE ? OR target LIKE ? OR details LIKE ?)"
		like := "%" + search + "%"
		args = append(args, like, like, like, like)
	}
	if from != "" {
		base += " AND at >= ?"
		args = append(args, from)
	}
	if to != "" {
		base += " AND at < ?"
		args = append(args, to)
	}

	// TS-20: allowlisted ?sort=&order= (see listRuns). Keys are the wire/table
	// column names the UI shows (timestamp/user), mapped to the at/actor columns.
	orderBy, sortErr := sortparam.OrderBy(q, map[string]string{
		"timestamp": "at",
		"user":      "actor",
		"category":  "category",
		"action":    "action",
		"target":    "target",
	}, " ORDER BY at DESC", "at DESC, id DESC")
	if sortErr != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_sort", sortErr.Error())
		return
	}

	// One filter slice for the COUNT + page query (CC.14 — no parallel cArgs).
	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) "+base, args...).Scan(&total)

	query := `SELECT id, at, actor, category, action, target, details ` +
		base + orderBy + " LIMIT ? OFFSET ?"
	args = append(args, pageSize, (page-1)*pageSize)

	rows, err := s.db.QueryContext(r.Context(), query, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	var items []map[string]any
	for rows.Next() {
		var id int64
		var at, actor, category, action string
		var target, details sql.NullString
		if err := rows.Scan(&id, &at, &actor, &category, &action, &target, &details); err != nil {
			continue
		}
		items = append(items, map[string]any{
			"id":        id,
			"timestamp": at,
			"user":      actor,
			"category":  category,
			"action":    action,
			"target":    nullStrVal(target),
			"details":   nullStrVal(details),
		})
	}
	if items == nil {
		items = []map[string]any{}
	}
	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

func (s *Server) deriveJobStatus(r *http.Request, source, jobName string, enabled int, schedule string) string {
	if enabled == 0 {
		return "paused"
	}
	// Check paused_jobs table (source-aware columns since migration 170).
	var n int
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT COUNT(*) FROM paused_jobs WHERE source = ? AND owner_kind = 'job' AND name = ?`, source, jobName).Scan(&n)
	if n > 0 {
		return "paused"
	}
	// Check for active runs.
	var status sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `
		SELECT status FROM runs
		 WHERE job_source = ? AND job_name = ? AND status <> 'skipped'
		 ORDER BY created_at DESC LIMIT 1
	`, source, jobName).Scan(&status)
	if status.Valid {
		switch status.String {
		case "running":
			return "running"
		case "queued":
			return "queued"
		case "success":
			return "success"
		case "failure", "killed", "danger":
			return "danger"
		case "warning":
			return "danger"
		}
	}
	if schedule == "" || schedule == "Manual" {
		return "idle"
	}
	return "idle"
}

// resolveJobRunnerTag reduces the job-level pin to the effective pin. Returns ""
// for "this job is not pinned", which is what every caller and the claim
// predicate treat as unpinned.
//
// One layer, as of v1.3.5 (migration 1090). RT-Q7 had two — a declared pin and
// an operator override that could mask it, including masking it to nothing —
// and this function existed to collapse them without a COALESCE, since NULL
// ("no opinion") and ” ("force-unpinned") had to stay distinguishable. The
// override layer is gone; a per-RUN pin still carries that tri-state, and it is
// resolved one rung up in resolveRunnerTag.
//
// Kept as a function rather than inlined because it names the step: the caller
// asks for the effective answer and does not decide what "effective" means.
func resolveJobRunnerTag(declared sql.NullString) string {
	if declared.Valid {
		return declared.String
	}
	return ""
}

// resolveRunnerTag picks the runner pin frozen on a run (RT-2), applying the
// full precedence (highest wins):
//
//	per-trigger override > jobs.runner_tag > unset
//
// triggerOverride carries RT-Q6's tri-state as a *string: nil = inherit the
// job's declared pin, "" = run THIS run unpinned regardless, "x" = pin this run
// to x. The middle state is why this is a *string and not a string: an empty
// per-run pin is a decision to escape a pinned job for one run, and it is now
// the ONLY way to do that — the operator override that could do it durably was
// retired in v1.3.5 (migration 1090).
//
// Deliberately does NOT consult the fleet (RT-Q4): a pin naming a tag nothing
// carries is legal and enqueues normally, because the runner may register a
// minute later and because a scheduled job must not start failing nightly over a
// transient fleet condition. execspec.UnclaimableReason explains the wait.
//
// Returns (tag, "") on success or ("", reason) to reject with 422.
func resolveRunnerTag(ctx context.Context, db *sql.DB, jobSource, jobName string, triggerOverride *string) (string, string) {
	if jobSource == "" {
		jobSource = "git"
	}
	if triggerOverride != nil {
		if *triggerOverride == "" {
			return "", "" // explicit per-run unpin; never consult the job
		}
		tag := gitlab.NormalizeRunnerTag(*triggerOverride)
		if tag == "" {
			return "", fmt.Sprintf("invalid runner tag %q", *triggerOverride)
		}
		return tag, ""
	}
	var declared sql.NullString
	_ = db.QueryRowContext(ctx,
		`SELECT runner_tag FROM jobs WHERE name = ? AND source = ?`,
		jobName, jobSource).Scan(&declared)
	return resolveJobRunnerTag(declared), ""
}

// resolveExecutor picks the executor frozen on a run (R5.1), applying the
// resolution precedence (highest wins):
//
//	per-trigger override > job spec.executor > global execution.defaultExecutor
//	> run-type capability default (shell types ⇒ ssh, ansible/terraform ⇒ runner)
//
// It then enforces the capability matrix (R5.2): executor='ssh' is invalid for
// ansible/terraform (SSH can't run them); executor='runner' is valid for any
// run type. ansible/terraform routed to 'runner' enqueue normally and sit
// queued until a capable runner registers (A6.3) — no longer a 422 (R4.3).
//
// Returns (executor, "") on success or ("", reason) to reject with 422.
func resolveExecutor(ctx context.Context, db *sql.DB, jobSource, jobName, runType, triggerOverride string) (string, string) {
	if jobSource == "" {
		jobSource = "git"
	}
	// Validate the per-trigger override enum up front.
	if triggerOverride != "" && triggerOverride != "ssh" && triggerOverride != "runner" {
		return "", fmt.Sprintf("invalid executor %q (want ssh|runner)", triggerOverride)
	}

	executor := triggerOverride

	// Job spec.executor (source-qualified — A9).
	if executor == "" {
		var jobExec sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT executor FROM jobs WHERE name = ? AND source = ?`, jobName, jobSource).Scan(&jobExec)
		if jobExec.Valid && (jobExec.String == "ssh" || jobExec.String == "runner") {
			executor = jobExec.String
		}
	}

	// Global execution.defaultExecutor.
	if executor == "" {
		var def sql.NullString
		_ = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'defaultExecutor'`).Scan(&def)
		if def.Valid && (def.String == "ssh" || def.String == "runner") {
			executor = def.String
		}
	}

	// Run-type capability default.
	if executor == "" {
		if sshexec.SupportedRunType(runType) {
			executor = "ssh"
		} else {
			executor = "runner"
		}
	}

	// Capability matrix (R5.2): SSH can't run ansible/terraform.
	if executor == "ssh" && !sshexec.SupportedRunType(runType) {
		return "", fmt.Sprintf("run-type %q cannot run over the SSH executor (it needs a local toolchain); choose the runner executor", runType)
	}

	return executor, ""
}

func (s *Server) maxConcurrent(r *http.Request) int {
	var val sql.NullString
	_ = s.db.QueryRowContext(r.Context(), `SELECT value FROM settings WHERE key = 'maxConcurrent'`).Scan(&val)
	if val.Valid && val.String != "" {
		if n, err := strconv.Atoi(val.String); err == nil && n > 0 {
			return n
		}
	}
	return 5
}

// dbStatusFilter reverse-maps a display RunStatus (the value mapRunStatus emits,
// which the frontend filters/sends) back to the raw runs/workflow_runs status
// value(s) for a WHERE clause. Without this, a filter like status=danger would
// compare against the raw column ('failure'/'killed') and match nothing (PP-H7
// review). Returns the SQL fragment and its bind args.
func dbStatusFilter(col, display string) (string, []any) {
	if display == "danger" {
		return col + " IN (?, ?)", []any{"failure", "killed"}
	}
	return col + " = ?", []any{display}
}

// workflowStatusFilter is dbStatusFilter's workflow_runs sibling, and it exists
// because that table's display status is not a column (F-1). Cancellation is
// carried by an additive flag — migration 260 keeps the raw status at a
// CHECK-legal terminal ('failure') — and workflowDisplayStatus folds the flag in
// when serialising. A filter that ignored the flag disagreed with the rows it
// returned in both directions: status=cancelled matched nothing at all, and
// status=danger returned soft-cancelled runs whose own Status column read
// Cancelled. Folding the flag in here is what makes "pick a label, get rows
// carrying that label" true.
//
// 'running' additionally groups queued, because the UI's canonical statusLabel
// folds the pair into one word: a queued run's badge says Running, so the
// Running filter has to return it.
func workflowStatusFilter(display string) (string, []any) {
	switch display {
	case "cancelled":
		return "cancelled = 1", nil
	case "danger":
		return "cancelled = 0 AND status IN (?, ?)", []any{"failure", "killed"}
	case "running":
		return "cancelled = 0 AND status IN (?, ?)", []any{"running", "queued"}
	default:
		return "cancelled = 0 AND status = ?", []any{display}
	}
}

func mapRunStatus(s string) string {
	switch s {
	case "success":
		return "success"
	case "warning":
		return "warning"
	case "failure", "killed":
		return "danger"
	case "skipped":
		return "skipped"
	case "queued":
		return "queued"
	case "running":
		return "running"
	default:
		return s
	}
}

func intParam(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return def
	}
	return n
}

// maxPageSize bounds the page size of every paginated list endpoint. It mirrors
// openapi.yaml components.parameters.pageSize.maximum (200); without this the
// server silently violates its own contract — a value like pageSize=100000000
// flows straight into LIMIT and materializes an entire growing table into one
// response (PP-H5: unbounded-LIMIT memory/CPU DoS).
const maxPageSize = 200

// pageParams parses the standard page/pageSize query params, clamping pageSize
// to maxPageSize. intParam stays generic (it also serves page, which has a
// different lower bound and no maximum), so the ceiling lives only here. page is
// deliberately NOT capped: a large OFFSET returns an empty body cheaply and is
// not a memory vector, and capping it would break deep history navigation.
func pageParams(q url.Values) (page, pageSize int) {
	page = intParam(q.Get("page"), 1)
	pageSize = min(intParam(q.Get("pageSize"), 50), maxPageSize)
	return
}

// pageEnvelope builds the standard paginated list response body (page, pageSize,
// totalItems, totalPages, items) shared by every list endpoint (CC.14).
// totalPages is at least 1 so an empty result still reports a single page.
func pageEnvelope(page, pageSize, total int, items any) map[string]any {
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	return map[string]any{
		"page":       page,
		"pageSize":   pageSize,
		"totalItems": total,
		"totalPages": totalPages,
		"items":      items,
	}
}

// mustActor returns the authenticated actor's email, or writes a 401 and returns
// ok=false. It collapses the IdentityFrom-then-401 boilerplate repeated across
// session-gated handlers (CC.15; adopted incrementally as handlers are touched).
func (s *Server) mustActor(w http.ResponseWriter, r *http.Request) (string, bool) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return "", false
	}
	return id.Email, true
}

func nullStrVal(n sql.NullString) any {
	if !n.Valid {
		return nil
	}
	return n.String
}

func nullInt64Val(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func nullInt64ToPtr(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

// parseAgenciesJSON decodes a run's frozen agency-set snapshot (migration 680) for
// display and for the stuck-run signal. Always a non-nil slice so the API renders
// [] rather than null, and a malformed or absent snapshot degrades to the general
// pool rather than failing the whole run list.
func parseAgenciesJSON(raw sql.NullString) []string {
	out := []string{}
	if !raw.Valid || raw.String == "" || raw.String == "[]" {
		return out
	}
	if err := json.Unmarshal([]byte(raw.String), &out); err != nil {
		return []string{}
	}
	return out
}

// joinAgencyNames renders an agency set for a stuck-run reason. Singular and plural
// read differently enough that an operator should not have to parse a bare list.
func joinAgencyNames(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "'" + n + "'"
	}
	if len(quoted) == 1 {
		return "agency " + quoted[0]
	}
	return "agencies " + strings.Join(quoted, ", ")
}

// splitAgencies decodes the \x1f-joined GROUP_CONCAT the derived-agency
// subqueries produce (R2F-3). Always a non-nil slice: an `agencies: null` on the
// wire and an empty list mean the same thing to every consumer, and only one of
// them survives a JSON round trip intact.
func splitAgencies(raw sql.NullString) []string {
	if !raw.Valid || raw.String == "" {
		return []string{}
	}
	return strings.Split(raw.String, "\x1f")
}
