package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// LR-42 — the cron and reaction halves of the producer conformance for the one
// executor: every run is written for the runner executor, on a bound scope and
// on an unbound one, whatever the job's own `executor` says. Where it runs is
// decided at claim time. (Until 2.3.0 a job that asked for ssh on a bound scope
// was refused, `scope_requires_runner`; there is nothing left to ask for.) The
// manual/token, workflow-step and file-arrival halves live next to their
// producers (api/scope_executor_run_test.go, workflow/scope_executor_step_test.go,
// runner/filewatch_scope_executor_test.go).
//
// The standing-refusal rules the scope refusal introduced — one skip row a day,
// and no concurrency key on it — are still in force for the refusal that
// remains, a key-bound job with no agent (LR-47), and are tested on it below.

// bindScopeForTest gives a scope name a row and a runner binding. The runner
// needs no row of its own — a binding names an id.
func bindScopeForTest(t *testing.T, pool *sql.DB, scope string) {
	t.Helper()
	if _, err := pool.Exec(
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-'||?, ?, 'cronomicon', 't')`, scope, scope); err != nil {
		t.Fatalf("seed scope: %v", err)
	}
	if _, err := pool.Exec(
		`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-'||?, 'r-dmz', 'runner-dmz-01', 't')`, scope); err != nil {
		t.Fatalf("bind scope: %v", err)
	}
}

func TestScheduledShellJobIsWrittenForTheRunnerExecutor(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "deploy", 1) // bash
	bindScopeForTest(t, pool, "dmz-web")
	s := New(pool, quietLog(), nil)

	for _, tc := range []struct{ jobExecutor, scope string }{
		{"", "dmz-web"},    // a bound scope
		{"", "open"},       // a scope nobody bound: no longer the SSH executor's
		{"ssh", "dmz-web"}, // the job says ssh, which used to be refused here
		{"ssh", "open"},
		{"runner", "open"},
	} {
		if _, err := pool.Exec(`DELETE FROM runs`); err != nil {
			t.Fatal(err)
		}
		if tc.jobExecutor == "" {
			if _, err := pool.Exec(`UPDATE jobs SET executor = NULL WHERE name = 'deploy'`); err != nil {
				t.Fatal(err)
			}
		} else {
			setExecutor(t, pool, "deploy", tc.jobExecutor)
		}
		s.fire("git", "deploy", "uid-deploy", "bash", tc.scope, "Allow", "", "nightly", "")
		var status, executor string
		if err := pool.QueryRow(`SELECT status, executor FROM runs WHERE job_name='deploy'`).Scan(&status, &executor); err != nil {
			t.Fatalf("job executor %q on %s: no run row: %v", tc.jobExecutor, tc.scope, err)
		}
		if status != "queued" || executor != "runner" {
			t.Errorf("job executor %q on %s: status/executor = %q/%q, want queued/runner", tc.jobExecutor, tc.scope, status, executor)
		}
	}
}

// The key-bound refusal is a STANDING one: it holds for as long as no agent
// serves the job's scope. One skip row for the whole episode would be reported
// as a missed run, with an alert, every day after the first — missed-run
// detection accepts a skip row only from the same day as the fire it explains.
// So the episode ends at midnight.
func TestStandingRefusalRecordsOncePerDay(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "deploy", 1)
	bindKey(t, pool, "deploy")
	s := New(pool, quietLog(), nil)
	fire := func() { s.fire("git", "deploy", "uid-deploy", "bash", "dmz-web", "Allow", "", "nightly", "") }

	fire()
	var status, reason string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name='deploy'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("no run row recorded — the fire vanished silently: %v", err)
	}
	if status != "skipped" || reason != runref.ReasonKeyBindingNeedsAgent {
		t.Fatalf("status/reason = %q/%q, want skipped/%q", status, reason, runref.ReasonKeyBindingNeedsAgent)
	}
	// Once per episode, like every other skip reason.
	fire()
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows after a second fire = %d, want 1 (de-duped per episode)", n)
	}
	// Age the row by two days and the next fire records again.
	if _, err := pool.Exec(`UPDATE runs SET created_at = ?, started_at = ?, completed_at = ? WHERE job_name='deploy'`,
		twoDaysAgo(), twoDaysAgo(), twoDaysAgo()); err != nil {
		t.Fatal(err)
	}
	fire()
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy' AND status='skipped'`).Scan(&n)
	if n != 2 {
		t.Errorf("skip rows after a fire on a later day = %d, want 2 (one per day)", n)
	}
}

func twoDaysAgo() string { return time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339) }

// TestStandingRefusalSkipDoesNotSwallowAnotherJobsForbidSkip: the refusal's skip
// row must carry no concurrency key. Two jobs may share a custom key; if this
// row carried it, it would become "the latest run for that key" and the other
// job's Forbid conflict — which de-dupes on exactly that — would record
// nothing. The pause and cap paths drop the key for the same reason.
func TestStandingRefusalSkipDoesNotSwallowAnotherJobsForbidSkip(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "refused", 1)
	bindKey(t, pool, "refused")

	s := New(pool, quietLog(), nil)
	s.fire("git", "refused", "uid-refused", "bash", "dmz-web", "Forbid", "shared-key", "nightly", "")

	var key sql.NullString
	var status string
	if err := pool.QueryRow(`SELECT status, concurrency_key FROM runs WHERE job_name='refused'`).Scan(&status, &key); err != nil {
		t.Fatalf("no skip row: %v", err)
	}
	if status != "skipped" {
		t.Fatalf("status = %q, want skipped (fixture: a key-bound job with no agent)", status)
	}
	if key.Valid && key.String != "" {
		t.Errorf("the refusal's skip row carries concurrency_key %q, want none", key.String)
	}
}

// A reacting job that says ssh on a bound scope is no longer refused: its
// executor is not read, and the run is parked for the runner executor.
func TestReactionIgnoresTheJobsExecutor(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "downstream", 1)
	if _, err := pool.Exec(`UPDATE jobs SET scope='dmz-web' WHERE name='downstream'`); err != nil {
		t.Fatal(err)
	}
	setExecutor(t, pool, "downstream", "ssh")
	bindScopeForTest(t, pool, "dmz-web")
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)
	primeCursor(t, s)

	s.ScanReactions(ctxb())

	d := deliveries(t, pool, "downstream")
	if len(d) != 1 || d[0].Result == "error" {
		t.Fatalf("deliveries = %+v, want one that is not an error", d)
	}
	var raw string
	if err := pool.QueryRow(`SELECT params_json FROM pending_runs WHERE name='downstream'`).Scan(&raw); err != nil {
		t.Fatalf("no parked run for the reacting job: %v", err)
	}
	var frozen EnqueueParams
	if err := json.Unmarshal([]byte(raw), &frozen); err != nil {
		t.Fatalf("decode parked params: %v", err)
	}
	if frozen.Executor != "runner" {
		t.Errorf("parked executor = %q, want runner", frozen.Executor)
	}
}

// The one writer of runs is where an old value stops: a run parked before
// 2.3.0 carries Executor "ssh" in its frozen snapshot, and promotion replays
// the snapshot. Written as it says, the row would have nothing left to claim
// it.
func TestTheRunWriterNeverWritesTheSSHExecutor(t *testing.T) {
	for _, in := range []string{"ssh", "", "runner", "anything"} {
		if got := (EnqueueParams{Executor: in}).executorOrDefault(); got != "runner" {
			t.Errorf("executor %q is written as %q, want runner", in, got)
		}
	}
}

// A reacting job with NO executor of its own on a bound scope is not refused:
// it is sent to the runners.
func TestReactionOnABoundScopeRunsOnItsRunners(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "downstream", 1)
	if _, err := pool.Exec(`UPDATE jobs SET scope='dmz-web' WHERE name='downstream'`); err != nil {
		t.Fatal(err)
	}
	bindScopeForTest(t, pool, "dmz-web")
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)
	primeCursor(t, s)

	s.ScanReactions(ctxb())

	// A reaction parks its run for promotion; the executor is frozen on the
	// parked params, which is what the promoted run is enqueued from.
	var raw string
	if err := pool.QueryRow(`SELECT params_json FROM pending_runs WHERE name='downstream'`).Scan(&raw); err != nil {
		t.Fatalf("no parked run for the reacting job: %v (deliveries: %+v)", err, deliveries(t, pool, "downstream"))
	}
	var frozen EnqueueParams
	if err := json.Unmarshal([]byte(raw), &frozen); err != nil {
		t.Fatalf("decode parked params: %v", err)
	}
	if frozen.Executor != "runner" {
		t.Errorf("parked executor = %q, want runner", frozen.Executor)
	}
}

// A job's become password makes its runs REQUIRE the become-file token — for an
// ansible job, which is the only kind that uses it. Sync stores the field on a
// job of any run type ("stored but ignored"), and a shell run that carried the
// token would wait for ever now that requirements are read for every run: the
// local runner advertises none.
func TestBecomeFileIsRequiredOfAnsibleRunsOnly(t *testing.T) {
	pool := mustPool(t)
	for _, tc := range []struct{ job, runType, want string }{
		{"shell-with-become", "bash", ""},
		{"play-with-become", "ansible", `["become-file"]`},
	} {
		seedJobRow(t, pool, tc.job, 1)
		if _, err := pool.Exec(`UPDATE jobs SET run_type = ?, become_password_secret = 'SUDO_PASS' WHERE name = ?`, tc.runType, tc.job); err != nil {
			t.Fatal(err)
		}
		s := New(pool, quietLog(), nil)
		s.fire("git", tc.job, "uid-"+tc.job, tc.runType, "", "Allow", "", "nightly", "")
		var requires sql.NullString
		if err := pool.QueryRow(`SELECT requires_json FROM runs WHERE job_name = ?`, tc.job).Scan(&requires); err != nil {
			t.Fatalf("%s: no run row: %v", tc.job, err)
		}
		if requires.String != tc.want {
			t.Errorf("%s (%s): requires_json = %q, want %q", tc.job, tc.runType, requires.String, tc.want)
		}
	}
}
