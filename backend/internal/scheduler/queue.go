package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// The Queue concurrency policy (QP, the prod-features plan §5).
//
// Allow lets runs overlap. Forbid loses the fire — it is recorded as a skipped
// run, which is honest but is rarely what an operator meant. Queue is the third
// answer: park the fire and run it when the gate clears.
//
// # A queued run is a pending run
//
// Deliberately no new table. The AR work (v0.55.20) already built parking:
// pending_runs holds a frozen EnqueueParams and a 15-second promoter that
// re-judges the cap and the Forbid gate before firing. A queued run is the same
// row with a different fire condition — "the gate cleared" instead of "the clock
// reached T" — and promoteOne's existing retry-not-fail behaviour on a held gate
// IS the queue, with no new loop.
//
// Two consequences fall out of that reuse, both wanted:
//
//   - run_at is set to NOW, so the row is immediately due and retries every
//     pass. That also means AR's 24-hour catch-up grace applies verbatim: a run
//     queued behind a permanently wedged job expires to `missed` rather than
//     waiting forever (PF-Q10).
//   - Promotion inherits exactly the gates promoteOne already applies —
//     staleness, the global cap, definition existence, and the key. It does NOT
//     acquire calendar or pause checks, because promoteOne has never had them
//     (the reactor carries its own); pretending otherwise would change AR and RX
//     behaviour as a side effect of this feature.

// QueueCap is the maximum number of runs that may be parked behind one
// concurrency key.
//
// A Go constant rather than a setting, following the reaction depth ceiling's
// reasoning verbatim: this is a safety backstop, not a tuning knob, and exposing
// it invites raising it to 500 — which converts a contained backlog into an
// uncontained one. An unbounded queue behind a stuck job is the runaway this
// exists to prevent.
//
// Three is chosen to be obviously a queue rather than obviously a buffer: it
// absorbs a slow run overlapping the next fire or two, and refuses to accumulate
// a night's worth of work that would all stampede at 6am when the gate finally
// cleared.
const QueueCap = 3

// QueueFullReason is the queued_reason recorded when the cap refuses a fire. It
// is a dedupeEpisodeReason text, so it is load-bearing — see skipRecord.Reason.
const QueueFullReason = "Skipped: the concurrency queue for this job is full"

// TryQueue parks a fire behind a held concurrency gate.
//
// Returns queued=false when the cap is already reached, which the caller must
// surface as a recorded skip rather than silence — a fire that is neither run
// nor queued nor recorded is the invisible failure this whole area of the
// codebase keeps having to fix.
func TryQueue(ctx context.Context, database *sql.DB, p EnqueueParams) (queued bool, err error) {
	key := p.ConcurrencyKey
	if key == "" {
		return false, fmt.Errorf("queue: refusing to park a run with no concurrency key")
	}

	var depth int
	if err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_runs
		 WHERE concurrency_key = ? AND gate_kind = 'concurrency' AND status = 'pending'`,
		key).Scan(&depth); err != nil {
		return false, fmt.Errorf("queue: count depth: %w", err)
	}
	if depth >= QueueCap {
		return false, nil
	}

	// The policy rides the frozen params so promotion re-judges the gate without
	// a jobs-table join a rename could invalidate — the same reason AR froze it.
	p.Policy = cronutil.PolicyQueue
	blob, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("queue: marshal params: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO pending_runs
			(id, kind, name, source, scope, run_at, scheduled_by, created_at,
			 status, params_json, concurrency_key, gate_kind, owner_uid)
		VALUES (?, 'job', ?, ?, ?, ?, ?, ?, 'pending', ?, ?, 'concurrency',
			(SELECT uid FROM jobs WHERE name = ? AND source = ?))`,
		db.NewID(), p.JobName, p.jobSourceOrDefault(), nullStr(p.Scope),
		now, // due immediately; the GATE is what holds it, not the clock
		queuedBy(p), now, string(blob), key,
		p.JobName, p.jobSourceOrDefault()); err != nil {
		return false, fmt.Errorf("queue: insert pending row: %w", err)
	}
	return true, nil
}

// queuedBy records who caused the parked run, for the Upcoming list's "scheduled
// by" column. A cron fire has no human behind it.
func queuedBy(p EnqueueParams) string {
	if p.TriggeredBy != "" {
		return p.TriggeredBy
	}
	return "scheduler"
}

// QueueDepth returns how many runs are parked behind a key. Used by the UI and
// by the cap-refusal message, so an operator sees a number rather than "full".
func QueueDepth(ctx context.Context, database *sql.DB, key string) int {
	if key == "" {
		return 0
	}
	var n int
	_ = database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_runs
		 WHERE concurrency_key = ? AND gate_kind = 'concurrency' AND status = 'pending'`,
		key).Scan(&n)
	return n
}
