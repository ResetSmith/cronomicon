package runner

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// The agent processes on one identity (2.5.0 C6a; present defect 70, question
// 67).
//
// The identity file names the runner, not the process. A cloned VM, a snapshot
// taken after enrolment, two containers on one volume or a restored backup are
// one runner row and two processes, and since 2.3.2 each copy's start
// (started=1) closed the other's live runs as runner_lost: their work went on
// to the end and was recorded as failed, its log refused. An agent of this
// release sends a value of its own on every poll (runnerproto.PollParamProcess)
// and the server keeps the values it has seen (runner_processes, ordered by
// first sight).
//
// A restart is one value replacing another: the old process never polls
// again. A copy is two values interleaving: an OLDER process polls after a
// newer one has started. That is the only thing read as a copy, and it raises
// runner_identity_shared.
//
// At a start, the server cannot yet tell which it is. If no other process has
// been seen within processHoldWindow, the runs it shows as running here have no
// live owner and are closed at once, as in 2.3.2. If one has, the close is
// deferred (runners.lost_runs_before): the runs started before this start are
// closed once every older process has stayed silent for the window, which is a
// crash's case (systemd restarts the agent within ten seconds), and never if
// one of them polls again, which is a copy's. Question 67 was answered "yes,
// decline started=1 while another process is live"; read literally that would
// decline every restart, because a crashed process was always seen moments
// ago. The deferral is what keeps both: a copy's runs are not failed, and a
// crash's runs are closed about a minute late instead of at once.
//
// An agent that sends no value (one older than this release) is treated as
// 2.3.2 treated every agent: nothing is detected, and nothing is declined.

// processHoldWindow is the longest a live process that holds a run goes unseen:
// the server holds its poll for up to pollTimeout, and it asks again within
// runnerproto.BusyHeartbeat of the answer. A process that holds no run may poll
// less often, but then it has no run to lose. The slack covers a slow network.
const processHoldWindow = pollTimeout + runnerproto.BusyHeartbeat + 10*time.Second

// maxProcessValueLen bounds what is stored for a process value. An agent sends
// 32 hex characters; anything longer is ignored as if absent.
const maxProcessValueLen = 128

// processWindow is how long a process value is kept after its last poll, and
// how long a copy must be gone before the notice resolves: the offline window,
// after which the reaper would call the runner itself gone.
func (s *Service) processWindow() time.Duration {
	if s.cfg != nil && s.cfg.RunnerOfflineAfter > 0 {
		return s.cfg.RunnerOfflineAfter
	}
	return 5 * time.Minute
}

// noteProcess records a poll of process value v on a runner and acts on what
// it shows. It reports whether a start (started) must NOT close the runner's
// running runs now, because another process was seen within the hold window:
// they are then closed later, or not at all (closeLostRunsIfOwnersGone).
//
// Best-effort, like the heartbeat it rides on: an error is logged and answered
// with false, so a start then closes what 2.3.2 closed.
func (s *Service) noteProcess(ctx context.Context, runnerID, v, ip string, started bool) (deferClose bool) {
	if v == "" || len(v) > maxProcessValueLen {
		return false
	}
	t := reaperNow()
	ts := t.Format(time.RFC3339)
	fail := func(what string, err error) bool {
		s.log.Error("runner process: "+what, "runner_id", runnerID, "error", err)
		return false
	}

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM runner_processes WHERE runner_id = ? AND last_seen_at < ?`,
		runnerID, t.Add(-s.processWindow()).Format(time.RFC3339)); err != nil {
		return fail("prune", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO runner_processes (runner_id, process, client_ip, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (runner_id, process) DO UPDATE SET
		    last_seen_at = excluded.last_seen_at,
		    client_ip    = COALESCE(excluded.client_ip, runner_processes.client_ip)`,
		runnerID, v, nullStrOrNil(ip), ts, ts); err != nil {
		return fail("record", err)
	}
	var seq int64
	var firstSeen string
	if err := s.db.QueryRowContext(ctx,
		`SELECT seq, first_seen_at FROM runner_processes WHERE runner_id = ? AND process = ?`,
		runnerID, v).Scan(&seq, &firstSeen); err != nil {
		return fail("read back", err)
	}

	// An older process polling after a newer one started: the identity is in
	// use twice. Whatever a start was waiting to close is a live copy's.
	var newer bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM runner_processes WHERE runner_id = ? AND seq > ?)`,
		runnerID, seq).Scan(&newer); err != nil {
		return fail("compare", err)
	}
	if newer {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE runners SET identity_shared_at = ?, lost_runs_before = NULL WHERE id = ?`,
			ts, runnerID); err != nil {
			return fail("mark shared", err)
		}
		s.raiseIdentityShared(ctx, runnerID)
	}

	if started && !newer {
		var othersLive bool
		if err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM runner_processes
			                WHERE runner_id = ? AND process <> ? AND last_seen_at >= ?)`,
			runnerID, v, t.Add(-processHoldWindow).Format(time.RFC3339)).Scan(&othersLive); err != nil {
			return fail("look for a live process", err)
		}
		if othersLive {
			if _, err := s.db.ExecContext(ctx,
				`UPDATE runners SET lost_runs_before = ? WHERE id = ?`, firstSeen, runnerID); err != nil {
				return fail("defer the close", err)
			}
			s.log.Warn("runner process started while another was seen moments ago — its runs are closed only if it stays silent",
				"runner_id", runnerID, "hold", processHoldWindow)
			deferClose = true
		} else if _, err := s.db.ExecContext(ctx,
			`UPDATE runners SET lost_runs_before = NULL WHERE id = ?`, runnerID); err != nil {
			s.log.Error("runner process: clear a deferred close", "runner_id", runnerID, "error", err)
		}
	}

	if !deferClose {
		s.closeLostRunsIfOwnersGone(ctx, runnerID, t)
	}
	if !newer {
		s.resolveIdentitySharedIfAlone(ctx, runnerID)
	}
	return deferClose
}

// closeLostRunsIfOwnersGone finishes a deferred close: once no process that
// was there before the start has been seen for processHoldWindow, the runs
// started before it are closed as runner_lost, and the runner's load drops by
// as many (the new process may hold runs of its own by now).
func (s *Service) closeLostRunsIfOwnersGone(ctx context.Context, runnerID string, t time.Time) {
	var before sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT lost_runs_before FROM runners WHERE id = ?`, runnerID).Scan(&before); err != nil || !before.Valid {
		return
	}
	var ownerLive bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM runner_processes
		                WHERE runner_id = ? AND first_seen_at < ? AND last_seen_at >= ?)`,
		runnerID, before.String, t.Add(-processHoldWindow).Format(time.RFC3339)).Scan(&ownerLive); err != nil || ownerLive {
		return
	}
	n := s.failRunningRunsBefore(ctx, runnerID, before.String)
	if n < 0 {
		return
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET lost_runs_before = NULL, load = MAX(0, load - ?) WHERE id = ?`, n, runnerID); err != nil {
		s.log.Error("runner process: finish a deferred close", "runner_id", runnerID, "error", err)
		return
	}
	if n > 0 {
		s.log.Warn("runner restarted with runs still marked running — the process before it stayed silent, closed them as runner_lost",
			"runner_id", runnerID, "runs", n)
	}
}

// raiseIdentityShared writes the runner_identity_shared notice, filed under the
// runner's owner agency (a global administrator reads every agency's).
func (s *Service) raiseIdentityShared(ctx context.Context, runnerID string) {
	var name string
	_ = s.db.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name)
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(client_ip, '') FROM runner_processes
		 WHERE runner_id = ? ORDER BY seq`, runnerID)
	if err != nil {
		s.log.Error("runner process: list for the notice", "runner_id", runnerID, "error", err)
		return
	}
	var n int
	var ips []string
	seen := map[string]bool{}
	for rows.Next() {
		var ip string
		if rows.Scan(&ip) == nil {
			n++
			if ip != "" && !seen[ip] {
				seen[ip] = true
				ips = append(ips, ip)
			}
		}
	}
	rows.Close()
	from := ""
	if len(ips) > 0 {
		from = " (seen from " + strings.Join(ips, ", ") + ")"
	}
	detail := fmt.Sprintf("Runner %s (id %s) is being polled by %d agent processes at once%s: its identity file "+
		"has been copied, by a VM cloned or a snapshot taken after it enrolled, two containers on one volume, or "+
		"a backup restored beside it. The server sees one runner: the copies take each other's Stops, drains, "+
		"settings and host-key deliveries, and History shows them as one. While both are polling, a copy's start "+
		"does not close the other's runs. To make the copy a runner of its own: on the copy, stop the agent, "+
		"delete its identity file (identity.json in its state directory), and enrol it with a registration token "+
		"and a name of its own (the runner install guide, \"One identity file, one agent\"). This notice clears "+
		"when only one process has polled for %s.", name, runnerID, n, from, s.processWindow())
	if err := notices.Upsert(ctx, s.db, notices.KindRunnerIdentityShared, notices.Finding{
		AgencyID: runnerAgency(ctx, s.db, runnerID),
		Subject:  runnerID,
		Detail:   detail,
	}); err != nil {
		s.log.Error("runner process: raise the notice", "runner_id", runnerID, "error", err)
	}
}

// resolveIdentitySharedIfAlone resolves the notice once only one process is
// left on the runner: the other's value has gone unseen for processWindow and
// was pruned.
func (s *Service) resolveIdentitySharedIfAlone(ctx context.Context, runnerID string) {
	var shared sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT identity_shared_at FROM runners WHERE id = ?`, runnerID).Scan(&shared); err != nil || !shared.Valid {
		return
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runner_processes WHERE runner_id = ?`, runnerID).Scan(&n); err != nil || n > 1 {
		return
	}
	s.clearIdentityShared(ctx, runnerID)
}

// clearIdentityShared resolves the notice and forgets the condition: one
// process left, or none (the reaper found the runner quiet).
func (s *Service) clearIdentityShared(ctx context.Context, runnerID string) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE runners SET identity_shared_at = NULL WHERE id = ?`, runnerID); err != nil {
		s.log.Error("runner process: clear shared", "runner_id", runnerID, "error", err)
		return
	}
	if err := notices.Resolve(ctx, s.db, notices.KindRunnerIdentityShared, runnerID); err != nil {
		s.log.Error("runner process: resolve the notice", "runner_id", runnerID, "error", err)
	}
}
