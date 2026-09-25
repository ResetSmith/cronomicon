package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// pollAs performs an authenticated poll and decodes the response body (if any).
func pollAs(t *testing.T, svc *Service, runnerID, token string) (int, runnerproto.PollResponse) {
	t.Helper()
	as := authSvc(t, svc)
	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+runnerID+"/poll", nil)
	req.SetPathValue("id", runnerID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var pr runnerproto.PollResponse
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &pr)
	}
	return rec.Code, pr
}

// TestPollReadmitsOfflineRunner is the RL-1 regression: an authenticated poll
// from an offline runner re-admits it (status back to online, drain deadline
// cleared) instead of answering with a drain op — and the SAME poll delivers
// pending work, so a restarted agent recovers in one round-trip. Before RL-1,
// offline was a one-way trap: poll → drain → agent exits 0 → forever offline.
func TestPollReadmitsOfflineRunner(t *testing.T) {
	svc := newTestService(t)
	id, tok := "runner-readmit", "crn_run_readmit"
	insertRunner(t, svc, id, "readmit", "offline", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	// Stale drain deadline left over from a completed drain — must be cleared.
	if _, err := svc.db.Exec(`UPDATE runners SET drain_deadline_at = ? WHERE id = ?`, now(), id); err != nil {
		t.Fatalf("set drain_deadline_at: %v", err)
	}
	// A queued run the resurrected runner should claim on this same poll.
	insertJobDef(t, svc, "readmit-job", "bash", "echo hi", 0)
	traceID := db.NewTraceID()
	insertQueuedRun(t, svc, traceID, "readmit-job", "bash", "")

	code, pr := pollAs(t, svc, id, tok)
	if code != http.StatusOK {
		t.Fatalf("poll: got %d, want 200", code)
	}
	if hasOp(pr.Control, "drain") {
		t.Errorf("re-admitted runner still received a drain op: %+v", pr.Control)
	}
	if pr.Assignment == nil || pr.Assignment.TraceID != traceID {
		t.Errorf("same-poll work delivery: assignment=%+v, want trace %s", pr.Assignment, traceID)
	}

	var status string
	var deadline any
	if err := svc.db.QueryRow(`SELECT status, drain_deadline_at FROM runners WHERE id = ?`, id).
		Scan(&status, &deadline); err != nil {
		t.Fatalf("read runner row: %v", err)
	}
	if status != "online" {
		t.Errorf("status after re-admit poll: got %q, want online", status)
	}
	if deadline != nil {
		t.Errorf("drain_deadline_at not cleared: %v", deadline)
	}

	// Audit trail (RL-Q3): one activity row records the re-admission.
	var n int
	if err := svc.db.QueryRow(`
		SELECT COUNT(*) FROM activity WHERE summary = 'runner re-admitted on poll (was offline)'
		AND target = 'runner:readmit'`).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 1 {
		t.Errorf("audit rows: got %d, want 1", n)
	}
}

// TestPollReadmitDeliversPendingResync: the pre-RL-1 offline branch returned
// before the resync-delivery block, so an offline runner could never receive
// the re-register op. The fall-through must deliver it on the re-admit poll.
func TestPollReadmitDeliversPendingResync(t *testing.T) {
	svc := newTestService(t)
	id, tok := "runner-resync-readmit", "crn_run_rr"
	insertRunner(t, svc, id, "resync-readmit", "offline", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)
	if _, err := svc.db.Exec(`
		UPDATE runners SET resync_requested = 1, protocol_version = ? WHERE id = ?`,
		runnerproto.ProtocolVersion, id); err != nil {
		t.Fatalf("set resync_requested: %v", err)
	}

	code, pr := pollAs(t, svc, id, tok)
	if code != http.StatusOK {
		t.Fatalf("poll: got %d, want 200", code)
	}
	if hasOp(pr.Control, "drain") {
		t.Errorf("re-admitted runner still received a drain op: %+v", pr.Control)
	}
	if !hasOp(pr.Control, "re-register") {
		t.Errorf("pending resync not delivered on the re-admit poll: %+v", pr.Control)
	}
}

// TestPollDrainingStillDrains is the RL-Q2 regression guard: `draining` is
// in-flight operator intent (A6.4) and must keep receiving the drain op — only
// `offline` re-admits.
func TestPollDrainingStillDrains(t *testing.T) {
	svc := newTestService(t)
	id, tok := "runner-draining", "crn_run_draining"
	insertRunner(t, svc, id, "draining", "draining", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	code, pr := pollAs(t, svc, id, tok)
	if code != http.StatusOK {
		t.Fatalf("poll: got %d, want 200", code)
	}
	if !hasOp(pr.Control, "drain") {
		t.Errorf("draining runner did not receive drain op: %+v", pr.Control)
	}
	var status string
	if err := svc.db.QueryRow(`SELECT status FROM runners WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "draining" {
		t.Errorf("status: got %q, want draining (unchanged)", status)
	}
}

// TestReadmitRunnerRaces covers the guarded-UPDATE edges: a row deleted by a
// concurrent deregister reports false (next poll 404s into fresh-register);
// a sibling poll's completed re-admit reports true (status already online).
func TestReadmitRunnerRaces(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	if svc.readmitRunner(ctx, "no-such-runner") {
		t.Error("readmit of a deleted runner reported success")
	}

	insertRunner(t, svc, "runner-sibling", "sibling", "online", []string{"bash"})
	if !svc.readmitRunner(ctx, "runner-sibling") {
		t.Error("readmit after a sibling re-admit (already online) reported failure")
	}
	// No spurious audit row for the no-op path (the sibling's poll wrote its own).
	var n int
	if err := svc.db.QueryRow(`
		SELECT COUNT(*) FROM activity WHERE summary = 'runner re-admitted on poll (was offline)'`).
		Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if n != 0 {
		t.Errorf("no-op readmit wrote %d audit rows, want 0", n)
	}
}

// TestReaperOfflineThenPollResurrects is the full RL loop end to end: a runner
// the reaper offlined (stale heartbeat) comes back with nothing but its next
// authenticated poll — online again and claiming queued work — honoring the
// reaper's "re-appears and resumes its identity on the next poll" promise.
func TestReaperOfflineThenPollResurrects(t *testing.T) {
	svc := newTestService(t)
	svc.cfg.RunnerOfflineAfter = 5 * time.Minute
	svc.cfg.RunnerDeregisterAfter = 24 * time.Hour // keep the deregister sweep out of this test

	id, tok := "runner-reaped", "crn_run_reaped"
	stale := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, id, "reaped", "online", stale, stale)
	bindRunnerToken(t, svc, tok, id)

	svc.reapOnce(context.Background())
	if st, _ := runnerStatus(t, svc, id); st != "offline" {
		t.Fatalf("after reap: status %q, want offline", st)
	}

	insertJobDef(t, svc, "reaped-job", "bash", "echo hi", 0)
	traceID := db.NewTraceID()
	insertQueuedRun(t, svc, traceID, "reaped-job", "bash", "")

	code, pr := pollAs(t, svc, id, tok)
	if code != http.StatusOK {
		t.Fatalf("poll: got %d, want 200", code)
	}
	if hasOp(pr.Control, "drain") {
		t.Errorf("resurrected runner received a drain op: %+v", pr.Control)
	}
	if pr.Assignment == nil || pr.Assignment.TraceID != traceID {
		t.Errorf("assignment=%+v, want trace %s", pr.Assignment, traceID)
	}
	if st, _ := runnerStatus(t, svc, id); st != "online" {
		t.Errorf("after resurrect poll: status %q, want online", st)
	}
}
