package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// HandlePoll implements the long-poll work-assignment endpoint (A6.2).
// GET /api/v1/runners/{id}/poll
//
// The runner calls this endpoint periodically; Cronomicon blocks up to 30s
// waiting for a queued run matching the runner's capabilities, then returns
// 204 if none arrived. This doubles as the heartbeat (§6.2).
func (s *Service) HandlePoll(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	// Ownership (R1.4): the presented runner bearer must own {id}. Without this,
	// any valid runner token can poll AS any other runner — hijacking its
	// heartbeat, consuming its kill/drain signals (drainControl below marks them
	// consumed), and claiming its runs. Respond 404 (not 403) so a non-owning
	// runner cannot probe which runner ids exist — consistent with the
	// HandleGetManifest / HandleIngestLog ownership guards. This must run before
	// the last_seen_at UPDATE and before drainControl consumes anything.
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// Managed-settings ack (Phase 4). A settings-capable agent ALWAYS sends
	// settingsVersion (even "0") — its presence is how the server knows the
	// agent can receive settings. The value is the version the agent has
	// APPLIED; record it so the UI can show a pending-ack state.
	ackParam := r.URL.Query().Get("settingsVersion")
	settingsCapable := ackParam != ""
	appliedVersion := 0
	if settingsCapable {
		appliedVersion, _ = strconv.Atoi(ackParam)
	}
	var ackArg any // NULL for a pre-Phase-4 agent (keeps the stored value)
	if settingsCapable {
		ackArg = appliedVersion
	}

	// Update last_seen_at (poll = heartbeat, A6.2) and record the settings ack,
	// CLAMPED to the current settings_version — a runner cannot ack a version the
	// server never issued (a bogus-high ack would otherwise spoof the pending-ack
	// UI and suppress its own future deliveries). min(NULL, v) is NULL, so an old
	// agent's absent param leaves the stored value untouched via the COALESCE.
	// DR-Q7 rides along on the heartbeat: refreshing last_client_ip here keeps the
	// value CURRENT rather than frozen at enrollment, and costs nothing because
	// this UPDATE already runs on every poll.
	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE runners SET last_seen_at = ?,
		       last_client_ip = COALESCE(?, last_client_ip),
		       settings_acked_version = COALESCE(min(?, settings_version), settings_acked_version)
		WHERE id = ?`, now(), nullStrOrNil(httpx.ClientIP(r, s.cfg.TrustedProxyNets())), ackArg, runnerID); err != nil {
		s.log.Error("update runner last_seen_at", "runner_id", runnerID, "error", err)
	}

	// Load the runner's capabilities, status, protocol version, the stored
	// declared-config digest (Phase 5; '' for pre-550 rows), and the managed
	// settings + current version (Phase 4).
	var status, capsJSON, storedDigest, managedRaw string
	var protocolVersion, settingsVersion int
	var allowSecretInjection bool
	err := s.db.QueryRowContext(r.Context(), `
		SELECT status, capabilities, protocol_version, COALESCE(config_digest, ''),
		       COALESCE(managed_settings, ''), settings_version, allow_secret_injection
		FROM runners WHERE id = ?`, runnerID).
		Scan(&status, &capsJSON, &protocolVersion, &storedDigest, &managedRaw, &settingsVersion, &allowSecretInjection)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// The protocol floor applies to every poll, not just to registration (DM-2).
	//
	// Registration and redeclare check it too, but an agent only registers ONCE:
	// it saves its identity and, on every later start, resumes from that file
	// (agent.registerOrResume) — it re-registers only when the server 404s or
	// 401s its identity. So an agent that was registered before a server upgrade
	// keeps polling at its old protocol forever, and since v1.5.40 deleted the
	// per-feature `agent_too_old` gates there is nothing else left to refuse it.
	// Without this check the server would hand such an agent secret files,
	// check-mode flags and per-run SSH identities that its binary silently drops
	// as unknown fields — a dry run would apply for real, a `become` run would
	// run without its password. The whole argument for a single high floor is
	// that the refusal is LOUD; this is where that stays true after an upgrade.
	//
	// 426 (not 409) matches what register and redeclare answer, so the agent has
	// one status to recognise, and last_seen_at is deliberately refreshed ABOVE
	// this point: the operator needs the runner to keep showing as alive-but-
	// refused on the Runners page, not to look reaped.
	//
	// The column is NOT NULL DEFAULT 1 (migration 390), so a row that pre-dates
	// the handshake reads as 1 and is refused here too — which is correct: it
	// declared nothing, and "declared nothing" is exactly what the register
	// handler also refuses.
	if protocolVersion < runnerproto.MinProtocolVersion {
		httpx.Fail(w, http.StatusUpgradeRequired, "protocol_too_old",
			fmt.Sprintf("runner speaks protocol version %d; this server requires %d — upgrade the amadeus-runner binary and restart it (the runner keeps its identity; no deregistration needed)",
				protocolVersion, runnerproto.MinProtocolVersion))
		return
	}

	// ET-D — the file-arrival specs this agent should poll. Resolved once, HERE,
	// before any early return: writePoll attaches them to every exit, so an
	// agent's watch set is refreshed by any poll outcome including a 204 or a
	// control-only response. Empty for a non-`watch` agent.
	var declaredCaps []string
	_ = json.Unmarshal([]byte(capsJSON), &declaredCaps)
	watches := watchesForRunner(r.Context(), s.db, runnerID, declaredCaps)

	// Clamp the claimed-applied version the same way as the stored ack — a runner
	// cannot suppress delivery by claiming a version the server never issued.
	if appliedVersion > settingsVersion {
		appliedVersion = settingsVersion
	}

	// Build the settings payload to deliver this poll: only to a settings-capable
	// agent, and only while it hasn't applied the current version. Sent even when
	// the managed set is empty-but-newer (an operator cleared a field) so the
	// agent reverts to its local values.
	managedVals, _ := parseManagedSettings(managedRaw)
	var settingsPayload *runnerproto.PollSettings
	if settingsCapable && settingsVersion > appliedVersion {
		settingsPayload = &runnerproto.PollSettings{Version: settingsVersion, Values: managedVals}
	}

	// Pending operator control signals (kill/drain) for this runner's runs.
	// B5 inserts action_queue rows on operator kill; B4 delivers + clears them
	// here — the B5→B4 kill-delivery seam. drainControl CONSUMES (marks
	// consumed_at) what it returns, so once pulled the signal must be delivered
	// on THIS response — it can't be re-read on a later poll.
	control := s.drainControl(r.Context(), runnerID)

	// Draining runners do not receive new work (A6.4). `draining` alone keeps
	// the drain op: it is in-flight operator intent, resolved to `offline` by
	// drain-complete or the deadline sweep.
	if status == "draining" {
		writePoll(w, runnerproto.PollResponse{
			Control:     append(control, runnerproto.PollControl{Op: "drain"}),
			PollAfterMs: 5000,
		}, settingsPayload, watches)
		return
	}

	// An authenticated poll from an `offline` runner is proof of life: the
	// bearer token is bound to this runner id (R1.4 guard above), and after
	// "drain complete — exiting" no agent process remains, so this poll can
	// only come from a deliberate restart. Re-admit it — the reaper's
	// "re-appears and resumes its identity on the next poll" promise
	// (reaper.go). Without this, offline is a one-way trap: every poll would
	// answer with a drain op, the agent would exit 0, and nothing ever writes
	// the row back to online (RL-1). Falls through so THIS response delivers
	// any pending resync/settings/work.
	if status == "offline" {
		if !s.readmitRunner(r.Context(), runnerID) {
			// Row vanished (deregistered mid-poll) or flipped to a non-online
			// state under us: keep the old drain answer for this poll — the
			// next one takes the 404/fresh-register or drain path cleanly.
			writePoll(w, runnerproto.PollResponse{
				Control:     append(control, runnerproto.PollControl{Op: "drain"}),
				PollAfterMs: 5000,
			}, settingsPayload, watches)
			return
		}
		status = "online"
	}

	// Pending operator resync (Phase 4): deliver "re-register" and clear the
	// flag in one shot (takeResync is deliver-once, like drainControl). Delivered
	// alongside any pending kill control, immediately (not after the long-poll).
	//
	// Automatic drift detection (Phase 5): when no manual resync is pending,
	// compare the digest the agent sent with this poll against the stored
	// declared-config digest — a mismatch (e.g. env edited + restarted, binary
	// upgraded) requests the same op, flap-guarded. This is why config changes
	// propagate on the next poll after a restart, no Resync click needed.
	agentDigest := r.URL.Query().Get("configDigest")
	if s.takeResync(r.Context(), runnerID) {
		s.noteReRegisterSent(runnerID) // manual click bypasses the cooldown but arms it
		control = append(control, runnerproto.PollControl{Op: "re-register"})
	} else if s.checkConfigDrift(runnerID, agentDigest, storedDigest) {
		control = append(control, runnerproto.PollControl{Op: "re-register"})
	}

	// Host-key ops (Phase 5): a pending scan request, and any approved host keys
	// not yet delivered. Both are deliver-once.
	control = s.appendHostKeyControl(r.Context(), runnerID, control)

	// Operator control (a kill) is time-sensitive: deliver it immediately rather
	// than entering the long-poll. Otherwise a kill consumed at the top of this
	// handler would sit in `control` undelivered until the 30s long-poll returns
	// — an operator "kill now" must not wait that long.
	if len(control) > 0 {
		writePoll(w, runnerproto.PollResponse{
			Control:     control,
			PollAfterMs: 0,
		}, settingsPayload, watches)
		return
	}

	var caps []string
	_ = json.Unmarshal([]byte(capsJSON), &caps)
	// Subtract the server-managed capability mask (Phase 4, D3): the run-types it
	// lists are removed from the runner's effective claim set. Enforced here at
	// claim time (the real gate); the agent honors it too for symmetry.
	caps = effectiveClaimCaps(caps, managedVals.CapabilityMask)
	if len(caps) == 0 {
		respondControlOr204(w, control, settingsPayload, watches)
		return
	}

	// Deliver a pending managed-settings change PROMPTLY (Phase 4) rather than
	// holding it for the full 30s long-poll: return now so the agent applies it
	// and re-polls for work. Settings changes are infrequent, so this brief
	// settings-only cycle (until the agent acks) is cheap.
	if settingsPayload != nil {
		writePoll(w, runnerproto.PollResponse{Control: control, PollAfterMs: 0}, settingsPayload, watches)
		return
	}

	// Long-poll: block up to pollTimeout waiting for a matching queued run.
	deadline := time.Now().Add(pollTimeout)
	for {
		// Re-check the runner's status each iteration: an operator drain (A6.4)
		// that lands while this long-poll is already in-flight must stop us from
		// claiming newly-queued work AND be delivered as a drain control without
		// waiting the full pollTimeout. The top-of-handler guard only covers polls
		// that START after the drain; without this re-check a runner drained
		// mid-poll keeps grabbing work for up to pollTimeout (30s).
		var curStatus string
		var resyncPending int
		if err := s.db.QueryRowContext(r.Context(),
			`SELECT status, resync_requested FROM runners WHERE id = ?`, runnerID).
			Scan(&curStatus, &resyncPending); err != nil {
			// Runner vanished (deregistered/reaped) mid-poll: stop claiming.
			break
		}
		if curStatus == "draining" {
			writePoll(w, runnerproto.PollResponse{
				Control:     []runnerproto.PollControl{{Op: "drain"}},
				PollAfterMs: 5000,
			}, settingsPayload, watches)
			return
		}
		if curStatus == "offline" {
			// The runner is inside this very long-poll, so it is alive — an
			// offline flip mid-poll can only be the reaper racing a live poll
			// (e.g. clock-skewed heartbeat). Re-admit and keep polling (RL-1)
			// rather than drain a healthy agent. On failure (row deleted by a
			// concurrent deregister) stop claiming; the next poll 404s into
			// the fresh-register path.
			if !s.readmitRunner(r.Context(), runnerID) {
				break
			}
		}

		// An operator Resync that lands while this long-poll is in-flight is
		// delivered within a pollInterval, not after the 30s timeout. The cheap
		// read above gates the consuming UPDATE (takeResync) so the 500ms loop
		// doesn't hammer SQLite's write lock when nothing is pending.
		if resyncPending == 1 && s.takeResync(r.Context(), runnerID) {
			s.noteReRegisterSent(runnerID) // arm the flap-guard cooldown, same as the top-of-poll path
			writePoll(w, runnerproto.PollResponse{
				Control:     []runnerproto.PollControl{{Op: "re-register"}},
				PollAfterMs: 0,
			}, settingsPayload, watches)
			return
		}

		// Re-drain pending control each iteration: an operator kill enqueued while
		// this long-poll is already in-flight must be delivered promptly (within a
		// pollInterval), not held until the 30s long-poll returns. Without this an
		// operator "kill now" can sit undelivered for up to pollTimeout.
		if more := s.drainControl(r.Context(), runnerID); len(more) > 0 {
			writePoll(w, runnerproto.PollResponse{
				Control:     more,
				PollAfterMs: 0,
			}, settingsPayload, watches)
			return
		}

		run, err := s.claimRun(r.Context(), runnerID, caps, allowSecretInjection)
		if err != nil {
			s.log.Error("claim run", "runner_id", runnerID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if run != nil {
			metrics.RunStarted() // queued→running (C.2)
			writePoll(w, runnerproto.PollResponse{
				Assignment:  run,
				Control:     control,
				PollAfterMs: 0,
			}, settingsPayload, watches)
			return
		}

		if time.Now().After(deadline) {
			break
		}

		// Check for context cancellation before sleeping.
		select {
		case <-r.Context().Done():
			return
		case <-time.After(pollInterval):
		}
	}

	// Timeout: no work arrived. Still flush any control messages / settings.
	respondControlOr204(w, control, settingsPayload, watches)
}

// writePoll stamps the settings payload (if any) onto a 200 PollResponse and
// writes it — every poll-response exit routes through here so a pending managed
// change rides whatever the poll returns.
func writePoll(w http.ResponseWriter, resp runnerproto.PollResponse, settings *runnerproto.PollSettings, watches []runnerproto.WatchSpec) {
	resp.Settings = settings
	// ET-D: attached HERE rather than at each call site, so a new early-return
	// added later cannot forget it and leave an agent watching a stale set.
	resp.Watches = watches
	httpx.JSON(w, http.StatusOK, resp)
}

// readmitRunner flips an offline runner back to online (RL-1): an
// authenticated poll is proof of life, and restarting the agent is the
// operator's "come back" (RL-Q1). Guarded on status='offline' so a concurrent
// deregister (row gone) or a sibling poll's re-admit loses harmlessly. Load is
// already 0 on any offline row (all three offline writers zero it), so only
// status and the stale drain deadline are touched. Returns true when the row
// now reads online.
func (s *Service) readmitRunner(ctx context.Context, runnerID string) bool {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runners SET status = 'online', drain_deadline_at = NULL
		WHERE id = ? AND status = 'offline'`, runnerID)
	if err != nil {
		s.log.Error("re-admit runner", "runner_id", runnerID, "error", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Not offline anymore: either deregistered (row gone) or a sibling poll
		// already re-admitted it. Report the current truth.
		var cur string
		if err := s.db.QueryRowContext(ctx,
			`SELECT status FROM runners WHERE id = ?`, runnerID).Scan(&cur); err != nil {
			return false
		}
		return cur == "online"
	}

	// Audit entry, same shape as drain/resync (drain.go, resync.go). Actor is
	// the runner id (the agent acted by polling); target its display name.
	var name string
	_ = s.db.QueryRowContext(ctx,
		`SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name)
	_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:         now(),
		Kind:       "config",
		Actor:      "runner:" + runnerID,
		Target:     "runner:" + name,
		RunnerName: name,
		Summary:    "runner re-admitted on poll (was offline)",
	})
	s.log.Info("runner re-admitted on poll (was offline)", "runner_id", runnerID, "name", name)
	return true
}

// drainControl pulls unconsumed operator control signals for the runs assigned
// to this runner, marks them consumed, and returns them as server→runner
// messages (the B5→B4 seam). Failures are logged and treated as "no signals".
func (s *Service) drainControl(ctx context.Context, runnerID string) []runnerproto.PollControl {
	rows, err := s.db.QueryContext(ctx, `
		SELECT aq.id, aq.run_id, aq.op
		FROM action_queue aq
		JOIN runs r ON r.id = aq.run_id
		WHERE aq.consumed_at IS NULL AND r.runner_id = ?`, runnerID)
	if err != nil {
		s.log.Error("drain action_queue", "runner_id", runnerID, "error", err)
		return nil
	}
	defer rows.Close()

	var control []runnerproto.PollControl
	var ids []int64
	for rows.Next() {
		var id int64
		var runID, op string
		if err := rows.Scan(&id, &runID, &op); err != nil {
			continue
		}
		tid := runID
		control = append(control, runnerproto.PollControl{Op: op, TraceID: &tid})
		ids = append(ids, id)
	}
	consumed := now()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE action_queue SET consumed_at = ? WHERE id = ?`, consumed, id); err != nil {
			s.log.Error("mark action_queue consumed", "id", id, "error", err)
		}
	}
	return control
}

// respondControlOr204 returns 200 with control messages if any are pending,
// otherwise 204 No Content.
func respondControlOr204(w http.ResponseWriter, control []runnerproto.PollControl, settings *runnerproto.PollSettings, watches []runnerproto.WatchSpec) {
	// A 204 has no body, so pending settings force a 200 even with no control —
	// otherwise a managed change to an idle runner would never be delivered.
	//
	// ET-D adds watches to that rule, and it matters more: an idle runner is the
	// NORMAL state for a file-watching agent, so a 204 here would mean the one
	// kind of runner this feature exists for is the one kind that never receives
	// a watch list. The body is a handful of specs every 30s.
	if len(control) == 0 && settings == nil && len(watches) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writePoll(w, runnerproto.PollResponse{Control: control, PollAfterMs: 0}, settings, watches)
}

// claimRun atomically selects and claims one queued run matching the runner's
// capabilities. Returns nil if no matching run is available.
// This implements the queued→running transition owned by B4.
//
// Capability matching is entirely query-driven (§5, review R2): both the
// run-type match and the requirement-token subset match read the runner's
// `capabilities` array directly via json_each, so the statement has a STATIC
// parameter list (runnerID/ts only) instead of Go-built `IN (?,…)` placeholders.
//   - run_type ∈ capabilities (the widened run-type vocabulary).
//   - requires ⊆ capabilities: NOT EXISTS a required token absent from caps.
//     A NULL/'[]' requires_json matches every runner (json_each over empty ⇒
//     the NOT EXISTS is trivially true) — the back-compat path for pre-512 runs.
//
// The len(caps)==0 early-return avoids a pointless query for a runner with no
// capabilities at all.
func (s *Service) claimRun(ctx context.Context, runnerID string, caps []string, allowSecretInjection bool) (*runnerproto.PollAssignment, error) {
	if len(caps) == 0 {
		return nil, nil
	}
	// Secret-injection gate (P1.4, D1 = 1C). A run whose job/script declares
	// reference bindings (migration 590) injects secret material at manifest fetch,
	// so it may ONLY be claimed by a runner the operator flagged
	// allow_secret_injection. Rather than snapshot a requires token at enqueue, the
	// gate reads the source-of-truth bindings table LIVE at claim time (the same
	// state the manifest resolver reads), so it can never drift from what will
	// actually be injected. A non-injection runner leaves a binding-bearing run
	// queued for an eligible one — exactly the agency-isolation behavior, no new
	// dispatch loop. injectFlag is bound into the correlated clause below.
	//
	// The fence is DISARMED when the global injection kill-switch is off
	// (SecretsInjectionEnabled=false): the manifest skips resolution, so a
	// binding-bearing run injects nothing and must not be fenced away from ordinary
	// runners — otherwise declaring one binding would strand those runs the moment
	// the feature is disabled. With the switch off, dispatch fully reverts to
	// pre-injection behavior.
	// (L1's "flagged but pre-v6" exclusion went with the protocol floor: every
	// registered agent speaks the current protocol.) The kill-switch-off case is
	// unaffected: nothing is injected, so any runner may claim it.
	injectFlag := 0
	if !s.cfg.SecretsInjectionEnabled || allowSecretInjection {
		injectFlag = 1
	}

	// Match against the EFFECTIVE caps passed in (already masked by the caller,
	// Phase 4) rather than re-reading runners.capabilities — so the capability
	// mask actually narrows what this claim can grab. Non-nil, non-empty here.
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}

	ts := now()

	// Atomic claim: find the oldest queued, capability-matched, requirement-
	// satisfied, agency-eligible, runner-tag-pinned, injection-gated run and
	// transition it to running in a single UPDATE ... RETURNING (SQLite 3.35+).
	// Placeholders in order: runner_id, started-at ts, caps (run_type), caps
	// (requires⊆), agency runnerID ×2, pin runnerID, injectFlag.
	var (
		traceID string
		jobName string
		runType string
		scope   sql.NullString
	)
	err = s.db.QueryRowContext(ctx, `
		UPDATE runs
		SET status = 'running', runner_id = ?, started_at = ?
		WHERE id = (
			SELECT id FROM runs
			WHERE status = 'queued' AND executor = 'runner'
			  -- run_type ∈ the runner's EFFECTIVE (mask-narrowed) capability tokens
			  AND run_type IN (SELECT value FROM json_each(?))
			  -- requires ⊆ effective caps: no required token is absent from caps
			  AND NOT EXISTS (
			    SELECT 1 FROM json_each(COALESCE(runs.requires_json, '[]')) je
			    WHERE je.value NOT IN (SELECT value FROM json_each(?)))
			  -- Agency eligibility as a SET INTERSECTION (agencies plan T3.2 /
			  -- AG-Q2b): a scope may belong to several agencies, so "the run's
			  -- agency" is no longer a scalar. The run's set is snapshotted at
			  -- enqueue into runs.agencies_json (mig. 680) and materialized into
			  -- run_agencies (mig. 690) purely so this probe is a keyed lookup
			  -- rather than a per-row JSON parse — agencies_json stays authoritative.
			  --
			  -- BOTH branches are deliberately shaped to stay cheap, and the shape is
			  -- load-bearing (see poll_claim_bench_test.go for the measurements):
			  --   · the membership branch's inner IN is UNCORRELATED, so SQLite
			  --     materializes the runner's agency names once per statement;
			  --   · the general-pool branch tests the compact-JSON column with a byte
			  --     comparison instead of a correlated NOT EXISTS, which is what the
			  --     legacy predicate got for free from a plain IS NULL test.
			  AND (
			    -- tagged run → this runner must be a member of AT LEAST ONE of the
			    -- agencies the run requires
			    (COALESCE(runs.agencies_json, '[]') <> '[]'
			     AND EXISTS (SELECT 1 FROM run_agencies rag
			                 WHERE rag.run_id = runs.id
			                   AND rag.agency IN (SELECT a.name FROM runner_agencies ra
			                                      JOIN agencies a ON a.id = ra.agency_id
			                                      WHERE ra.runner_id = ?)))
			    -- untagged run → ONLY a runner with no agencies. This disjoint
			    -- general-pool rule is UNCHANGED (AG-Q3a): it is an isolation
			    -- invariant, not an implementation detail, and relaxing it in passing
			    -- would silently let agency-bound runners start taking general work.
			    OR (COALESCE(runs.agencies_json, '[]') = '[]'
			        AND NOT EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = ?))
			  )
			  -- RT-1 runner pin (mig. 1070). An unpinned run — NULL or '' — must
			  -- behave exactly as it did pre-1070, so the short-circuit comes
			  -- first and no pinned-run machinery is reachable for it.
			  --
			  -- This is ANDed with the agency branch above, deliberately and
			  -- permanently (RT-Q2): the pin NARROWS the eligible set and must
			  -- never widen it. Written as an OR — or moved inside the agency
			  -- parenthesis — it would become a route to a runner agency
			  -- isolation denies, which is the one thing this feature must not be.
			  --
			  -- Probes runner_tags, not json_each(runners.tags): the JSON form
			  -- re-parses an array per candidate row per poll, which is the
			  -- shape that measured +350% when the agency predicate was written
			  -- that way (mig. 690 header). The composite PK's leading column
			  -- makes this a seek.
			  AND (runs.runner_tag IS NULL OR runs.runner_tag = ''
			       OR EXISTS (SELECT 1 FROM runner_tags rt
			                  WHERE rt.runner_id = ? AND rt.tag = runs.runner_tag))
			  -- secret-injection gate: a run whose job/script declares reference
			  -- bindings — or that carries a per-run ssh_credential (CA-3b, an
			  -- implicit key binding whose material ships in the manifest) — is
			  -- claimable only by an allow_secret_injection runner
			  AND (
			    ? = 1
			    OR (
			      COALESCE(runs.ssh_credential, '') = ''
			      AND NOT EXISTS (
			        SELECT 1 FROM reference_bindings rb
			        -- R2F-1: by the run's frozen job identity when it has one, so a
			        -- same-named sibling's bindings cannot decide this run's gate.
			        -- The name arm serves pre-R2-1 runs, whose job_uid is NULL.
			        WHERE (rb.owner_kind = 'job'
			                AND CASE WHEN COALESCE(runs.job_uid, '') != ''
			                         THEN rb.owner_uid = runs.job_uid
			                         ELSE rb.owner_source = COALESCE(NULLIF(runs.job_source, ''), 'git')
			                              AND rb.owner_name = runs.job_name END)
			           OR (rb.owner_kind = 'script' AND rb.owner_name = runs.script_ref))
			    )
			  )
			-- QP: highest priority first, then oldest. The index
			-- idx_runs_claimable is cut (status, executor, priority DESC,
			-- created_at ASC) to match this mixed-direction sort exactly; a
			-- single-direction index cannot serve it. There are TWO claim
			-- queries — this one and sshexec's — and priority added to only
			-- one silently does nothing for the other's run types.
			ORDER BY priority DESC, created_at ASC
			LIMIT 1
		)
		RETURNING id, job_name, run_type, scope`,
		runnerID, ts, string(capsJSON), string(capsJSON), runnerID, runnerID, runnerID, injectFlag,
	).Scan(&traceID, &jobName, &runType, &scope)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// Increment runner load counter.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET load = load + 1 WHERE id = ?`, runnerID); err != nil {
		s.log.Error("increment runner load", "runner_id", runnerID, "error", err)
	}

	// Emit run-start activity (B4 seam: B4 owns queued→running transition).
	scopeVal := scope.String
	// At is the claim ts also written to runs.started_at — a fresh stamp here
	// could order the run-start after its own run row.
	_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "run-start",
		Actor:      "runner:" + runnerID,
		RunnerName: s.runnerNameOf(ctx, runnerID),
		JobName:    jobName,
		Scope:      scopeVal,
		TraceID:    traceID,
	})

	return &runnerproto.PollAssignment{
		TraceID: traceID,
		JobName: jobName,
		RunType: runType,
		Scope:   scopeVal,
		Payload: map[string]any{
			"jobName": jobName,
			"type":    runType,
			"scope":   scopeVal,
		},
		LiveLog: true, // v12: this server accepts mid-run partial log chunks
	}, nil
}
