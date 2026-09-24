package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountWorkflowCompose owns the in-app Workflow composition write surface (A11 /
// v20 Phase 4). An amadeus-source Workflow is a DB-authored step graph over jobs;
// git-source workflows are read-only here (409). Coexists with the existing
// PATCH /workflows/{id} (disabled toggle) and POST /workflows/{id}/trigger.
//
// Routes (behind requireCompose):
//
//	POST   /api/v1/workflows               create an amadeus-source workflow
//	POST   /api/v1/workflows/validate      dry-run validate a step graph (WB-A1; no write)
//	PUT    /api/v1/workflows/{workflowId}   edit an amadeus-source workflow (409 on git)
//	DELETE /api/v1/workflows/{workflowId}   delete an amadeus-source workflow (409 on git)
func (s *Server) mountWorkflowCompose(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/workflows", s.requireCompose(http.HandlerFunc(s.createWorkflow)))
	mux.Handle("POST /api/v1/workflows/validate", s.requireCompose(http.HandlerFunc(s.validateWorkflow)))
	mux.Handle("PUT /api/v1/workflows/{workflowId}", s.requireCompose(http.HandlerFunc(s.updateWorkflow)))
	mux.Handle("DELETE /api/v1/workflows/{workflowId}", s.requireCompose(http.HandlerFunc(s.deleteWorkflow)))
}

type workflowComposeInput struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Enabled      *bool                  `json:"enabled"`
	Steps        json.RawMessage        `json:"steps"`
	ScheduleRefs []string               `json:"scheduleRefs"`
	Schedules    []composeScheduleEntry `json:"schedules"`
	// Layout is the advisory canvas node-position map (WC-P7): {"<nodeId>":{"x":n,
	// "y":n}, …}. Stored verbatim in workflows.layout_json; NEVER affects steps or
	// steps_hash. The graph editor sends it; the linear editor omits it (and an
	// omitted layout preserves any existing one — see writeComposedWorkflow).
	Layout json.RawMessage `json:"layout"`
}

// validateWorkflow is the dry-run endpoint (WB-A1): it runs the same structural,
// duplicate, and job-existence checks as the write path but persists nothing and
// always returns 200 {ok, errors[]} so the editor can surface inline feedback
// before committing (WB-E6). A malformed body is the one hard 422. Behind
// requireCompose like the rest of the compose surface.
func (s *Server) validateWorkflow(w http.ResponseWriter, r *http.Request) {
	var in workflowComposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	errs := []workflow.ValidationError{}
	if name := strings.TrimSpace(in.Name); name != "" && !composeNameRe.MatchString(name) {
		errs = append(errs, workflow.ValidationError{Field: "name", Message: "invalid workflow name (want " + composeNameRe.String() + ")"})
	}
	steps, err := workflow.ParseSteps(string(in.Steps))
	if err != nil {
		errs = append(errs, workflow.ValidationError{Field: "steps", Message: "invalid steps: " + err.Error()})
	} else {
		errs = append(errs, s.validateComposedWorkflowSteps(r.Context(), strings.TrimSpace(in.Name), steps)...)
		// AF-2 — the dry run reports on the same graph the write would accept, so
		// it answers for jobs the caller may author and no others. Reported as a
		// validation error rather than a 403: this endpoint always returns 200
		// {ok,errors} by contract (WB-E6), and the editor renders the list inline.
		// It also keeps the dry run from becoming an existence oracle for another
		// department's jobs — the answer is the same shape either way.
		actorID, _ := auth.IdentityFrom(r.Context())
		for _, name := range unauthoredStepJobs(r, s, actorID, steps) {
			errs = append(errs, workflow.ValidationError{
				Step:    name,
				Message: "you may not compose jobs in this job's scope: " + name,
			})
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"ok": len(errs) == 0, "errors": errs})
}

func (s *Server) createWorkflow(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	var in workflowComposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !composeNameRe.MatchString(in.Name) {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid workflow name (want "+composeNameRe.String()+")")
		return
	}
	// RH: a binned workflow still holds its name — see createJob.
	// R2-5 — per-agency uniqueness, derived from the graph's jobs (a workflow
	// has no scope of its own). Same generic-refusal rule as jobs.
	steps, perr := workflow.ParseSteps(string(in.Steps))
	if perr == nil {
		scopes := s.collectStepScopes(r, steps)
		conflict, cerr := execspec.WorkflowNamePoolConflict(r.Context(), s.db, "amadeus", in.Name, scopes, "")
		if cerr != nil {
			httpx.Fail500(w, s.log, "db_error", cerr)
			return
		}
		if conflict {
			httpx.Fail(w, http.StatusConflict, "conflict", execspec.NamePoolRefusal)
			return
		}
	}
	s.writeComposedWorkflow(w, r, in, id.Email, true, db.NewID())
}

func (s *Server) updateWorkflow(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	wfID := r.PathValue("workflowId")
	existing := s.fetchWorkflowByID(r, wfID)
	if existing == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	if existing.Source != "amadeus" {
		httpx.Fail(w, http.StatusConflict, "conflict", "only amadeus-source workflows are editable in-app")
		return
	}
	if existing.DeletedAt != nil {
		httpx.Fail(w, http.StatusConflict, "conflict", "this workflow is in the recycle bin — restore it before editing")
		return
	}
	// AF-2 — the graph being REPLACED. writeComposedWorkflow checks the incoming
	// one; both must pass, or a departmental composer could take over a workflow
	// driving out-of-reach jobs simply by sending a graph of their own jobs.
	if !s.requireComposeStepScopes(w, r, id, existing.Steps) {
		return
	}
	var in workflowComposeInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return
	}
	in.Name = existing.Name // identity is immutable on edit
	s.writeComposedWorkflow(w, r, in, id.Email, false, existing.UID)
}

func (s *Server) deleteWorkflow(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	wfID := r.PathValue("workflowId")
	existing := s.fetchWorkflowByID(r, wfID)
	if existing == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "workflow not found")
		return
	}
	if existing.Source != "amadeus" {
		httpx.Fail(w, http.StatusConflict, "conflict", "only amadeus-source workflows are deletable in-app")
		return
	}
	// AF-2 — deleting is authoring: same per-job authority as editing the graph.
	if !s.requireComposeStepScopes(w, r, id, existing.Steps) {
		return
	}
	// RX-24 — refuse while reactions WATCH this workflow, unless forced. Its own
	// code, not "conflict": the git-source refusal above is also a 409, and only
	// this one is clearable with ?force=true.
	if msg, err := s.reactionDeleteGuard(r, "workflow", existing.Source, existing.Name); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	} else if msg != "" {
		httpx.Fail(w, http.StatusConflict, ErrCodeReactionsWatching, msg)
		return
	}
	// RH — SOFT delete; see deleteJob for the full rationale. The row is stamped
	// rather than removed so an undelete is one lossless UPDATE, and the entity
	// code retires at purge time rather than here.
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(r.Context(),
		`UPDATE workflows SET deleted_at=?, deleted_by=?
		  WHERE (uid = ? OR (? = '' AND source='amadeus' AND name = ?)) AND deleted_at IS NULL`,
		now, id.Email, existing.UID, existing.UID, existing.Name)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "this workflow is already in the recycle bin")
		return
	}
	if err := snapshotRevision(r.Context(), tx, revKindWorkflow, "amadeus", existing.Name, existing.UID, id.Email, revActionDeleted,
		map[string]any{"deletedAt": now, "deletedBy": id.Email}); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	s.auditDefinitionDelete(r.Context(), id.Email, "workflow", "Workflows", existing.Name, nil)
	s.forceScheduleReload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// unauthoredStepJobs lists the jobs in a graph whose scope this actor may NOT
// author in — the reporting counterpart of requireComposeStepScopes, for the
// dry-run endpoint that must answer 200 with a list rather than 403 (WB-E6).
// Writes no audit rows: nothing was attempted, and a keystroke-driven validator
// would flood the log.
func unauthoredStepJobs(r *http.Request, s *Server, id auth.Identity, steps []workflow.Step) []string {
	var out []string
	// R2F-2: one entry per distinct REFERENCE, not per name — two steps pinning
	// different twins of one name are two jobs, and reporting one would hide the
	// other's authorization failure behind a name that looked already-checked.
	for _, ref := range workflow.CollectStepRefs(steps) {
		if ref.Name == "" {
			continue
		}
		scope, found := s.stepJobScope(r, ref)
		if !found {
			continue // unknown job: the existence validator already reported it
		}
		// AllScopes for the All pool, mirroring requireComposeScope — Can(perm, "")
		// is the permissive Q-F7 reading and would report every departmental
		// composer as authorized for an unscoped job.
		ok := false
		if strings.TrimSpace(scope.String) == "" {
			ok = id.Can(auth.PermCompose, auth.AllScopes)
		} else {
			ok = id.Can(auth.PermCompose, scope.String)
		}
		if !ok {
			out = append(out, ref.Name)
		}
	}
	return out
}

// requireComposeStepScopes checks the caller may author every job a step graph
// names (AF-2). A workflow carries no scope, so this IS its scope check: the
// agencies a workflow belongs to are derived from its jobs (RB-23/RF-17), and
// authoring a graph over a job is authority over that job — a scheduled workflow
// fires it. An All-scoped job therefore keeps workflows admin-only too, via
// requireComposeScope's CanUnbound branch.
//
// A job the graph names but that does not exist is left to the existence
// validator, which has already run and produced a precise 422; skipping it here
// avoids answering "does a job by this name exist in another agency?" with a 403.
func (s *Server) requireComposeStepScopes(w http.ResponseWriter, r *http.Request, id auth.Identity, steps []workflow.Step) bool {
	seenScope := map[string]bool{}
	for _, ref := range workflow.CollectStepRefs(steps) {
		if ref.Name == "" {
			continue
		}
		scope, ok := s.stepJobScope(r, ref)
		if !ok {
			continue // unknown job: the existence validator owns that message
		}
		// One check per distinct scope, not per job: a ten-step graph in one scope
		// should not write ten identical denial audit rows.
		if seenScope[scope.String] {
			continue
		}
		seenScope[scope.String] = true
		if !s.requireComposeScope(w, r, id, scope.String) {
			return false
		}
	}
	return true
}

func (s *Server) writeComposedWorkflow(w http.ResponseWriter, r *http.Request, in workflowComposeInput, actor string, isCreate bool, uid string) {
	// Validate the step graph parses, and that each referenced job resolves to an
	// existing job (in either source — the A11 precedence picks the effective one
	// at run time, defaulting to the workflow's own amadeus source).
	steps, err := workflow.ParseSteps(string(in.Steps))
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid steps: "+err.Error())
		return
	}
	// WB-S5 — structural/enum shape, the same-path duplicate-name guard (PP-H8 a),
	// and job-existence, all via the shared validator the dry-run endpoint reuses.
	// Return the first error as the human message plus the full list under details.
	if verrs := s.validateComposedWorkflowSteps(r.Context(), in.Name, steps); len(verrs) > 0 {
		httpx.FailDetails(w, http.StatusUnprocessableEntity, "validation_failed", verrs[0].Message, map[string]any{"errors": verrs})
		return
	}
	// AF-2 — a workflow has no scope of its own; its authority comes from the jobs
	// it drives, because triggering it triggers them. So every job the incoming
	// graph names must be one this actor could author. updateWorkflow separately
	// checks the graph being REPLACED, for the same both-sides reason job edits do.
	actorID, _ := auth.IdentityFrom(r.Context())
	if !s.requireComposeStepScopes(w, r, actorID, steps) {
		return
	}
	entries, verr := s.resolveComposeSchedules(r.Context(), jobComposeInput{ScheduleRefs: in.ScheduleRefs, Schedules: in.Schedules})
	if verr != "" {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", verr)
		return
	}
	// Reserved-namespace guard (W4, N-D1): no CRONOMICON_* key in any (inline or
	// reusable) schedule env bound to this workflow.
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
	legacyMirror := ""
	if len(entries) > 0 {
		legacyMirror = entries[0].Cron
	}
	stepsJSON := string(in.Steps)
	if strings.TrimSpace(stepsJSON) == "" {
		stepsJSON = "[]"
	}
	// WC-P7 advisory canvas layout — stored verbatim, bound as NULL when the caller
	// omits it or sends malformed/`null` JSON. The COALESCE in the upsert then
	// preserves any existing layout (so a linear-editor save doesn't wipe positions
	// hand-arranged in the graph editor). Advisory only: never touches steps_hash.
	var layoutJSON any
	if len(in.Layout) > 0 && json.Valid(in.Layout) && string(in.Layout) != "null" {
		layoutJSON = string(in.Layout)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO workflows(name, source, description, steps, schedule, enabled,
		                      created_by, created_at, last_modified_by, last_modified_at, layout_json, uid)
		VALUES(?, 'amadeus', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uid) DO UPDATE SET
			description=excluded.description, steps=excluded.steps, schedule=excluded.schedule,
			enabled=excluded.enabled, last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at,
			layout_json=COALESCE(excluded.layout_json, workflows.layout_json)`,
		in.Name, nullStrIf(in.Description), stepsJSON, nullStrIf(legacyMirror), enabled, actor, now, actor, now, layoutJSON,
		// R2-5 — the identity IS the conflict target: create inserts fresh,
		// edit collides on the existing uid and lands in DO UPDATE.
		uid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	// LU-6: allocate inside the transaction — see writeComposedJob for why this
	// must not sit beside the post-commit changelog block.
	if _, err := entitycode.Allocate(r.Context(), tx, entitycode.KindWorkflow, "amadeus", in.Name, uid); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM definition_schedules WHERE owner_kind='workflow' AND owner_uid=?`, uid); err != nil {
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
			VALUES('amadeus','workflow',?,?,?,?,?,?,?,?,?,?,?,
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
	// RH — the snapshot commits with the write it describes (see writeComposedJob).
	revAction := revActionUpdated
	if isCreate {
		revAction = revActionCreated
	}
	if err := snapshotRevision(r.Context(), tx, revKindWorkflow, "amadeus", in.Name, uid, actor, revAction, in); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	auditAction := "Created"
	if !isCreate {
		auditAction = "Updated"
	}
	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Workflows", auditAction, in.Name, "Cronomicon workflow "+strings.ToLower(auditAction))
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Workflows", Action: auditAction, Target: in.Name,
	})
	s.forceScheduleReload(r.Context())

	var rowid int64
	_ = s.db.QueryRowContext(r.Context(), `SELECT rowid FROM workflows WHERE source='amadeus' AND name=?`, in.Name).Scan(&rowid)
	wr := s.fetchWorkflowByID(r, strconv.FormatInt(rowid, 10))
	status := http.StatusOK
	if isCreate {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, wr)
}

// validateComposedWorkflowSteps runs the full validation for an amadeus workflow's
// step graph and returns the combined error list (empty ⇒ valid): WB-S5 structural
// /enum shape (workflow.ValidateSteps), the same-path duplicate-name guard
// (PP-H8 a), then job-existence for every referenced job. Shared by the write path
// (422 on any error) and the dry-run endpoint (WB-A1, 200 {ok,errors}).
func (s *Server) validateComposedWorkflowSteps(ctx context.Context, selfName string, steps []workflow.Step) []workflow.ValidationError {
	errs := workflow.ValidateSteps(steps)
	// PP-H8 a: the engine mints per-node trace IDs (no PK collision), but the
	// name-keyed results map can't disambiguate two same-named nodes on one path,
	// so a branch condition / A12 input referencing that name is ambiguous.
	// Mutually-exclusive branch arms (Pass vs Fail) are independent paths, so the
	// same name may appear once in each. (Git-sourced YAML bypasses this guard.)
	if dup := firstSamePathDuplicate(steps, map[string]bool{}); dup != "" {
		errs = append(errs, workflow.ValidationError{Step: dup, Message: "duplicate job reference on one execution path: " + dup})
	}
	for _, ref := range workflow.CollectStepRefs(steps) {
		if ref.Name == "" {
			continue
		}
		// R2F-2 — a pinned step is checked against its IDENTITY, and the pair must
		// agree: a uid pointing at a differently-named job means the graph displays
		// one job and runs another. The name and the uid travel together or the
		// display lies, so a mismatch is a 422 rather than a silent preference for
		// either half.
		if ref.UID != "" {
			var pinned string
			err := s.db.QueryRowContext(ctx,
				`SELECT name FROM jobs WHERE uid = ? AND deleted_at IS NULL`, ref.UID).Scan(&pinned)
			switch {
			case err != nil:
				errs = append(errs, workflow.ValidationError{Step: ref.Name, Field: "jobUid",
					Message: "step references unknown job: " + ref.Name})
			case pinned != ref.Name:
				errs = append(errs, workflow.ValidationError{Step: ref.Name, Field: "jobUid",
					Message: "step name and job identity disagree: the pinned job is named " + pinned + ", not " + ref.Name})
			}
			continue
		}
		var n int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE name=? AND deleted_at IS NULL`, ref.Name).Scan(&n)
		if n == 0 {
			errs = append(errs, workflow.ValidationError{Step: ref.Name, Message: "step references unknown job: " + ref.Name})
		}
	}
	errs = append(errs, s.validateWorkflowRefs(ctx, selfName, steps)...)
	return errs
}

// validateWorkflowRefs is the SW referential half: does every referenced
// sub-workflow exist, and does any reference close a cycle?
//
// The cycle walk is a DFS over the STORED graph plus the graph being written,
// modelled on detectReactionCycle (RX) — including its two hard-won details:
//
//   - the proposed graph must REPLACE the stored one for this workflow, or a
//     write that removes a cycle is judged against the cycle it just removed;
//   - the error names the whole path, because "cycle detected" without the path
//     is unactionable in a graph of any size.
//
// A depth ceiling still guards at run time (workflow.MaxWorkflowDepth). Both
// exist deliberately: dual-source means the graph can change between authoring
// and fire, so a cycle can be closed by editing a DIFFERENT workflow that this
// check never saw.
func (s *Server) validateWorkflowRefs(ctx context.Context, selfName string, steps []workflow.Step) []workflow.ValidationError {
	var errs []workflow.ValidationError
	refs := workflow.WorkflowRefs(steps)
	if len(refs) == 0 {
		return nil
	}

	seenRef := map[string]bool{}
	for _, ref := range refs {
		if seenRef[ref] {
			continue
		}
		seenRef[ref] = true
		if selfName != "" && ref == selfName {
			errs = append(errs, workflow.ValidationError{
				Step: ref, Field: "workflow",
				Message: "a workflow cannot run itself: " + ref,
			})
			continue
		}
		var n int
		_ = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM workflows WHERE name = ? AND deleted_at IS NULL`, ref).Scan(&n)
		if n == 0 {
			errs = append(errs, workflow.ValidationError{
				Step: ref, Field: "workflow",
				Message: "step references unknown workflow: " + ref,
			})
		}
	}
	if len(errs) > 0 {
		return errs // don't walk a graph with known-bad edges
	}

	edges, err := s.workflowRefEdges(ctx)
	if err != nil {
		// Fail OPEN with a loud log, matching the calendar gate's stance: a
		// transient DB error must not block authoring, and the runtime ceiling is
		// the backstop that makes that safe.
		s.log.Error("workflow: could not load sub-workflow edges; skipping cycle check", "err", err)
		return nil
	}
	if selfName != "" {
		edges[selfName] = refs // the proposed graph replaces the stored one
	}
	if path := findWorkflowCycle(selfName, edges); path != "" {
		errs = append(errs, workflow.ValidationError{
			Field:   "workflow",
			Message: "sub-workflow reference would create a cycle: " + path,
		})
	}
	return errs
}

// workflowRefEdges reads every workflow's sub-workflow references, across both
// sources — a cycle can close through a git workflow just as easily.
func (s *Server) workflowRefEdges(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, COALESCE(steps,'[]') FROM workflows WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		parsed, perr := workflow.ParseSteps(raw)
		if perr != nil {
			continue // an unparseable stored graph is the sync's problem to report
		}
		if refs := workflow.WorkflowRefs(parsed); len(refs) > 0 {
			out[name] = append(out[name], refs...)
		}
	}
	return out, rows.Err()
}

// findWorkflowCycle returns a human-readable path when start can reach itself.
func findWorkflowCycle(start string, edges map[string][]string) string {
	if start == "" {
		return ""
	}
	var path []string
	onPath := map[string]bool{}
	var walk func(node string) string
	walk = func(node string) string {
		if onPath[node] {
			return strings.Join(append(path, node), " → ")
		}
		onPath[node] = true
		path = append(path, node)
		for _, next := range edges[node] {
			if p := walk(next); p != "" {
				return p
			}
		}
		path = path[:len(path)-1]
		onPath[node] = false
		return ""
	}
	return walk(start)
}

// firstSamePathDuplicate returns a job name that appears more than once on a
// single execution path (which the engine's name-keyed results map can't
// disambiguate), or "" if none. A branch's Pass and Fail arms are mutually
// exclusive, so each is walked with its OWN copy of the seen-set — a name may
// appear once per arm without conflict.
func firstSamePathDuplicate(steps []workflow.Step, seen map[string]bool) string {
	mark := func(name string) bool {
		if name == "" {
			return false
		}
		if seen[name] {
			return true
		}
		seen[name] = true
		return false
	}
	for _, s := range steps {
		switch s.Type {
		case "job":
			if mark(s.Name) {
				return s.Name
			}
		case "parallel":
			// PS-1: every arm runs, so all arms share ONE path (and one seen-set);
			// an arm is a leaf job or a sequence of further steps.
			for _, j := range s.Jobs {
				if j.Type == "sequence" {
					if dup := firstSamePathDuplicate(j.Steps, seen); dup != "" {
						return dup
					}
					continue
				}
				if mark(j.Name) {
					return j.Name
				}
			}
		case "sequence":
			if dup := firstSamePathDuplicate(s.Steps, seen); dup != "" {
				return dup
			}
		case "branch":
			if s.Pass != nil {
				if dup := firstSamePathDuplicate(s.Pass.Steps, copySeen(seen)); dup != "" {
					return dup
				}
			}
			if s.Fail != nil {
				if dup := firstSamePathDuplicate(s.Fail.Steps, copySeen(seen)); dup != "" {
					return dup
				}
			}
		}
	}
	return ""
}

func copySeen(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	maps.Copy(out, m)
	return out
}

// stepJobScope resolves ONE step's job the way the engine will, and returns its
// scope (R2-3).
//
// The `ok` return distinguishes "no such job" from "a job with an empty scope"
// — the two are opposite answers for authorization (the second is the All pool,
// which is admin-only) and a bare sql.NullString conflates them.
//
// The composed workflow's source is 'amadeus': these endpoints only write
// amadeus-source workflows, so that is the wfSource the engine will resolve
// with. Sharing workflow.StepSourceOrder is the point — the previous
// implementation asked `ORDER BY source LIMIT 1`, which authorized against
// whichever source sorted first rather than the one that will actually run.
func (s *Server) stepJobScope(r *http.Request, ref workflow.StepRef) (sql.NullString, bool) {
	// R2F-2 — a pinned step resolves by identity alone, exactly as the engine's
	// resolveJobDef does. Falling back to the name here would authorize against a
	// sibling department's job and then run the pinned one.
	if ref.UID != "" {
		var scope sql.NullString
		err := s.db.QueryRowContext(r.Context(),
			`SELECT scope FROM jobs WHERE uid = ? AND deleted_at IS NULL`, ref.UID).Scan(&scope)
		return scope, err == nil
	}
	for _, src := range workflow.StepSourceOrder(ref.Source, "amadeus") {
		var scope sql.NullString
		err := s.db.QueryRowContext(r.Context(),
			`SELECT scope FROM jobs WHERE name = ? AND source = ? AND deleted_at IS NULL`,
			ref.Name, src).Scan(&scope)
		if err == nil {
			return scope, true
		}
	}
	return sql.NullString{}, false
}

// collectStepScopes resolves each step job's scope through the same A11
// precedence execution uses (R2-5) — the input to the workflow name-pool check.
func (s *Server) collectStepScopes(r *http.Request, steps []workflow.Step) []string {
	var out []string
	for _, ref := range workflow.CollectStepRefs(steps) {
		if ref.Name == "" {
			continue
		}
		if scope, ok := s.stepJobScope(r, ref); ok {
			out = append(out, scope.String)
		}
	}
	return out
}
