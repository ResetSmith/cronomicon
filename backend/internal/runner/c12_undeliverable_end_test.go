package runner

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 2.5.0 C12, present defect 92: a run's end arrives however its log ends. Until
// then a final chunk past the log cap was refused with 413 like any other (the
// run stayed `running` for good, and since C11 its agent re-sent it every
// minute), and a deregister left its runner's running runs to nobody.

func logCapFixture(t *testing.T, traceID string) (*Service, func(body string, partial bool) int) {
	t.Helper()
	svc := newTestService(t)
	svc.cfg.MaxRunLogBytes = 200
	as := authSvc(t, svc)
	runnerID, tok := "runner-cap", "crn_run_cap"
	insertRunner(t, svc, runnerID, "cap", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES (?, 'j', 'bash', 'prod', 'running', ?, 'test', 'manual', 'runner', ?, ?)`,
		traceID, runnerID, ts, ts); err != nil {
		t.Fatal(err)
	}
	post := func(body string, partial bool) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/"+traceID+"/log", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		if partial {
			req.Header.Set("X-Log-Partial", "1")
		}
		req.SetPathValue("traceId", traceID)
		rec := httptest.NewRecorder()
		as.RequireRunner(http.HandlerFunc(svc.HandleIngestLog)).ServeHTTP(rec, req)
		return rec.Code
	}
	return svc, post
}

func runEnd(t *testing.T, svc *Service, traceID string) (status string, exit sql.NullInt64, reason sql.NullString) {
	t.Helper()
	if err := svc.db.QueryRow(`SELECT status, exit_code, queued_reason FROM runs WHERE id = ?`, traceID).
		Scan(&status, &exit, &reason); err != nil {
		t.Fatal(err)
	}
	return
}

const envelopeExit3 = `{"exitCode":3,"durationMs":5,"endedAt":"2026-10-19T00:00:00Z"}`

// A final chunk that arrives once the run's log is at its cap ends the run from
// its envelope; a partial one is still refused.
func TestC12_AFinalChunkPastTheLogCapEndsTheRun(t *testing.T) {
	svc, post := logCapFixture(t, "run-cap-after")
	if c := post(strings.Repeat("x", 500)+"\n", true); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("partial chunk over the cap: %d, want 413", c)
	}
	if c := post("more\n", true); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("partial chunk at the cap: %d, want 413", c)
	}
	if c := post("discarded\n"+envelopeExit3+"\n", false); c != http.StatusNoContent {
		t.Fatalf("final chunk at the cap: %d, want 204", c)
	}
	if st, exit, _ := runEnd(t, svc, "run-cap-after"); st != "failure" || !exit.Valid || exit.Int64 != 3 {
		t.Errorf("run after its final chunk: %s exit %v, want failure exit 3", st, exit)
	}
}

// A final chunk that itself crosses the cap ends the run from its envelope, its
// text past the cap discarded.
func TestC12_AFinalChunkThatCrossesTheLogCapEndsTheRun(t *testing.T) {
	svc, post := logCapFixture(t, "run-cap-cross")
	body := "first\n" + strings.Repeat("y", 500) + "\nafter the cap\n" + envelopeExit3 + "\n"
	if c := post(body, false); c != http.StatusNoContent {
		t.Fatalf("final chunk crossing the cap: %d, want 204", c)
	}
	if st, exit, _ := runEnd(t, svc, "run-cap-cross"); st != "failure" || !exit.Valid || exit.Int64 != 3 {
		t.Errorf("run after its final chunk: %s exit %v, want failure exit 3", st, exit)
	}
}

// The line that crosses the cap may be the envelope itself (C12.4's
// re-review): it ended the run as log_stream_lost, the outcome lost.
func TestC12_AnEnvelopeThatCrossesTheLogCapStillEndsTheRun(t *testing.T) {
	svc, post := logCapFixture(t, "run-cap-envelope")
	body := strings.Repeat("z", 150) + "\n" + envelopeExit3 + "\n" // the envelope is the line past 200
	if c := post(body, false); c != http.StatusNoContent {
		t.Fatalf("final chunk whose envelope crosses the cap: %d, want 204", c)
	}
	if st, exit, reason := runEnd(t, svc, "run-cap-envelope"); st != "failure" || !exit.Valid || exit.Int64 != 3 {
		t.Errorf("run: %s exit %v (%s), want failure exit 3 from its envelope", st, exit, reason.String)
	}
}

// A read of the run that FAILS is not "no such run" (C12.4's re-review): since
// C12 an agent takes the server's own 404 as final, so a busy or closing
// database must answer what it sends again.
func TestC12_AFailedReadOfTheRunIsNotA404(t *testing.T) {
	svc := newTestService(t)
	if err := svc.db.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runs/run-closed-db/log", strings.NewReader(envelopeExit3+"\n"))
	req.SetPathValue("traceId", "run-closed-db")
	rec := httptest.NewRecorder()
	svc.HandleIngestLog(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("a failed read of the run = %d %s, want 503", rec.Code, rec.Body)
	}
}

// Question 120: the ingest route ignored the errors of two reads. A failed
// read of runs.injects_secret read false, so a secret-bearing run's chunk took
// the lenient redaction path; a failed read of log_raw_offset read 0, which
// opens the log with O_TRUNC. Each failure refuses with 503 now (the agent
// sends the chunk again). The column is renamed from under the one read.
func TestC12_AFailedReadOnTheIngestPathRefuses(t *testing.T) {
	for _, column := range []string{"injects_secret", "log_raw_offset"} {
		t.Run(column, func(t *testing.T) {
			svc, post := logCapFixture(t, "run-noread-"+strings.ReplaceAll(column, "_", "-"))
			if _, err := svc.db.Exec(`ALTER TABLE runs RENAME COLUMN ` + column + ` TO ` + column + `_gone`); err != nil {
				t.Fatal(err)
			}
			if c := post("a line\n", true); c != http.StatusServiceUnavailable {
				t.Errorf("a chunk whose run's %s cannot be read = %d, want 503", column, c)
			}
		})
	}
}

// Deregistering a runner ends its running runs as runner_lost: the delete sets
// their runner_id to NULL, and nothing found them after that.
func TestC12_DeregisteringARunnerEndsItsRunningRuns(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r-dereg", "dereg", "online", []string{"bash"})
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES ('run-dereg', 'j', 'bash', 'prod', 'running', 'r-dereg', 'test', 'manual', 'runner', ?, ?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r-dereg", nil)
	req.SetPathValue("id", "r-dereg")
	rec := httptest.NewRecorder()
	svc.HandleDeregisterRunner(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("deregister = %d, want 204 (body %s)", rec.Code, rec.Body)
	}
	if st, _, reason := runEnd(t, svc, "run-dereg"); st != "failure" || reason.String != "runner_lost" {
		t.Errorf("a running run of a deregistered runner: %s (%s), want failure (runner_lost)", st, reason.String)
	}
	// Through the reaper's own path (C12's code review): the run-end activity
	// is written, as for every runner_lost.
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM activity WHERE kind = 'run-end' AND trace_id = 'run-dereg'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("run-end activity for the deregistered runner's run: %d rows, want 1", n)
	}
}

// A close that cannot be written fails the deregistration (C12.5's review):
// C12.5 closed the runs after the commit, so a close that failed then left a
// run running with its runner gone and its runner_id NULL, which nothing ever
// finds again. The close is in the delete's transaction: both or neither.
func TestC12_AFailedCloseFailsTheDeregistration(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r-x", "x", "online", []string{"bash"})
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES ('run-x', 'j', 'bash', 'prod', 'running', 'r-x', 'test', 'manual', 'runner', ?, ?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	// Stands in for any failure of the close's write.
	if _, err := svc.db.Exec(`CREATE TRIGGER c12_refuse_lost BEFORE UPDATE OF status ON runs
		WHEN NEW.queued_reason = 'runner_lost' BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r-x", nil)
	req.SetPathValue("id", "r-x")
	rec := httptest.NewRecorder()
	svc.HandleDeregisterRunner(rec, req)
	var status string
	var rid sql.NullString
	if err := svc.db.QueryRow(`SELECT status, runner_id FROM runs WHERE id = 'run-x'`).Scan(&status, &rid); err != nil {
		t.Fatal(err)
	}
	var runners int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runners WHERE id = 'r-x'`).Scan(&runners)
	if rec.Code/100 == 2 || runners != 1 || status != "running" || rid.String != "r-x" {
		t.Errorf("deregister %d: run %s, runner_id %v, runner rows %d; want the deregistration refused and the run still its runner's",
			rec.Code, status, rid, runners)
	}
}

// A deregistration that fails closes nothing (C12's code review): its runs
// were ended before its transaction, so a failed delete left the agent running
// runs already recorded as lost, their alerts sent.
func TestC12_AFailedDeregistrationEndsNoRun(t *testing.T) {
	svc := newTestService(t)
	insertRunner(t, svc, "r-keep", "keep", "online", []string{"bash"})
	ts := now()
	if _, err := svc.db.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, triggered_by, trigger_kind, executor, created_at, started_at)
		VALUES ('run-keep', 'j', 'bash', 'prod', 'running', 'r-keep', 'test', 'manual', 'runner', ?, ?)`, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`CREATE TRIGGER c12_refuse_runner_delete BEFORE DELETE ON runners
		BEGIN SELECT RAISE(ABORT, 'refused by the test'); END`); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/r-keep", nil)
	req.SetPathValue("id", "r-keep")
	rec := httptest.NewRecorder()
	svc.HandleDeregisterRunner(rec, req)
	if rec.Code/100 == 2 {
		t.Fatalf("deregister with its delete refused = %d, want a failure", rec.Code)
	}
	if st, _, reason := runEnd(t, svc, "run-keep"); st != "running" {
		t.Errorf("a run of a runner whose deregistration failed: %s (%s), want still running", st, reason.String)
	}
}
