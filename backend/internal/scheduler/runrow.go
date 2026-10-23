package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// RunRow is the ONE shape a `runs` row is written from (RR-2,
// the run-row-unification plan). Every producer — cron fire, manual
// and token triggers, promoted pending runs, file-watch sightings, suppressed
// fires, workflow step children and skipped steps — builds one of these and
// calls InsertRun. There is exactly one INSERT INTO runs in the codebase
// outside seed data, and run_writer_conformance_test.go fails the build if a
// second appears.
//
// Why: five hand-rolled column lists drifted four times in eleven days (the
// become-file token off the cron path; workflow children with no requires_json
// and no checkout snapshot; cron runs with no unclaimable reason). claimRun and the manifest read dispatch policy from THIS row, so a
// column a writer forgot was not a default — it was a policy the run did not
// have. One writer, one column list, one place to add the next field.
//
// The embedded EnqueueParams carries everything a queued run needs. The fields
// below it are what the other producers needed that EnqueueParams lacked.
type RunRow struct {
	EnqueueParams

	// TraceID is the row id. Empty mints a UUIDv7 (the normal case); the
	// workflow engine passes its node's pre-minted id for skipped steps so
	// the canvas can address the row it drew (PP-H8 c).
	TraceID string

	// Status is 'queued' when empty. Producers set 'skipped' for terminal
	// suppression rows and, on the workflow path, 'failure' for a step refused
	// at insert (RA-24 unbound references).
	Status string

	// QueuedReason is the operator-facing "why": the suppression text on a
	// skipped row, the refusal on a failed one. NULL when empty. Queued rows
	// leave it empty here and get the RA-20(b) advisory stamp from
	// execspec.StampUnclaimableReason AFTER the insert, in the caller.
	QueuedReason string

	// Terminal marks a row that will never execute — a suppressed fire or a
	// skipped step. Two consequences, both deliberate:
	//   - started_at and completed_at are set to created_at, so History shows
	//     a zero-length run rather than one that never started;
	//   - the dispatch-policy snapshot (checkout_sha/entry, requires_json) is
	//     NOT taken. Those columns are what claimRun and the
	//     manifest read to dispatch; a row that cannot be dispatched carries
	//     none, exactly as the skipped writers always wrote.
	// script_ref, script_uid, content_hash and entity_code ARE still
	// snapshotted: they say what the row was about, which History needs either
	// way.
	Terminal bool

	// SuppressedByCalendar names the working calendar that vetoed a fire
	// (CAL-6); NULL otherwise. Only meaningful with Terminal.
	SuppressedByCalendar string

	// NoDefinition writes the row as of no definition even though JobName
	// names one: a workflow step refused for being another agency's, whose
	// row must not be that agency's job's (2.4.2, present defect 58). Without
	// it an empty JobUID is filled from the name.
	NoDefinition bool

	// CreatedAt overrides the row timestamp (RFC3339 UTC). Empty means now.
	// Only the missed-run detector sets it: a miss is DISCOVERED minutes after
	// the fire it describes, and stamping discovery time would put the row
	// somewhere History does not match the gap it explains.
	CreatedAt string

	// Kind is the row's discriminator: 'job' when empty (every run of a
	// definition), 'ssh-test' for a "Test connection" probe mirrored into
	// History (V1.1-2). A non-job kind has no definition behind it, so
	// job_source stays NULL rather than defaulting to 'git', and the caller
	// supplies EntityCode because there is no jobs row to resolve one from.
	Kind string

	// EntityCode overrides the log-folder code that is otherwise resolved from
	// entity_codes by the job's uid. Set only by producers with no job —
	// entitycode.SystemCode for diagnostics.
	EntityCode string

	// DurationMs is the measured duration for a row that completes at insert
	// (an ssh-test probe's dial+auth latency). NULL when nil; a skipped row
	// never ran and carries none.
	DurationMs *int64
}

func (r RunRow) kindOrDefault() string {
	if r.Kind == "" {
		return "job"
	}
	return r.Kind
}

func (r RunRow) statusOrDefault() string {
	if r.Status == "" {
		return "queued"
	}
	return r.Status
}

// snapshotFlag is the SQL-side switch for the dispatch-policy columns: 1 takes
// the snapshot from the jobs row, 0 leaves the columns NULL.
func (r RunRow) snapshotFlag() int {
	if r.Terminal {
		return 0
	}
	return 1
}

// InsertRun writes the row and returns its id. It does NOT stamp the
// unclaimable reason or materialize run_agencies — those are post-insert side
// effects that differ by producer (a skipped row has neither), and they stay
// in the callers where they always were.
func InsertRun(ctx context.Context, database *sql.DB, r RunRow) (string, error) {
	traceID := r.TraceID
	if traceID == "" {
		traceID = db.NewTraceID()
	}
	now := r.CreatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	// Terminal rows are complete the instant they exist.
	var startedAt, completedAt any
	if r.Terminal {
		startedAt, completedAt = now, now
	}
	var workflowRunID any
	if r.WorkflowRunID != "" {
		workflowRunID = r.WorkflowRunID
	}

	// job_source defaults to 'git' for a job row (A9). A non-job kind has no
	// definition, so an unset source is honestly NULL, not 'git'.
	var src any = r.jobSourceOrDefault()
	if r.kindOrDefault() != "job" && r.JobSource == "" {
		src = nil
	}
	var durationMs any
	if r.DurationMs != nil {
		durationMs = *r.DurationMs
	}
	uid := ""
	if !r.NoDefinition {
		uid = resolveEnqueueUID(ctx, database, r.EnqueueParams)
	}
	snap := r.snapshotFlag()

	_, err := database.ExecContext(ctx, `
		INSERT INTO runs
			(id, kind, job_name, job_source, run_type, scope, target_host,
			 status, queued_reason, triggered_by, trigger_kind,
			 concurrency_key, workflow_run_id, executor, schedule_name,
			 env_json, override_json, ssh_user, ssh_credential, agencies_json,
			 suppressed_by_calendar, started_at, completed_at, duration_ms, created_at,
			 entity_code, script_ref, content_hash,
			 checkout_repo, checkout_sha, checkout_entry, requires_json,
			 reaction_depth, reacted_to_run_id, priority, scheduled_for,
			 job_uid, script_uid)
		VALUES (?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			-- LU-6/LU-7: stamp the job's log-folder code AT ENQUEUE. runs has no FK
			-- to jobs, so resolving this at log-write time would break for a job
			-- deleted mid-run; making it a property of the run keeps the
			-- destination fixed for the run's whole life. NULL when the job has no
			-- live code (nothing has allocated one yet), which the path builder
			-- reads as the flat layout. A producer with no job supplies its own
			-- (entitycode.SystemCode) and the lookup is skipped.
			COALESCE(NULLIF(?, ''),
			         (SELECT printf('%08x', code) FROM entity_codes
			           WHERE kind = 'job' AND uid = ? AND deleted_at IS NULL)),
			(SELECT script_ref FROM jobs WHERE uid = ?),
			-- The hash of what will run. For a job built in the app that is the
			-- SCRIPT's, since dispatch reads the script's record and not the
			-- job's copy of it (execspec.ResolveCommand); for every other job it
			-- is the job's own.
			(SELECT COALESCE((SELECT s.content_hash FROM scripts s
			                   WHERE s.uid = j.script_uid AND j.source <> 'git'), j.content_hash)
			   FROM jobs j WHERE j.uid = ?),
			-- §7/RX.2: a checkout job (project_root set) pins, AT ENQUEUE, the
			-- repository its tree is fetched from, that repository's last synced
			-- commit, and the entry playbook, so a mid-run sync can't retarget the
			-- run. Body-only jobs leave all three NULL. Terminal rows (? = 0) take
			-- no snapshot at all.
			--
			-- GR-22: the repository is the SCRIPT's, not the job's. The entry and
			-- the tree are the script's files: an agency's job may use Global's
			-- script (GR-16), and a job built in the app has no repository of its
			-- own. Until 2.4.0 there was one repository and the commit was always
			-- Global's. A project job FROM GIT whose script row has gone falls back
			-- to the job's own repository and then to Global's, which is where it
			-- was pinned before. One built in the app has no repository of its own
			-- and is pinned to none (present defect 41): Global's is not its
			-- script's, and the run is refused at dispatch (execspec.ErrScriptGone).
			CASE WHEN ? = 1 THEN
				(SELECT CASE WHEN j.project_root IS NOT NULL AND j.project_root != ''
				             THEN COALESCE((SELECT s.repo_id FROM scripts s WHERE s.uid = j.script_uid), CASE WHEN j.source = 'git' THEN COALESCE(j.repo_id, 'global') END) END
				   FROM jobs j WHERE j.uid = ?) END,
			CASE WHEN ? = 1 THEN
				(SELECT CASE WHEN j.project_root IS NOT NULL AND j.project_root != ''
				             THEN (SELECT g.last_sha FROM git_repos g
				                    WHERE g.id = COALESCE((SELECT s.repo_id FROM scripts s WHERE s.uid = j.script_uid), CASE WHEN j.source = 'git' THEN COALESCE(j.repo_id, 'global') END)) END
				   FROM jobs j WHERE j.uid = ?) END,
			CASE WHEN ? = 1 THEN
				(SELECT CASE WHEN project_root IS NOT NULL AND project_root != ''
				             THEN script_path END
				   FROM jobs WHERE uid = ?) END,
			-- §5/RX.13: snapshot the job's requirement tokens (drives vault +
			-- Phase-4 claim-gating). NULL for a job with no requires.
			--
			-- RA-20a: a job with a become password additionally REQUIRES the
			-- 'become-file' token, injected here rather than left to the author.
			-- Only a protocol-v9 agent with an ansible-core new enough for
			-- --become-password-file advertises the token, so the run WAITS for a
			-- capable runner instead of being assigned to an incapable one and
			-- 409ing at manifest time. Injected rather than authored because a
			-- requirement an operator can forget is a requirement that does not
			-- exist. (Missing from two of the five writers until RR-0.)
			--
			-- ANSIBLE jobs only. Sync stores become_password_secret on a job of
			-- any run type ("stored but ignored" for the others), and until 2.3.0
			-- a shell run was claimed by an SSH pool that did not read a run's
			-- requirements, so the token on a shell run cost nothing. The local
			-- runner claims by the one claim, which does read them, and it
			-- advertises no such token: a shell job carrying an ignored become
			-- password would have waited for ever.
			--
			-- 2.4.0, present defect 39: a PROJECT job's run additionally REQUIRES
			-- the 'checkout' token, injected the same way and for the same reason.
			-- A checkout run is run only by an agent started with -allow-checkout,
			-- which is the one that advertises the token. Until now nothing asked
			-- for it at the claim: any agent with the run type took the run, and
			-- one that does not allow checkout refused it and the run failed. On a
			-- scope served by both kinds of agent the same job passed or failed by
			-- which agent polled first. The run WAITS for a capable agent now. The
			-- local runner advertises no such token and no longer takes a project
			-- job's run, which it ran as a body with the pin ignored.
			CASE WHEN ? = 1 THEN
				-- A stored value that is not a JSON list (nothing writes one; a row
				-- edited by hand could hold null, a string, or text that is not JSON)
				-- is read as no requirements: json_insert adds nothing to a value
				-- that is no list, and the run would then ask for nothing at all.
				(SELECT CASE
				          WHEN r.project_root IS NULL OR r.project_root = '' THEN r.req
				          WHEN EXISTS (
				                 SELECT 1 FROM json_each(CASE WHEN json_valid(r.req) AND json_type(r.req) = 'array' THEN r.req ELSE '[]' END)
				                 WHERE value = 'checkout')
				            THEN r.req
				          ELSE json_insert(CASE WHEN json_valid(r.req) AND json_type(r.req) = 'array' THEN r.req ELSE '[]' END, '$[#]', 'checkout')
				        END
				   FROM (SELECT project_root,
				                CASE
				                  WHEN become_password_secret IS NOT NULL AND become_password_secret != ''
				                       AND run_type = 'ansible'
				                    THEN CASE WHEN EXISTS (
				                                SELECT 1 FROM json_each(COALESCE(NULLIF(requires_json,''), '[]'))
				                                WHERE value = 'become-file')
				                              THEN requires_json
				                              ELSE json_insert(COALESCE(NULLIF(requires_json,''), '[]'), '$[#]', 'become-file')
				                         END
				                  WHEN requires_json IS NOT NULL AND requires_json != '[]'
				                    THEN requires_json
				                END AS req
				           FROM jobs WHERE uid = ?) r) END,
			?, ?, ?, ?,
			-- R2-1/R2-5: the identity, resolved once by resolveEnqueueUID.
			NULLIF(?, ''),
			-- 1290 (GR-5): WHICH script script_ref means, frozen beside it. Dispatch
			-- finds the script's reference bindings from the run (the manifest,
			-- the claim and its gates, the log redactor, the local runner), and a
			-- name is a script in every repository that has one. NULL for a job
			-- with no script, and for one whose script has gone.
			(SELECT script_uid FROM jobs WHERE uid = ?))
	`,
		traceID, r.kindOrDefault(), r.JobName, src, r.RunType, nullStr(r.Scope), nullStr(r.TargetHost),
		r.statusOrDefault(), nullStr(r.QueuedReason), r.TriggeredBy, r.TriggerKind,
		nullStr(r.ConcurrencyKey), workflowRunID, r.executorOrDefault(), nullStr(r.ScheduleName),
		nullStr(r.EnvJSON), nullStr(r.OverrideJSON), nullStr(r.SSHUser), nullStr(r.SSHCredential), r.agenciesSnapshot(ctx, database),
		nullStr(r.SuppressedByCalendar), startedAt, completedAt, durationMs, now,
		r.EntityCode, uid, uid, uid,
		snap, uid, snap, uid, snap, uid, snap, uid,
		r.ReactionDepth, nullStr(r.ReactedToRunID), r.Priority, nullStr(r.ScheduledFor),
		uid, uid)
	if err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}
	return traceID, nil
}

// agenciesSnapshot is what the row's agencies_json holds. A row that will
// execute always names an agency (agenciesJSONOrDefault). A TERMINAL row — a
// suppressed fire, a skipped step — never reaches a claim or a resolver, and
// keeps whatever its producer gave it, which for the suppression writers is
// nothing: '[]'. That is history, not a decision, and it is left as it was.
func (r RunRow) agenciesSnapshot(ctx context.Context, q agencyid.Queryer) string {
	if r.Terminal {
		if r.AgenciesJSON == "" {
			return "[]"
		}
		return r.AgenciesJSON
	}
	return r.agenciesJSONOrDefault(ctx, q)
}
