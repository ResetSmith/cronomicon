package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// SB — the cron and reaction halves of the producer conformance for a scope
// bound to runners (execspec.ResolveExecutor): a job with no executor of its own
// runs on the bound runners, and a job that asks for ssh is refused with the
// reason recorded rather than leaving from the control plane. The manual/token,
// workflow-step and file-arrival halves live next to their producers
// (api/scope_executor_run_test.go, workflow/scope_executor_step_test.go,
// runner/filewatch_scope_executor_test.go).
//
// This is the test the runner-tag pin never had: Phase 0 showed a pinned shell
// job on a schedule being queued for ssh with its pin recorded and ignored.

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

func TestScheduledShellJobOnABoundScopeRunsOnItsRunners(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "deploy", 1) // bash, no executor of its own
	bindScopeForTest(t, pool, "dmz-web")

	s := New(pool, quietLog(), nil)
	s.fire("git", "deploy", "uid-deploy", "bash", "dmz-web", "Allow", "", "nightly", "")

	var status, executor string
	if err := pool.QueryRow(`SELECT status, executor FROM runs WHERE job_name='deploy'`).Scan(&status, &executor); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if status != "queued" || executor != "runner" {
		t.Errorf("status/executor = %q/%q, want queued/runner — a shell job on a bound scope must not default to ssh", status, executor)
	}

	// The same job on a scope nobody bound keeps the run type's default.
	s.fire("git", "deploy", "uid-deploy", "bash", "open", "Allow", "", "nightly", "")
	if err := pool.QueryRow(`SELECT executor FROM runs WHERE job_name='deploy' AND scope='open'`).Scan(&executor); err != nil {
		t.Fatalf("no run row on the unbound scope: %v", err)
	}
	if executor != "ssh" {
		t.Errorf("executor on an unbound scope = %q, want ssh (unchanged)", executor)
	}
}

func TestScheduledFireAskingForSSHOnABoundScopeIsRecordedNotEnqueued(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "deploy", 1)
	setExecutor(t, pool, "deploy", "ssh")
	bindScopeForTest(t, pool, "dmz-web")

	s := New(pool, quietLog(), nil)
	s.fire("git", "deploy", "uid-deploy", "bash", "dmz-web", "Allow", "", "nightly", "")

	var status, reason string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name='deploy'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("no run row recorded — the fire vanished silently: %v", err)
	}
	if status != "skipped" {
		t.Errorf("status = %q, want skipped — ssh on a bound scope would run from the server", status)
	}
	if reason != execspec.ReasonScopeRequiresRunner {
		t.Errorf("queued_reason = %q, want %q", reason, execspec.ReasonScopeRequiresRunner)
	}

	// Once per episode, like every other skip reason.
	s.fire("git", "deploy", "uid-deploy", "bash", "dmz-web", "Allow", "", "nightly", "")
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows after a second fire = %d, want 1 (de-duped per episode)", n)
	}

	// ...but the episode ends at midnight. This refusal stands for as long as
	// the job says ssh, and missed-run detection accepts a skip row only from
	// the same day as the fire it explains — so a single row for the whole
	// episode would be reported as a missed run, with an alert, every day after
	// the first. Age the row by two days and the next fire records again.
	if _, err := pool.Exec(`UPDATE runs SET created_at = ?, started_at = ?, completed_at = ? WHERE job_name='deploy'`,
		twoDaysAgo(), twoDaysAgo(), twoDaysAgo()); err != nil {
		t.Fatal(err)
	}
	s.fire("git", "deploy", "uid-deploy", "bash", "dmz-web", "Allow", "", "nightly", "")
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy' AND status='skipped'`).Scan(&n)
	if n != 2 {
		t.Errorf("skip rows after a fire on a later day = %d, want 2 (one per day)", n)
	}
}

func twoDaysAgo() string { return time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339) }

// TestScopeRefusalSkipDoesNotSwallowAnotherJobsForbidSkip: the refusal's skip
// row must carry no concurrency key. Two jobs may share a custom key; if this
// row carried it, it would become "the latest run for that key" and the other
// job's Forbid conflict — which de-dupes on exactly that — would record
// nothing. The pause and cap paths drop the key for the same reason.
func TestScopeRefusalSkipDoesNotSwallowAnotherJobsForbidSkip(t *testing.T) {
	pool := mustPool(t)
	seedJobRow(t, pool, "refused", 1)
	setExecutor(t, pool, "refused", "ssh")
	bindScopeForTest(t, pool, "dmz-web")

	s := New(pool, quietLog(), nil)
	s.fire("git", "refused", "uid-refused", "bash", "dmz-web", "Forbid", "shared-key", "nightly", "")

	var key sql.NullString
	if err := pool.QueryRow(`SELECT concurrency_key FROM runs WHERE job_name='refused'`).Scan(&key); err != nil {
		t.Fatalf("no skip row: %v", err)
	}
	if key.Valid && key.String != "" {
		t.Errorf("the refusal's skip row carries concurrency_key %q, want none", key.String)
	}
}

func TestReactionAskingForSSHOnABoundScopeRecordsADeliveryErrorNotARun(t *testing.T) {
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

	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='downstream'`).Scan(&runs)
	if runs != 0 {
		t.Errorf("run rows for downstream = %d, want 0 — a reaction refusal must not invent a run row", runs)
	}
	d := deliveries(t, pool, "downstream")
	if len(d) != 1 || d[0].Result != "error" {
		t.Fatalf("deliveries = %+v, want one 'error'", d)
	}
	if !strings.Contains(d[0].Detail, "dmz-web") || !strings.Contains(d[0].Detail, "bound to runners") {
		t.Errorf("delivery detail %q should name the scope and why", d[0].Detail)
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
