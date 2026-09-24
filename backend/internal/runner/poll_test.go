package runner

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestPollOwnership is the PP-H3 regression: a runner bearer token may only poll
// AS the runner it is bound to. A token for runner A polling runner B's id is
// rejected (404, not 403 — no existence leak), and the spoofed poll has NO side
// effects on B (heartbeat not hijacked, kill signal not swallowed, runs not
// claimed). The owning runner's own-id poll still succeeds.
func TestPollOwnership(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	aID, aTok := "runner-a", "amt_run_a"
	bID, bTok := "runner-b", "amt_run_b"
	insertRunner(t, svc, aID, "a", "online", []string{"bash"})
	insertRunner(t, svc, bID, "b", "online", []string{"bash"})
	bindRunnerToken(t, svc, aTok, aID)
	bindRunnerToken(t, svc, bTok, bID)

	// A claimed run owned by B with a pending kill signal queued for it. A
	// successful spoofed poll by A would consume (swallow) this kill.
	traceID := db.NewTraceID()
	insertJobDef(t, svc, "b-job", "bash", "echo hi", 0)
	insertClaimedRun(t, svc, traceID, "b-job", "bash", "", bID, "")
	if _, err := svc.db.Exec(`
		INSERT INTO action_queue(run_id, op, created_at) VALUES (?, 'kill', ?)`,
		traceID, now()); err != nil {
		t.Fatalf("insert kill signal: %v", err)
	}
	// A queued run matching B's caps; a spoofed claim by A would grab it.
	queuedID := db.NewTraceID()
	insertQueuedRun(t, svc, queuedID, "b-job", "bash", "")

	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	poll := func(targetID, token string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+targetID+"/poll", nil)
		req.SetPathValue("id", targetID)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	// Impersonation: A's token polling B's id → 404.
	if got := poll(bID, aTok); got != http.StatusNotFound {
		t.Fatalf("A polling B's id: got %d, want 404", got)
	}

	// Side-effect: B's heartbeat must NOT have been touched (last_seen_at still NULL).
	var lastSeen any
	if err := svc.db.QueryRow(`SELECT last_seen_at FROM runners WHERE id = ?`, bID).Scan(&lastSeen); err != nil {
		t.Fatalf("read B last_seen_at: %v", err)
	}
	if lastSeen != nil {
		t.Errorf("B last_seen_at was hijacked by impersonator: %v", lastSeen)
	}

	// Side-effect: B's pending kill must NOT have been consumed.
	var unconsumed int
	if err := svc.db.QueryRow(`
		SELECT COUNT(*) FROM action_queue WHERE run_id = ? AND consumed_at IS NULL`,
		traceID).Scan(&unconsumed); err != nil {
		t.Fatalf("count unconsumed: %v", err)
	}
	if unconsumed != 1 {
		t.Errorf("B's kill signal was swallowed by impersonator: unconsumed=%d, want 1", unconsumed)
	}

	// Side-effect: the queued run must NOT have been claimed by the impersonator.
	var qStatus string
	var qRunner any
	if err := svc.db.QueryRow(`SELECT status, runner_id FROM runs WHERE id = ?`, queuedID).
		Scan(&qStatus, &qRunner); err != nil {
		t.Fatalf("read queued run: %v", err)
	}
	if qStatus != "queued" || qRunner != nil {
		t.Errorf("queued run claimed by impersonator: status=%q runner_id=%v", qStatus, qRunner)
	}

	// The owner's own-id poll passes the guard. Flip B to draining so the handler
	// returns promptly (200 + drain) instead of entering the 30s long-poll.
	if _, err := svc.db.Exec(`UPDATE runners SET status='draining' WHERE id = ?`, bID); err != nil {
		t.Fatalf("set B draining: %v", err)
	}
	if got := poll(bID, bTok); got == http.StatusNotFound {
		t.Fatalf("B polling its own id: got 404, want non-404 (owner must pass the guard)")
	}
}
