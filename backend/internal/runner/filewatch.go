package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
	"github.com/ResetSmith/cronomicon/internal/watchspec"
)

// File-arrival triggers, server side (ET-D,
// the prod-features plan §1.2).
//
// The agent watches; the server decides what is watchable, de-dupes what comes
// back, and fires. Two halves:
//
//   - WATCH DISTRIBUTION rides the poll response (v10). Sent on every poll, so
//     an edited watch reaches the agent with no operator action and a restarted
//     agent re-acquires the full set. Only a v10+ agent advertising the `watch`
//     capability receives them — an older one would drop the field and silently
//     never watch.
//
//   - SIGHTING INGEST is POST /runners/{id}/file-sightings, mirroring the
//     host-key upload (v5): the agent reports what it OBSERVED, and the server
//     decides what that means. The agent never triggers a run directly.
//
// # Why the agent is not trusted with the path
//
// The server tells a runner which globs to watch, so the allowlist that bounds
// them lives on the AGENT (`-watch-paths`), applied to what the server sent.
// Reversing that — trusting the agent to declare where it will look — would
// mean a compromised agent could report a sighting for any path and have the
// server fire a job with it. The server correspondingly refuses a sighting
// whose path does not match a glob it actually distributed.

// watchesForRunner returns the specs this runner should poll.
//
// Eligibility is the AGENCY intersection, not the claim rules. A watch is about
// where the file is, and the run it produces is dispatched afterwards by the
// ordinary claim path — possibly to a different runner. What must not happen is
// a runner in one department being told that a file landed in another's
// directory, which is why the agency gate is here at all.
func watchesForRunner(ctx context.Context, database *sql.DB, runnerID string, caps []string) []runnerproto.WatchSpec {
	if !hasCap(caps, "watch") {
		return nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT j.source, j.name, j.watch_json, COALESCE(j.scope,''), COALESCE(j.uid,'')
		  FROM jobs j
		 WHERE j.watch_json IS NOT NULL AND j.watch_json != ''
		   AND j.enabled = 1 AND j.deleted_at IS NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	type cand struct{ source, name, raw, scope, uid string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.source, &c.name, &c.raw, &c.scope, &c.uid); err == nil {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return nil
	}

	runnerAgencies := runnerAgencyNames(ctx, database, runnerID)
	var out []runnerproto.WatchSpec
	for _, c := range cands {
		if !watchAgencyPermits(ctx, database, c.scope, runnerAgencies) {
			continue
		}
		ws, err := watchspec.Parse(c.raw)
		if err != nil {
			continue
		}
		for _, w := range watchspec.Normalize(ws) {
			out = append(out, runnerproto.WatchSpec{
				JobSource: c.source, JobName: c.name, JobUID: c.uid,
				Path: w.Path, StableSeconds: w.StableSeconds,
			})
		}
	}
	return out
}

// watchAgencyPermits mirrors claimRun's disjoint general-pool rule (AG-Q3a): a
// scope belonging to agencies is watchable only by a member runner, and an
// unscoped job only by a runner with no agencies at all. Relaxing either half
// would let one department observe another's directories.
func watchAgencyPermits(ctx context.Context, database *sql.DB, scope string, runnerAgencies []string) bool {
	if strings.TrimSpace(scope) == "" {
		return len(runnerAgencies) == 0
	}
	jobAgencies, err := execspec.ScopeAgencies(ctx, database, scope)
	if err != nil {
		return false
	}
	if len(jobAgencies) == 0 {
		return len(runnerAgencies) == 0
	}
	for _, ja := range jobAgencies {
		if slices.Contains(runnerAgencies, ja) {
			return true
		}
	}
	return false
}

func runnerAgencyNames(ctx context.Context, database *sql.DB, runnerID string) []string {
	rows, err := database.QueryContext(ctx, `
		SELECT a.name FROM runner_agencies ra
		  JOIN agencies a ON a.id = ra.agency_id
		 WHERE ra.runner_id = ?`, runnerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func hasCap(caps []string, want string) bool {
	return slices.Contains(caps, want)
}

// ─────────────────────────────────────────────────────────────────────────────
// Sighting ingest
// ─────────────────────────────────────────────────────────────────────────────

type sightingIn struct {
	JobSource string `json:"jobSource"`
	JobName   string `json:"jobName"`
	// JobUID is echoed by a v11 agent (empty from a v10 one). Treated as a
	// CLAIM, never as authority: it is checked against the specs this runner
	// was actually sent before it selects anything.
	JobUID    string `json:"jobUid"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	MTime     string `json:"mtime"`
}

// HandleFileSightings ingests a batch of observed arrivals and fires the ones
// that are new.
//
// Ownership-guarded like the host-key upload: a runner may only report as
// itself. The de-dupe is the UNIQUE index rather than a check-then-insert,
// because two runners sharing an NFS mount reporting the same arrival in the
// same second is the ORDINARY case here, not the race — and losing that race
// must mean "already handled", not a second run of a job that ingests the file.
func (s *Service) HandleFileSightings(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	if authed, ok := auth.RunnerIDFrom(r.Context()); !ok || authed != runnerID {
		httpx.Fail(w, http.StatusForbidden, "forbidden", "a runner may only report its own sightings")
		return
	}
	var in struct {
		Sightings []sightingIn `json:"sightings"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	// What this runner was actually told to watch. A sighting for anything else
	// is refused: the agent must not be able to widen its own remit by reporting
	// a path nobody asked it to look at.
	var caps []string
	var capsJSON sql.NullString
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT capabilities FROM runners WHERE id = ?`, runnerID).Scan(&capsJSON)
	if capsJSON.Valid {
		_ = json.Unmarshal([]byte(capsJSON.String), &caps)
	}
	allowed := watchesForRunner(r.Context(), s.db, runnerID, caps)

	fired, ignored := 0, 0
	for _, sg := range in.Sightings {
		// R2-4 — the MATCHED SPEC is what fires, not the echo. The agent's report
		// is only ever a claim about which distributed watch it belongs to; once
		// that claim is matched, every identity used downstream comes from the
		// server's own copy. So a runner cannot select a job by echoing an
		// identity it was never sent, and cannot mix one spec's uid with
		// another's name.
		spec, ok := matchDistributedWatch(sg, allowed)
		if !ok {
			s.log.Warn("runner reported a sighting for a path it was not asked to watch; refusing",
				"runner", runnerID, "job", sg.JobName, "path", sg.Path)
			ignored++
			continue
		}
		ok, err := s.recordAndFireSighting(r.Context(), runnerID, sg, spec)
		if err != nil {
			s.log.Error("file watch: record sighting", "job", sg.JobName, "path", sg.Path, "err", err)
			continue
		}
		if ok {
			fired++
		} else {
			ignored++
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"fired": fired, "ignored": ignored})
}

// matchDistributedWatch finds the watch spec a reported sighting belongs to,
// and returns the SERVER's copy of it.
//
// Returning the spec rather than a bool (R2-4) is what keeps the echo from
// being authority: the caller fires the job the spec names, never the one the
// report names. A v11 agent that echoes a uid must match a spec carrying that
// same uid; a v10 agent echoes none, and the (source, name) pair matches — the
// pair being exactly correct while names are unique.
func matchDistributedWatch(sg sightingIn, allowed []runnerproto.WatchSpec) (runnerproto.WatchSpec, bool) {
	var hit runnerproto.WatchSpec
	var found bool
	for _, a := range allowed {
		if sg.JobUID != "" && a.JobUID != "" {
			if a.JobUID != sg.JobUID {
				continue
			}
		} else if a.JobName != sg.JobName || a.JobSource != sg.JobSource {
			continue
		}
		if ok, err := pathMatches(a.Path, sg.Path); err == nil && ok {
			// R2-5 — the armed refusal (RA-17's rule): a v10 agent's name pair may
			// now match two same-named jobs' watches. There is no defensible
			// silent pick between departments, so the sighting is refused; the
			// v11 uid echo is the escape.
			if found && hit.JobUID != a.JobUID {
				return runnerproto.WatchSpec{}, false
			}
			hit, found = a, true
		}
	}
	return hit, found
}

// recordAndFireSighting claims the arrival and enqueues the run. Returns false
// when the sighting was already known (another runner won the race, or the file
// has not changed since it last fired).
func (s *Service) recordAndFireSighting(ctx context.Context, runnerID string, sg sightingIn, spec runnerproto.WatchSpec) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	sightingID := db.NewID()
	// Identity comes from the SPEC (server-side), never from the report. The
	// de-dupe columns still record what was observed — a sighting is a fact
	// about a file — but which job it belongs to is the server's answer.
	res, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO file_watch_sightings
			(id, job_source, job_name, path, size_bytes, mtime, runner_id, seen_at, job_uid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?,
			COALESCE(NULLIF(?,''), (SELECT uid FROM jobs WHERE name = ? AND source = ?)))`,
		sightingID, spec.JobSource, spec.JobName, sg.Path, sg.SizeBytes, sg.MTime, runnerID, now,
		spec.JobUID, spec.JobName, spec.JobSource)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // already seen — the UNIQUE index did its job
	}

	// Resolve by uid when the spec carries one — the lookup that stays
	// unambiguous once a name may belong to more than one job. The (source,
	// name) arm serves specs distributed before uids existed and is exactly
	// correct while names are unique; R2-5 turns an ambiguous pair into a
	// refusal rather than a silent pick (the RA-17 rule).
	var runType, scope string
	var concPolicy, concKey, jobUID sql.NullString
	if spec.JobUID != "" {
		err = s.db.QueryRowContext(ctx, `
			SELECT run_type, COALESCE(scope,''), concurrency_policy, concurrency_key, uid
			  FROM jobs WHERE uid = ? AND enabled = 1 AND deleted_at IS NULL`,
			spec.JobUID).Scan(&runType, &scope, &concPolicy, &concKey, &jobUID)
	} else {
		err = s.db.QueryRowContext(ctx, `
			SELECT run_type, COALESCE(scope,''), concurrency_policy, concurrency_key, uid
			  FROM jobs WHERE source = ? AND name = ? AND enabled = 1 AND deleted_at IS NULL`,
			spec.JobSource, spec.JobName).Scan(&runType, &scope, &concPolicy, &concKey, &jobUID)
	}
	if err != nil {
		s.refuseSighting(ctx, sightingID, "the job no longer exists, is disabled, or is in the recycle bin")
		return false, nil
	}

	// FX-C — the admission gates, through the shared origin policy rather than
	// re-derived here. Pause and the fleet cap were added by hand in v1.0.0 after
	// the review found this producer missing both; the global calendar FREEZE was
	// still missing, and is the gate whose whole purpose is to stop work nobody
	// has time to supervise. An external event is no less subject to it than a
	// clock is — arguably more, since nobody chose its timing.
	//
	// Each is refused rather than deferred, matching what these gates do to a
	// cron fire: a suppressed fire is a recorded skip, not a queue. The sighting
	// row is still written (the UNIQUE index above claimed it), so the file does
	// not silently re-fire later, and the refusal reason says what stopped it.
	if g := scheduler.Evaluate(ctx, s.db, s.log, scheduler.GateRequest{
		Origin: scheduler.OriginFileArrival, Source: spec.JobSource, OwnerKind: "job", Name: spec.JobName,
	}, nil); !g.Allowed {
		s.refuseSighting(ctx, sightingID, g.Reason)
		return false, nil
	}

	// The arrival's provenance rides the run's env, in the reserved namespace —
	// the same shape RX uses for AMADEUS_REACTED_TO_*. Path only, never content
	// (PF-Q2): the job's script already runs where the file is.
	envJSON, _ := json.Marshal(map[string]string{
		"AMADEUS_WATCH_PATH": sg.Path,
		"AMADEUS_WATCH_FILE": baseName(sg.Path),
		"AMADEUS_WATCH_SIZE": fmt.Sprintf("%d", sg.SizeBytes),
	})

	scopeAgencies, _ := execspec.ScopeAgencies(ctx, s.db, scope)

	// RA-24 — the file-arrival half, and the last producer that was missing it.
	// Cron, reactions, the manual trigger and the workflow engine all refuse an
	// UNBOUND run of a job that consumes department-owned credentials; this one
	// did not, so a watched file could enqueue a run that resolves nothing and is
	// claimable by no departmental runner. Like the cron path — and unlike the
	// manual trigger, which can hand a person a 422 — there is nobody watching an
	// arrival, so it is refused with the reason recorded rather than enqueued to
	// sit unclaimable forever.
	// R2F-1 — jobUID is the row the resolution above actually landed on, so
	// both the script lookup and the binding probes read THAT job, not a
	// same-named sibling in another department.
	var scriptRef sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT script_ref FROM jobs WHERE CASE WHEN ? != '' THEN uid = ? ELSE name = ? AND source = ? END`,
		jobUID.String, jobUID.String, spec.JobName, spec.JobSource).Scan(&scriptRef)
	owners := runref.RunOwners(spec.JobSource, spec.JobName, jobUID.String, scriptRef.String)
	if scope == "" && len(scopeAgencies) == 0 {
		blocked, berr := runref.UnboundRunBlocked(ctx, s.db, owners, scope, scopeAgencies)
		if berr != nil {
			s.refuseSighting(ctx, sightingID, "could not check credential bindings: "+berr.Error())
			return false, nil
		}
		if len(blocked) > 0 {
			s.refuseSighting(ctx, sightingID, runref.QueuedReasonUnboundReferences)
			return false, nil
		}
	}
	// KB — an arrival for a key-bound job that resolves to the ssh executor is
	// refused with the reason recorded, like every other gate here: the executor
	// cannot deliver the key, and nobody is watching a file land.
	executor := scheduler.ResolveExecutor(ctx, s.db, spec.JobSource, spec.JobName, runType)
	keys, kerr := runref.KeyBindingsOnSSH(ctx, s.db, owners, executor)
	if kerr != nil {
		s.refuseSighting(ctx, sightingID, "could not check key bindings: "+kerr.Error())
		return false, nil
	}
	if len(keys) > 0 {
		s.refuseSighting(ctx, sightingID, runref.ReasonKeyBindingOnSSH)
		return false, nil
	}

	params := scheduler.EnqueueParams{
		JobName:      spec.JobName,
		JobSource:    spec.JobSource,
		JobUID:       jobUID.String,
		RunType:      runType,
		Scope:        scope,
		TriggerKind:  "webhook", // the external-event kind; see the mount comment
		TriggeredBy:  "watcher:" + runnerID,
		EnvJSON:      string(envJSON),
		Executor:     executor,
		AgenciesJSON: execspec.MarshalAgencies(scopeAgencies),
	}
	// The concurrency gate applies exactly as it does to a cron fire: an arrival
	// is a fire like any other, and a Forbid job whose run is still going must
	// not be started twice because two files landed.
	if concPolicy.Valid && cronutil.HoldsGate(concPolicy.String) {
		key := cronutil.ConcurrencyKey(concKey.String, jobUID.String, spec.JobSource, spec.JobName)
		params.ConcurrencyKey = key
		params.Policy = concPolicy.String
		if conflict, cerr := scheduler.CheckForbid(ctx, s.db, key); cerr == nil && conflict {
			if concPolicy.String == "Queue" {
				if queued, qerr := scheduler.TryQueue(ctx, s.db, params); qerr == nil && queued {
					s.markSightingRun(ctx, sightingID, "", "queued behind an active run")
					return true, nil
				}
			}
			s.refuseSighting(ctx, sightingID, "a run for this job is already active (concurrency policy)")
			return false, nil
		}
	}

	traceID, err := scheduler.EnqueueRunWithID(ctx, s.db, params)
	if err != nil {
		s.refuseSighting(ctx, sightingID, "could not enqueue: "+err.Error())
		return false, nil
	}
	s.markSightingRun(ctx, sightingID, traceID, "")
	s.log.Info("file watch: arrival started a run",
		"job", spec.JobName, "path", sg.Path, "trace", traceID, "runner", runnerID)
	return true, nil
}

// refuseSighting records WHY an arrival produced no run.
//
// The row is the durable answer to "the file landed, why did nothing happen?" —
// the question this feature would otherwise make unanswerable, since there is
// no run to look at and the reason would live only in a log line.
func (s *Service) refuseSighting(ctx context.Context, sightingID, reason string) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE file_watch_sightings SET refused_reason = ? WHERE id = ?`, reason, sightingID); err != nil {
		s.log.Error("file watch: record refusal", "sighting", sightingID, "err", err)
	}
}

func (s *Service) markSightingRun(ctx context.Context, sightingID, runID, note string) {
	_, _ = s.db.ExecContext(ctx,
		`UPDATE file_watch_sightings SET run_id = ?, refused_reason = ? WHERE id = ?`,
		nullStrOrNil(runID), nullStrOrNil(note), sightingID)
}

// nullStrOrNil maps "" to SQL NULL, so an unset run id and an unset refusal
// reason are both absent rather than empty strings.
func nullStrOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// pathMatches applies the glob the way the agent does, so the server's refusal
// check and the agent's match cannot disagree about what a spec covers.
func pathMatches(glob, path string) (bool, error) {
	return filepath.Match(glob, path)
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 && i+1 < len(p) {
		return p[i+1:]
	}
	return p
}
