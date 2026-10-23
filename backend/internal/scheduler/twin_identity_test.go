package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Two agencies may each hold a definition of one name (R2-5), and each may give
// it a schedule entry of one name. A pause, a working calendar, the watch for a
// missed fire and the record of a skipped one are each of ONE definition
// (2.4.0 Phase R6; left by the review of Phase R5). Each was found by source,
// kind and name: pausing one agency's `nightly` paused the other's; one fired,
// or did not, on the other's calendars; one's pause decided what was expected
// of both; and one's recorded skip stood for the other's, whose own was then
// never written.
func TestTwoDefinitionsOfOneNameAreEachPausedTimedAndWatchedAsThemselves(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, created_at) VALUES
	        ('w-fin', 'nightly', 'cronomicon', '[]', 1, 't'), ('w-tax', 'nightly', 'cronomicon', '[]', 1, 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at) VALUES
	        ('j-fin', 'load', 'cronomicon', 'bash', 'Allow', 1, 't'), ('j-tax', 'load', 'cronomicon', 'bash', 'Allow', 1, 't')`)
	// Each has an entry called at-two. FIN's entries skip on FIN's calendar.
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, skip_calendars) VALUES
	        ('cronomicon', 'workflow', 'nightly', 'at-two', '0 2 * * *', 0, 'w-tax', NULL),
	        ('cronomicon', 'workflow', 'nightly', 'at-two', '0 2 * * *', 0, 'w-fin', '["fin-closed"]'),
	        ('cronomicon', 'job',      'load',    'at-two', '0 2 * * *', 0, 'j-tax', NULL),
	        ('cronomicon', 'job',      'load',    'at-two', '0 2 * * *', 0, 'j-fin', '["fin-closed"]')`)

	s := New(pool, quietLog(), nil)
	var mu sync.Mutex
	started := map[string]int{}
	s.SetWorkflowFirer(func(_ context.Context, f WorkflowFire) {
		mu.Lock()
		started[f.UID]++
		mu.Unlock()
	})
	flowsStarted := func(uid string) int {
		mu.Lock()
		defer mu.Unlock()
		return started[uid]
	}
	rows := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	fireJob := func(uid string) { s.fire("cronomicon", "load", uid, "bash", "", "Allow", "", "at-two", "") }
	fireFlow := func(uid string) { s.fireWorkflow(uid, "cronomicon", "nightly", "at-two", "") }

	// ── The calendar: FIN's is closed today, TAX's names none. ──────────────
	seedCal(t, pool, "fin-closed", false, true, map[string]string{today(s): "FIN is closed"})
	fireFlow("w-tax")
	fireJob("j-tax")
	if flowsStarted("w-tax") != 1 || rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-tax' AND status = 'queued'`) != 1 {
		t.Fatalf("TAX's definitions, which name no calendar, did not fire on a day FIN's calendar is closed: workflow %d, job runs queued %d",
			flowsStarted("w-tax"), rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-tax' AND status = 'queued'`))
	}
	fireFlow("w-fin")
	fireJob("j-fin")
	if flowsStarted("w-fin") != 0 || rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-fin' AND status = 'queued'`) != 0 {
		t.Fatalf("FIN's definitions fired on a day their calendar is closed")
	}
	if rows(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_uid = 'w-fin' AND status = 'skipped' AND suppressed_by_calendar = 'fin-closed'`) != 1 ||
		rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-fin' AND status = 'skipped' AND suppressed_by_calendar = 'fin-closed'`) != 1 {
		t.Fatalf("FIN's suppressed fires were not recorded as FIN's")
	}
	exec(`DELETE FROM calendar_days`)
	exec(`DELETE FROM calendars`)
	exec(`DELETE FROM runs`)
	exec(`DELETE FROM workflow_runs`)
	mu.Lock()
	started = map[string]int{}
	mu.Unlock()

	// ── The pause: FIN's are paused, by their identity. ─────────────────────
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid) VALUES
	        ('cronomicon', 'workflow', 'nightly', 'u', 't', 'w-fin'), ('cronomicon', 'job', 'load', 'u', 't', 'j-fin')`)
	for kind, uids := range map[string][2]string{"workflow": {"w-fin", "w-tax"}, "job": {"j-fin", "j-tax"}} {
		name := map[string]string{"workflow": "nightly", "job": "load"}[kind]
		if !IsPausedUID(ctx, pool, "cronomicon", kind, name, uids[0]) {
			t.Errorf("FIN's %s, which is paused, reads as not paused", kind)
		}
		if IsPausedUID(ctx, pool, "cronomicon", kind, name, uids[1]) {
			t.Errorf("TAX's %s reads as paused: FIN's of the same name is", kind)
		}
		// A caller with no identity is answered by the name, as before.
		if !IsPaused(ctx, pool, "cronomicon", kind, name) {
			t.Errorf("by the name alone, a %s of that name is paused", kind)
		}
		// The gate every other producer asks.
		for i, paused := range []bool{true, false} {
			g := Evaluate(ctx, pool, quietLog(), GateRequest{Origin: OriginPromotion, Source: "cronomicon", OwnerKind: kind, Name: name, UID: uids[i]}, s.location())
			if (g.Gate == "pause") != paused {
				t.Errorf("the gate for the %s %s: %+v, want paused = %v", kind, uids[i], g, paused)
			}
		}
	}
	fireFlow("w-tax")
	fireJob("j-tax")
	fireFlow("w-fin")
	fireJob("j-fin")
	if flowsStarted("w-tax") != 1 || flowsStarted("w-fin") != 0 {
		t.Fatalf("with FIN's nightly paused: TAX's started %d times and FIN's %d, want 1 and 0", flowsStarted("w-tax"), flowsStarted("w-fin"))
	}
	if rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-tax' AND status = 'queued'`) != 1 || rows(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-fin' AND status = 'queued'`) != 0 {
		t.Fatalf("with FIN's load paused: TAX's or FIN's job did not do as its own pause says")
	}

	// ── The recorded skip: FIN's does not stand for TAX's. ──────────────────
	if rows(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_uid = 'w-fin' AND status = 'skipped'`) != 1 {
		t.Fatalf("FIN's paused fire left no skipped row of its own")
	}
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid) VALUES ('cronomicon', 'workflow', 'nightly', 'u', 't', 'w-tax')`)
	fireFlow("w-tax")
	if rows(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_uid = 'w-tax' AND status = 'skipped'`) != 1 {
		t.Fatalf("TAX's paused fire was taken to be recorded already, by FIN's row of the same name and entry")
	}
	fireFlow("w-tax") // the same episode again: one row stands for it
	if rows(`SELECT COUNT(*) FROM workflow_runs WHERE workflow_uid = 'w-tax' AND status = 'skipped'`) != 1 {
		t.Errorf("TAX's second paused fire of the episode wrote a second row")
	}
	exec(`DELETE FROM paused_jobs WHERE owner_uid = 'w-tax'`)

	// ── The watch for a missed fire. ────────────────────────────────────────
	entries, err := s.scheduleEntriesForDetection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	watched := map[string]int{}
	for _, e := range entries {
		watched[e.ownerUID]++
	}
	if watched["w-tax"] != 1 || watched["j-tax"] != 1 || watched["w-fin"] != 0 || watched["j-fin"] != 0 || len(entries) != 2 {
		t.Fatalf("the entries watched are %v, want TAX's two (each once) and none of FIN's, which are paused", watched)
	}
	// A fire of FIN's does not explain TAX's missing one.
	at := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	exec(`DELETE FROM runs`)
	exec(`DELETE FROM workflow_runs`)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, schedule_name, status, triggered_by, trigger_kind, created_at)
	        VALUES ('wr-fin', 'nightly', 'cronomicon', 'w-fin', 'at-two', 'success', 'schedule', 'scheduled', ?)`, at.Format(time.RFC3339))
	exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, schedule_name, status, triggered_by, trigger_kind, created_at)
	        VALUES ('r-fin', 'load', 'cronomicon', 'j-fin', 'bash', 'at-two', 'success', 'schedule', 'scheduled', ?)`, at.Format(time.RFC3339))
	for _, e := range entries {
		got, err := s.fireExplained(ctx, e, at, s.location())
		if err != nil {
			t.Fatal(err)
		}
		if got {
			t.Errorf("the %s %s's fire at %s reads as explained: by the other agency's run of the same name and entry", e.ownerKind, e.ownerUID, at.Format(time.RFC3339))
		}
		fin := e
		fin.ownerUID = map[string]string{"w-tax": "w-fin", "j-tax": "j-fin"}[e.ownerUID]
		if got, err := s.fireExplained(ctx, fin, at, s.location()); err != nil || !got {
			t.Errorf("FIN's own %s's fire, which did run, reads as not explained (%v)", e.ownerKind, err)
		}
	}

	// ── Switched off: TAX's are, FIN's are not. ─────────────────────────────
	exec(`DELETE FROM paused_jobs`)
	exec(`DELETE FROM runs`)
	exec(`UPDATE jobs SET enabled = 0 WHERE uid = 'j-tax'`)
	exec(`UPDATE workflows SET enabled = 0 WHERE uid = 'w-tax'`)
	for kind, uids := range map[string][2]string{"workflow": {"w-fin", "w-tax"}, "job": {"j-fin", "j-tax"}} {
		name := map[string]string{"workflow": "nightly", "job": "load"}[kind]
		for i, off := range []bool{false, true} {
			g := Evaluate(ctx, pool, quietLog(), GateRequest{Origin: OriginPromotion, Source: "cronomicon", OwnerKind: kind, Name: name, UID: uids[i]}, s.location())
			if (g.Gate == "disabled") != off {
				t.Errorf("the gate for the %s %s: %+v, want switched off = %v", kind, uids[i], g, off)
			}
			if enabled, known := s.definitionEnabled(ctx, "cronomicon", kind, name, uids[i]); !known || enabled == off {
				t.Errorf("the reactor's reading of the %s %s: enabled %v (known %v), want %v", kind, uids[i], enabled, known, !off)
			}
		}
	}

	// ── A pause that records no owner is of the name, as before. ────────────
	exec(`DELETE FROM paused_jobs`)
	exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid) VALUES ('cronomicon', 'workflow', 'nightly', 'u', 't', NULL)`)
	if !IsPausedUID(ctx, pool, "cronomicon", "workflow", "nightly", "w-fin") || !IsPausedUID(ctx, pool, "cronomicon", "workflow", "nightly", "w-tax") {
		t.Errorf("a pause of the name that records no owner does not pause both: whose it is cannot be told")
	}
}
