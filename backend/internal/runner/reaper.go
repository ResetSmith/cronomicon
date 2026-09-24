package runner

import (
	"context"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/notify"
)

// reaperInterval is how often the stale-runner reaper sweeps. ~60s keeps the
// offline detection responsive (the threshold is minutes, the deregister
// window is days) without churning the DB.
const reaperInterval = 60 * time.Second

// reaperNow is a clock seam so tests can advance time. It mirrors the nowUTC
// seam in internal/db/retention.go.
var reaperNow = func() time.Time { return time.Now().UTC() }

// StartReaper launches the stale-runner sweep in a goroutine (R3 / V1.1-8.4),
// mirroring db.StartRetention: a ticker plus ctx.Done() cancellation. It returns
// immediately; the worker stops when ctx is cancelled.
//
// last_seen_at is written on every long-poll (HandlePoll), but nothing else
// sweeps it — a crashed runner stays 'online' forever and its 'running' runs
// hang forever. Each tick:
//
//  1. Offline sweep (R3.1/D7): online/draining runners whose last heartbeat is
//     older than cfg.RunnerOfflineAfter become 'offline'.
//  2. Orphaned-run reconcile (R3.2/D6): each newly-offlined runner's 'running'
//     runs are driven terminal (failure / queued_reason='runner_lost') through
//     the same emitRunTerminal seam the drain-timeout path uses, so metrics +
//     notifications never get skipped.
//  3. Deregister sweep (D4): runners offline beyond cfg.RunnerDeregisterAfter
//     are fully removed (tokens revoked, row deleted).
func (s *Service) StartReaper(ctx context.Context) {
	if s.shutdownWG != nil {
		s.shutdownWG.Add(1)
	}
	go func() {
		if s.shutdownWG != nil {
			defer s.shutdownWG.Done()
		}
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reapOnce(ctx)
			}
		}
	}()
}

// reapOnce performs one full sweep. Split out so a test can drive a single pass
// deterministically (no ticker).
func (s *Service) reapOnce(ctx context.Context) {
	s.sweepOffline(ctx)
	s.sweepDeregister(ctx)
}

// sweepOffline marks online/draining runners with a stale heartbeat as offline
// and reconciles their orphaned runs (R3.1 + R3.2).
//
// "stale heartbeat" uses last_seen_at when present (an RFC3339 UTC string, see
// now() in service.go), falling back to registered_at for a runner that was
// created long ago but never polled (NULL last_seen_at). Both are parsed with
// time.Parse(time.RFC3339, ...).
func (s *Service) sweepOffline(ctx context.Context) {
	cutoff := reaperNow().Add(-s.cfg.RunnerOfflineAfter)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, last_seen_at, registered_at
		FROM runners
		WHERE status IN ('online','draining')`)
	if err != nil {
		s.log.Error("reaper: query live runners", "error", err)
		return
	}
	var stale []string
	for rows.Next() {
		var id, registeredAt string
		var lastSeen *string
		if err := rows.Scan(&id, &lastSeen, &registeredAt); err != nil {
			s.log.Error("reaper: scan runner row", "error", err)
			continue
		}
		// Prefer last_seen_at; fall back to registered_at when the runner has
		// never polled (NULL last_seen_at).
		hb := registeredAt
		if lastSeen != nil && *lastSeen != "" {
			hb = *lastSeen
		}
		t, perr := time.Parse(time.RFC3339, hb)
		if perr != nil {
			// Unparseable timestamp ⇒ treat as stale (better to reap a runner
			// with a corrupt heartbeat than leave it stuck online forever).
			s.log.Warn("reaper: unparseable heartbeat, treating as stale",
				"runner_id", id, "heartbeat", hb, "error", perr)
			stale = append(stale, id)
			continue
		}
		if t.Before(cutoff) {
			stale = append(stale, id)
		}
	}
	_ = rows.Err()
	rows.Close()

	for _, id := range stale {
		s.offlineRunner(ctx, id)
	}
}

// offlineRunner marks one stale runner offline and reconciles its in-flight
// runs to terminal. Closely mirrors forceOfflineOnDrainTimeout (log.go) — the
// difference is the marker: queued_reason='runner_lost' (D6) vs 'drain_timeout'.
func (s *Service) offlineRunner(ctx context.Context, runnerID string) {
	ts := now()
	nowT := reaperNow()

	// Collect the runner's still-running runs (and started_at to compute duration).
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, job_name, COALESCE(scope,''), COALESCE(started_at,'')
		FROM runs
		WHERE runner_id = ? AND status = 'running'`, runnerID)
	if err != nil {
		s.log.Error("reaper: query running runs", "runner_id", runnerID, "error", err)
		return
	}
	// One lookup per sweep, not per orphan: every run in this loop belongs to
	// the single runner being reaped (AA-1).
	runnerName := s.runnerNameOf(ctx, runnerID)

	var orphans [][4]string
	for rows.Next() {
		var tid, jn, sc, startedAt string
		if err := rows.Scan(&tid, &jn, &sc, &startedAt); err == nil {
			orphans = append(orphans, [4]string{tid, jn, sc, startedAt})
		}
	}
	_ = rows.Err()
	rows.Close()

	for _, run := range orphans {
		tid, jn, sc, startedAt := run[0], run[1], run[2], run[3]

		// Compute duration_ms from started_at when available (D6 keeps status
		// 'failure'; the runner_lost marker is what the UI renders distinctly).
		var durationMs int64
		if startedAt != "" {
			if st, perr := time.Parse(time.RFC3339, startedAt); perr == nil {
				if d := nowT.Sub(st).Milliseconds(); d > 0 {
					durationMs = d
				}
			}
		}

		// RX-6 — status-guarded, matching the SSH reaper's reconcileOrphan. The
		// orphan SELECT and this write straddle a reaper tick, so a run an
		// operator stopped in between (or one whose late log landed) must keep
		// the terminal state it already reached rather than being rewritten as
		// runner_lost — and must not double-emit run-end.
		res, err := s.db.ExecContext(ctx, `
			UPDATE runs
			SET status = 'failure', queued_reason = 'runner_lost',
			    completed_at = ?, duration_ms = ?
			WHERE id = ? AND status = 'running'`, ts, durationMs, tid)
		if err != nil {
			s.log.Error("reaper: finalize orphaned run", "trace_id", tid, "error", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // already terminal — don't overwrite or double-emit
		}
		// run-end activity, actor 'system' (mirrors the drain-timeout path). It
		// shares ts with the completed_at just written above.
		if err := auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
			At:         ts,
			Kind:       "run-end",
			Outcome:    "failure",
			Actor:      "system",
			RunnerName: runnerName,
			JobName:    jn,
			Scope:      sc,
			TraceID:    tid,
		}); err != nil {
			s.log.Error("reaper: insert run-end activity", "trace_id", tid, "error", err)
		}
		// Same terminal seam as finalizeRun / drain-timeout: a runner_lost
		// failure must also emit the RunFinished metric and fire notifications.
		s.emitRunTerminal(notify.RunEvent{TraceID: tid, JobName: jn, Scope: sc, Status: "failure"})
	}

	// Flip the runner to offline and zero its load. We deliberately do NOT stamp
	// an offline_at column: there is none today, and D4's deregister sweep reuses
	// last_seen_at as the "offline since" proxy (an offline runner stops updating
	// it), so adding a column would buy nothing. A reaped runner re-appears and
	// resumes its identity on the next register/poll.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET status = 'offline', load = 0 WHERE id = ?`, runnerID); err != nil {
		s.log.Error("reaper: mark runner offline", "runner_id", runnerID, "error", err)
		return
	}
	s.log.Info("reaper: runner offlined", "runner_id", runnerID, "orphaned_runs", len(orphans))
}

// sweepDeregister fully removes runners that have been offline longer than
// cfg.RunnerDeregisterAfter (D4). "Offline since when" is derived from
// last_seen_at — an offline runner stops updating it, so now-last_seen_at is a
// reasonable proxy; this avoids a schema migration for an offline_at column.
// A runner with NULL last_seen_at falls back to registered_at (it never polled
// and has been offline since registration). Mirrors HandleDeregisterRunner:
// revoke runner_tokens by created_by='runner:'+name, then DELETE the row.
func (s *Service) sweepDeregister(ctx context.Context) {
	cutoff := reaperNow().Add(-s.cfg.RunnerDeregisterAfter)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, last_seen_at, registered_at
		FROM runners
		WHERE status = 'offline'`)
	if err != nil {
		s.log.Error("reaper: query offline runners", "error", err)
		return
	}
	type stale struct{ id, name string }
	var toRemove []stale
	for rows.Next() {
		var id, name, registeredAt string
		var lastSeen *string
		if err := rows.Scan(&id, &name, &lastSeen, &registeredAt); err != nil {
			s.log.Error("reaper: scan offline runner row", "error", err)
			continue
		}
		since := registeredAt
		if lastSeen != nil && *lastSeen != "" {
			since = *lastSeen
		}
		t, perr := time.Parse(time.RFC3339, since)
		if perr != nil {
			// Don't delete on a parse error in the destructive sweep — log and
			// skip rather than risk removing a runner on a corrupt timestamp.
			s.log.Warn("reaper: unparseable offline timestamp, skipping deregister",
				"runner_id", id, "timestamp", since, "error", perr)
			continue
		}
		if t.Before(cutoff) {
			toRemove = append(toRemove, stale{id: id, name: name})
		}
	}
	_ = rows.Err()
	rows.Close()

	for _, r := range toRemove {
		s.deregisterRunner(ctx, r.id, r.name)
	}
}

// deregisterRunner revokes a runner's tokens and deletes its row in one tx,
// reusing the logic in HandleDeregisterRunner (register.go).
func (s *Service) deregisterRunner(ctx context.Context, runnerID, name string) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		s.log.Error("reaper: begin deregister tx", "runner_id", runnerID, "error", err)
		return
	}
	defer tx.Rollback() //nolint:errcheck

	ts := now()
	if _, err := tx.ExecContext(ctx, `
		UPDATE runner_tokens SET revoked_at = ?
		WHERE created_by = ? AND revoked_at IS NULL`,
		ts, "runner:"+name); err != nil {
		s.log.Error("reaper: revoke runner tokens", "runner_id", runnerID, "error", err)
	}
	// DR-7: capture placement BEFORE the delete cascades runner_agencies away.
	// This path matters MORE than the operator one for disaster recovery: during
	// an outage runners go offline, get reaped here, and return to a server with
	// no record of them. Bailing on failure leaves the runner in place, and the
	// next sweep retries — self-healing, and better than a silent loss.
	if err := capturePlacement(ctx, tx, runnerID, name, "system", deregisterViaReaper, ts); err != nil {
		s.log.Error("reaper: capture runner placement", "runner_id", runnerID, "error", err)
		return
	}
	// DRF-4 / DRF-Q5: the reap is an event, not just an absence. Naming the
	// window distinguishes a timeout from a deliberate removal without a join.
	if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      "system",
		Target:     "runner:" + name,
		RunnerName: name,
		Summary:    "deregistered (offline past " + s.cfg.RunnerDeregisterAfter.String() + ")",
	}); err != nil {
		s.log.Error("reaper: record deregistration", "runner_id", runnerID, "error", err)
		return
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM runners WHERE id = ?`, runnerID); err != nil {
		s.log.Error("reaper: delete runner", "runner_id", runnerID, "error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		s.log.Error("reaper: commit deregister", "runner_id", runnerID, "error", err)
		return
	}
	s.log.Info("reaper: runner deregistered (offline past deregister window)",
		"runner_id", runnerID, "name", name)
}
