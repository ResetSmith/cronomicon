package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/calendar"
)

// 2.5.3 CA4b (CA-30 to CA-33): a job's and a workflow's own Forbid and Allow
// calendars gate every automatic start, and not a person's Run nor the
// promotion of a run a person deferred. A start they stop is dropped, never
// postponed, and recorded.

func forbidToday(t *testing.T, pool *sql.DB, s *Scheduler, table, name string) {
	t.Helper()
	seedCalOf(t, pool, opsAgency, "change-days", false, false, map[string]string{today(s): "Release day"})
	if _, err := pool.Exec(`UPDATE `+table+` SET forbid_calendars = '["change-days"]' WHERE name = ?`, name); err != nil {
		t.Fatal(err)
	}
}

func TestCA4b_TheMatrixGatesEveryAutomaticStart(t *testing.T) {
	for origin, want := range map[RunOrigin]bool{
		OriginCron: true, OriginToken: true, OriginReaction: true, OriginFileArrival: true, OriginPromotion: true,
		OriginManual: false, OriginPromotionAdHoc: false,
	} {
		if got := PolicyFor(origin).definitionCalendars; got != want {
			t.Errorf("%s: definition calendars = %v, want %v", origin, got, want)
		}
	}
}

func TestCA4b_TheGateRefusesWhatTheJobsCalendarsForbid(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	forbidToday(t, pool, s, "jobs", "patch")

	for _, origin := range []RunOrigin{OriginToken, OriginFileArrival, OriginPromotion} {
		g := Evaluate(ctx, pool, quietLog(), GateRequest{Origin: origin, Source: "git", OwnerKind: "job", Name: "patch", UID: "uid-patch"}, s.location())
		if g.Allowed || g.Gate != "calendar" || g.Level != "job" || !strings.Contains(g.Reason, `the job's calendar "change-days"`) {
			t.Errorf("%s: %+v, want refused by the job's calendar", origin, g)
		}
	}
	for _, origin := range []RunOrigin{OriginManual, OriginPromotionAdHoc} {
		if g := Evaluate(ctx, pool, quietLog(), GateRequest{Origin: origin, Source: "git", OwnerKind: "job", Name: "patch", UID: "uid-patch"}, s.location()); !g.Allowed {
			t.Errorf("%s: %+v, want allowed (a person's start)", origin, g)
		}
	}

	// A reaction: dropped and recorded as a calendar suppression.
	result, detail := s.reactionVerdict(ctx, srcEvent{kind: "job", source: "git", name: "upstream", completedAt: time.Now()},
		reactionRow{ownerSource: "git", ownerKind: "job", ownerName: "patch", ownerUID: "uid-patch", name: "after-upstream", enabled: true}, time.Now())
	if result != "suppressed_calendar" || !strings.Contains(detail, "change-days") {
		t.Errorf("the reaction's verdict = %s %q, want suppressed_calendar naming the job's calendar", result, detail)
	}
}

func TestCA4b_ACronFireTheJobForbidsIsSkippedAndRecorded(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "patch", 1)
	forbidToday(t, pool, s, "jobs", "patch")
	bindSchedule(t, pool, "job", "patch", "nightly", "", "")

	s.fire("git", "patch", "uid-patch", "bash", "prod", "Allow", "", "nightly", "")
	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Fatalf("%d runs queued on a day the job forbids", n)
	}
	var reason, byCal string
	if err := pool.QueryRow(`SELECT queued_reason, suppressed_by_calendar FROM runs WHERE job_name = 'patch' AND status = 'skipped'`).Scan(&reason, &byCal); err != nil {
		t.Fatalf("no skipped run recorded: %v", err)
	}
	if byCal != "change-days" || !strings.Contains(reason, "the job's calendar") {
		t.Errorf("recorded %q by %q", reason, byCal)
	}
}

// A promotion its definition's calendars refuse is dropped, not held for a
// day they allow, and recorded: an Allow miss too, though its calendar does
// not opt into recording.
func TestCA4b_APromotionTheCalendarsRefuseIsDroppedAndRecorded(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	seedCalOf(t, pool, opsAgency, "month-end", false, false, map[string]string{"2000-01-31": ""})
	if _, err := pool.Exec(`UPDATE jobs SET allow_calendars = '["month-end"]' WHERE name = 'patch'`); err != nil {
		t.Fatal(err)
	}
	parkDueRun(t, pool, "patch")
	if _, err := pool.Exec(`UPDATE pending_runs SET origin_kind = 'reaction'`); err != nil {
		t.Fatal(err)
	}
	s.PromotePending(ctx)

	var pending int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pending_runs`).Scan(&pending)
	if pending != 0 {
		t.Errorf("%d pending rows after the drop, want none: a dropped start is never postponed", pending)
	}
	if n := countRuns(t, pool, "patch", "queued"); n != 0 {
		t.Errorf("%d runs queued on a day the job does not allow", n)
	}
	var reason, byCal string
	if err := pool.QueryRow(`SELECT queued_reason, suppressed_by_calendar FROM runs WHERE job_name = 'patch' AND status = 'skipped'`).Scan(&reason, &byCal); err != nil {
		t.Fatalf("the dropped start left no record: %v", err)
	}
	if byCal != "month-end" || !strings.Contains(reason, `not a run day in the job's calendar "month-end"`) {
		t.Errorf("recorded %q by %q", reason, byCal)
	}

	// A person's deferral of the same job is promoted: its calendars do not
	// gate a hand.
	parkDueRun(t, pool, "patch")
	s.PromotePending(ctx)
	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Errorf("a person's deferred run was not promoted (%d queued)", n)
	}
}

// CA-33: a workflow's calendars decide whether it starts; its step's job's do
// not stop it. (The engine starts steps without asking the gates.)
func TestCA4b_AWorkflowStartsOnADayItsStepsJobForbids(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	s := New(pool, quietLog(), nil)
	var fired []string
	s.SetWorkflowFirer(func(_ context.Context, f WorkflowFire) { fired = append(fired, f.Name) })
	seedJobRow(t, pool, "step-job", 1)
	forbidToday(t, pool, s, "jobs", "step-job")
	seedOpsRepo(t, pool)
	if _, err := pool.ExecContext(ctx, `INSERT INTO workflows (uid, name, enabled, synced_at, repo_id, steps) VALUES ('wf-release', 'release', 1, 't', ?, '[{"job":"step-job"}]')`, opsRepo); err != nil {
		t.Fatal(err)
	}
	bindSchedule(t, pool, "workflow", "release", "nightly", "", "")
	s.fireWorkflow("wf-release", "git", "release", "nightly", "")
	if len(fired) != 1 {
		t.Fatalf("the workflow fired %v, want once: its step's job's calendars do not gate it", fired)
	}

	// Its own Forbid does.
	if _, err := pool.Exec(`UPDATE workflows SET forbid_calendars = '["change-days"]' WHERE uid = 'wf-release'`); err != nil {
		t.Fatal(err)
	}
	s.fireWorkflow("wf-release", "git", "release", "nightly", "")
	if len(fired) != 1 {
		t.Errorf("the workflow fired on a day its own calendar forbids: %v", fired)
	}
	var reason string
	if err := pool.QueryRow(`SELECT queued_reason FROM workflow_runs WHERE workflow_uid = 'wf-release' AND status = 'skipped'`).Scan(&reason); err != nil || !strings.Contains(reason, "the workflow's calendar") {
		t.Errorf("the workflow's skip = %q (%v)", reason, err)
	}
}


// The review of CA4b: a person's Run the queue parked behind an active one is
// a person's start, promoted whatever the job's calendars; a reaction's row
// they drop is recorded as the reaction's, at its due instant.
func TestCA4b_APersonsQueuedRunIsNotDroppedAndAReactionsIsRecordedAsIts(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	forbidToday(t, pool, s, "jobs", "patch")

	parkDueRun(t, pool, "patch") // the params say manual
	if _, err := pool.Exec(`UPDATE pending_runs SET gate_kind = 'concurrency'`); err != nil {
		t.Fatal(err)
	}
	s.PromotePending(ctx)
	if n := countRuns(t, pool, "patch", "queued"); n != 1 {
		t.Fatalf("a person's queued Run was not promoted on a day the job forbids (%d queued)", n)
	}

	id := parkDueRun(t, pool, "patch")
	var runAt string
	_ = pool.QueryRow(`SELECT run_at FROM pending_runs WHERE id = ?`, id).Scan(&runAt)
	if _, err := pool.Exec(`UPDATE pending_runs SET origin_kind = 'reaction' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	s.PromotePending(ctx)
	var kind, by, created string
	if err := pool.QueryRow(`SELECT trigger_kind, triggered_by, created_at FROM runs WHERE job_name = 'patch' AND status = 'skipped'`).Scan(&kind, &by, &created); err != nil {
		t.Fatalf("the dropped reaction start left no record: %v", err)
	}
	if kind != "reaction" || by != "reactor" || created != runAt {
		t.Errorf("recorded %s by %s at %s, want the reaction's, at %s", kind, by, created, runAt)
	}
}

// The review of CA4b: what a definition's own calendars say of a day is not
// hidden by an installation freeze (a service account's deferral into a
// freeze window was parked, and promoted unasked).
func TestCA4b_TheDefinitionsVerdictIsNotHiddenByAFreeze(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	ctx := context.Background()
	seedQueueJob(t, pool, "patch", "Allow")
	forbidToday(t, pool, s, "jobs", "patch")
	if _, err := pool.Exec(`INSERT INTO settings (key, value) VALUES (?, '{"on":true,"reason":"year-end"}')`, calendar.FreezeKey); err != nil {
		t.Fatal(err)
	}
	v, _, err := DefinitionVerdictAt(ctx, pool, "job", "git", "patch", "uid-patch", time.Now(), s.location())
	if err != nil || !v.Suppressed || v.Level != "job" || v.Calendar != "change-days" {
		t.Errorf("the job's verdict under a freeze = %+v (%v), want its own Forbid", v, err)
	}
}
