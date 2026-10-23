package runner

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// The agent processes on one identity (2.5.0 C6a; present defect 70).

// processFixture is one runner, owned by an agency, with one run that its
// first process claimed a minute before the clock starts.
type processFixture struct {
	svc         *Service
	id, tok     string
	t0          time.Time
	lostTraceID string
}

func newProcessFixture(t *testing.T) *processFixture {
	t.Helper()
	svc := newTestService(t)
	f := &processFixture{svc: svc, id: "runner-copied", tok: "crn_run_copied",
		t0: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), lostTraceID: "trace-held-by-a"}
	insertAgencyRow(t, svc, "ag-fin", "Finance")
	insertRunner(t, svc, f.id, "copied", "online", []string{"bash"})
	if _, err := svc.db.Exec(`UPDATE runners SET owner_agency = 'ag-fin', load = 1 WHERE id = ?`, f.id); err != nil {
		t.Fatal(err)
	}
	bindRunnerToken(t, svc, f.tok, f.id)
	insertRunningRun(t, svc, f.lostTraceID, "load", "", f.id, f.t0.Add(-time.Minute).Format(time.RFC3339))
	prev := reaperNow
	t.Cleanup(func() { reaperNow = prev })
	return f
}

// poll is one poll of process p (none when ""), at t0+after. claim=0 keeps the
// handler from holding the request or claiming anything.
func (f *processFixture) poll(t *testing.T, after time.Duration, p string, started bool) {
	t.Helper()
	at := f.t0.Add(after)
	reaperNow = func() time.Time { return at }
	q := runnerproto.PollParamClaim + "=0"
	if p != "" {
		q += "&" + runnerproto.PollParamProcess + "=" + p
	}
	if started {
		q += "&" + runnerproto.PollParamStarted + "=1"
	}
	if code, _ := pollWith(t, f.svc, f.id, f.tok, q); code != http.StatusNoContent && code != http.StatusOK {
		t.Fatalf("poll at +%s: %d", after, code)
	}
}

func (f *processFixture) runStatus(t *testing.T, traceID string) (status, reason string) {
	t.Helper()
	var r sql.NullString
	if err := f.svc.db.QueryRow(`SELECT status, queued_reason FROM runs WHERE id = ?`, traceID).Scan(&status, &r); err != nil {
		t.Fatal(err)
	}
	return status, r.String
}

func (f *processFixture) load(t *testing.T) int {
	t.Helper()
	var n int
	_ = f.svc.db.QueryRow(`SELECT load FROM runners WHERE id = ?`, f.id).Scan(&n)
	return n
}

// notice returns the runner_identity_shared notice's agency and whether it is
// open; found is false when none was ever written.
func (f *processFixture) notice(t *testing.T) (agency string, open, found bool) {
	t.Helper()
	var resolved sql.NullString
	err := f.svc.db.QueryRow(`SELECT agency_id, resolved_at FROM notices WHERE kind = ? AND subject = ?`,
		notices.KindRunnerIdentityShared, f.id).Scan(&agency, &resolved)
	if err == sql.ErrNoRows {
		return "", false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return agency, !resolved.Valid, true
}

// TestACopysStartDoesNotCloseTheOthersRuns is present defect 70: copy B starts
// on A's identity while A holds a run. Until C6a, B's first poll (started=1)
// closed A's run as runner_lost; A finished the work and the record said it
// failed. Now the close waits, A's next poll shows it is alive, and the run is
// left to A for good. The notice names the runner, under its owner.
func TestACopysStartDoesNotCloseTheOthersRuns(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, 10*time.Second, "bbbb", true)
	if st, _ := f.runStatus(t, f.lostTraceID); st != "running" {
		t.Fatalf("the copy's start closed the other copy's run (status %q)", st)
	}
	if _, _, found := f.notice(t); found {
		t.Error("a start alone raised the notice: it could still be a restart")
	}

	f.poll(t, 15*time.Second, "aaaa", false) // A is alive: an older process after a newer one
	agency, open, found := f.notice(t)
	if !found || !open {
		t.Fatalf("no open runner_identity_shared notice after the older process polled again")
	}
	if agency != "ag-fin" {
		t.Errorf("notice filed under %q, want the runner's owner ag-fin", agency)
	}

	// Long past the hold window, with only B polling: A's run is still A's.
	f.poll(t, 2*time.Minute, "bbbb", false)
	if st, _ := f.runStatus(t, f.lostTraceID); st != "running" {
		t.Errorf("a copy's run was closed after the copy was seen alive (status %q)", st)
	}
	if n := f.load(t); n != 1 {
		t.Errorf("runner load = %d, want 1: a copy's start resets no load", n)
	}
}

// TestACrashsRunsAreClosedOnceItStaysSilent: the case 2.3.2's started=1 is
// for. The process holding a run dies and systemd starts another ten seconds
// later, so the old one WAS seen moments ago, and the start cannot tell this
// from a copy. The close waits for the hold window; the old process never
// polls again, and its run is closed on the first poll after the window. A run
// the new process claimed meanwhile is not touched, and the load drops by one.
func TestACrashsRunsAreClosedOnceItStaysSilent(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, 10*time.Second, "bbbb", true)
	mine := "trace-claimed-by-b"
	insertRunningRun(t, f.svc, mine, "load", "", f.id, f.t0.Add(20*time.Second).Format(time.RFC3339))
	if _, err := f.svc.db.Exec(`UPDATE runners SET load = 2 WHERE id = ?`, f.id); err != nil {
		t.Fatal(err)
	}

	f.poll(t, 30*time.Second, "bbbb", false) // A unseen for 30s: inside the window
	if st, _ := f.runStatus(t, f.lostTraceID); st != "running" {
		t.Fatalf("closed inside the hold window (status %q)", st)
	}

	f.poll(t, 10*time.Second+processHoldWindow+time.Second, "bbbb", false)
	if st, reason := f.runStatus(t, f.lostTraceID); st != "failure" || reason != "runner_lost" {
		t.Errorf("the crashed process's run: %q / %q, want failure / runner_lost", st, reason)
	}
	if st, _ := f.runStatus(t, mine); st != "running" {
		t.Errorf("the new process's own run was closed (status %q)", st)
	}
	if n := f.load(t); n != 1 {
		t.Errorf("runner load = %d, want 1 (two, less the one closed)", n)
	}
	if _, _, found := f.notice(t); found {
		t.Error("a restart raised runner_identity_shared")
	}
}

// TestARestartAfterSilenceClosesAtOnce: when no other process has been seen
// within the hold window, a start closes what the runner shows as running at
// once, as in 2.3.2.
func TestARestartAfterSilenceClosesAtOnce(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, processHoldWindow+time.Second, "bbbb", true)
	if st, reason := f.runStatus(t, f.lostTraceID); st != "failure" || reason != "runner_lost" {
		t.Errorf("the lost run: %q / %q, want failure / runner_lost", st, reason)
	}
	if n := f.load(t); n != 0 {
		t.Errorf("runner load = %d, want 0", n)
	}
	if _, _, found := f.notice(t); found {
		t.Error("a restart raised runner_identity_shared")
	}
}

// TestAnAgentThatSendsNoProcessIsAs232: an agent older than C6a names no
// process. Nothing is detected for it and nothing is declined: its start
// closes the runner's runs at once, and no row is kept.
func TestAnAgentThatSendsNoProcessIsAs232(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "", false)
	f.poll(t, 5*time.Second, "", true)
	f.poll(t, 6*time.Second, "", false)
	if st, reason := f.runStatus(t, f.lostTraceID); st != "failure" || reason != "runner_lost" {
		t.Errorf("the run: %q / %q, want failure / runner_lost (2.3.2's start)", st, reason)
	}
	var n int
	_ = f.svc.db.QueryRow(`SELECT COUNT(*) FROM runner_processes`).Scan(&n)
	if n != 0 {
		t.Errorf("%d process rows for an agent that names none", n)
	}
	if _, _, found := f.notice(t); found {
		t.Error("runner_identity_shared raised for an agent that names no process")
	}
}

// TestTheIdentityNoticeResolvesWhenTheCopyIsGone: once only one process has
// polled for the offline window, the other's value is dropped and the notice
// resolves. A copy that comes back raises it again.
func TestTheIdentityNoticeResolvesWhenTheCopyIsGone(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, 10*time.Second, "bbbb", true)
	f.poll(t, 20*time.Second, "aaaa", false)
	if _, open, _ := f.notice(t); !open {
		t.Fatal("no open notice to resolve")
	}

	f.poll(t, 4*time.Minute, "bbbb", false) // A unseen, but not yet for the window
	if _, open, _ := f.notice(t); !open {
		t.Fatal("resolved before the copy was gone for the offline window")
	}
	f.poll(t, 20*time.Second+f.svc.processWindow()+time.Second, "bbbb", false)
	if _, open, _ := f.notice(t); open {
		t.Error("still open with one process left")
	}

	f.poll(t, 7*time.Minute, "aaaa", false) // the copy is back
	f.poll(t, 7*time.Minute+5*time.Second, "bbbb", false)
	if _, open, _ := f.notice(t); !open {
		t.Error("a copy that came back did not reopen the notice")
	}
}

// TestTheReaperResolvesTheIdentityNotice: a runner that goes quiet has no
// process left on it.
func TestTheReaperResolvesTheIdentityNotice(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, 10*time.Second, "bbbb", true)
	f.poll(t, 20*time.Second, "aaaa", false)
	f.svc.offlineRunner(t.Context(), f.id)
	if _, open, _ := f.notice(t); open {
		t.Error("the notice is open on a runner the reaper found quiet")
	}
}

// TestDeregisteringResolvesTheIdentityNotice: deleting a runner while the
// notice is open takes its processes with it, and nothing would ever poll to
// resolve the notice.
func TestDeregisteringResolvesTheIdentityNotice(t *testing.T) {
	f := newProcessFixture(t)
	f.poll(t, 0, "aaaa", false)
	f.poll(t, 10*time.Second, "bbbb", true)
	f.poll(t, 20*time.Second, "aaaa", false)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/"+f.id, nil)
	req.SetPathValue("id", f.id)
	rec := httptest.NewRecorder()
	f.svc.HandleDeregisterRunner(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deregister: %d %s", rec.Code, rec.Body)
	}
	if _, open, _ := f.notice(t); open {
		t.Error("the notice is open on a runner that no longer exists")
	}
}
