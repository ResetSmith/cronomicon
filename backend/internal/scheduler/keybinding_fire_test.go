package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// LR-47 (the KB refusal, narrowed) — the cron and reaction halves of the
// producer conformance: a key-bound shell job with no agent to deliver the key
// is refused with the reason recorded, and the same job enqueues once an agent
// serves its scope. The job's own `executor` is not read any more (LR-42); the
// fixtures still set it, to show it changes nothing. The manual/token,
// workflow-step and file-arrival halves live next to their producers
// (api/keybinding_run_test.go, workflow/keybinding_step_test.go,
// runner/filewatch_keybinding_test.go) — the FX-C shape.

func bindKey(t *testing.T, pool *sql.DB, job string) {
	t.Helper()
	// The real writer, so the row carries the owner uid the match keys on (R2).
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "git", Name: job},
		[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "t"); err != nil {
		t.Fatalf("bind key: %v", err)
	}
}

func setExecutor(t *testing.T, pool *sql.DB, job, executor string) {
	t.Helper()
	if _, err := pool.Exec(`UPDATE jobs SET executor = ? WHERE name = ? AND source = 'git'`, executor, job); err != nil {
		t.Fatalf("set executor: %v", err)
	}
}

func TestScheduledFireOfKeyBoundJobOnSSHIsRecordedNotEnqueued(t *testing.T) {
	pool, _ := unboundFireDB(t)
	setExecutor(t, pool, "unscoped-job", "ssh")
	bindKey(t, pool, "unscoped-job")

	// Scoped, so the AF unbound probe is not what refuses it.
	s := New(pool, quietLog(), nil)
	s.fire("git", "unscoped-job", "", "bash", "tax", "Allow", "unscoped-job", "nightly", "")

	var status, reason string
	if err := pool.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(queued_reason,'') FROM runs WHERE job_name='unscoped-job'`).
		Scan(&status, &reason); err != nil {
		t.Fatalf("no run row recorded — the fire vanished silently: %v", err)
	}
	if status != "skipped" {
		t.Errorf("status = %q, want skipped — no agent can deliver the key, so the run must not be queued", status)
	}
	if reason != runref.ReasonKeyBindingNeedsAgent {
		t.Errorf("queued_reason = %q, want %q", reason, runref.ReasonKeyBindingNeedsAgent)
	}

	// Once per episode, like every other skip reason.
	s.fire("git", "unscoped-job", "", "bash", "tax", "Allow", "unscoped-job", "nightly", "")
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='unscoped-job'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows after a second fire = %d, want 1 (de-duped per episode)", n)
	}
}

// agentServing registers an agent of Global's and makes sure the scope exists
// (a new scope is Global's), so the agent serves it.
func agentServing(t *testing.T, pool *sql.DB, scope string) {
	t.Helper()
	const now = "2026-01-01T00:00:00Z"
	for _, q := range []string{
		`INSERT OR IGNORE INTO scopes (id, name, source, created_at) VALUES ('sc-` + scope + `', '` + scope + `', 'cronomicon', '` + now + `')`,
		`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('agent-1', 'agent-1', 'offline', '` + now + `', '` + now + `')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

func TestScheduledFireOfKeyBoundJobWithAnAgentEnqueues(t *testing.T) {
	pool, _ := unboundFireDB(t)
	// The job says ssh, which until 2.3.0 was what refused it. An agent serves
	// its scope — offline, even: that is an ordinary wait — so it enqueues.
	setExecutor(t, pool, "unscoped-job", "ssh")
	bindKey(t, pool, "unscoped-job")
	agentServing(t, pool, "tax")

	s := New(pool, quietLog(), nil)
	s.fire("git", "unscoped-job", "", "bash", "tax", "Allow", "unscoped-job", "nightly", "")

	var status, executor string
	if err := pool.QueryRow(`SELECT status, COALESCE(executor,'') FROM runs WHERE job_name='unscoped-job'`).
		Scan(&status, &executor); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if status != "queued" || executor != "runner" {
		t.Errorf("status/executor = %q/%q, want queued/runner — an agent can deliver the key, so the run waits for it", status, executor)
	}
}

func TestReactionToKeyBoundJobOnSSHRecordsADeliveryErrorNotARun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	seedJobRow(t, pool, "downstream", 1)
	// Scoped, so the AF unbound probe (unscoped runs only) is not what refuses it.
	if _, err := pool.Exec(`UPDATE jobs SET scope='tax' WHERE name='downstream'`); err != nil {
		t.Fatal(err)
	}
	setExecutor(t, pool, "downstream", "ssh")
	bindKey(t, pool, "downstream")
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)
	primeCursor(t, s)

	s.ScanReactions(ctxb())

	if n := countPending(t, pool, "downstream"); n != 0 {
		t.Errorf("pending runs for downstream = %d, want 0", n)
	}
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='downstream'`).Scan(&runs)
	if runs != 0 {
		t.Errorf("run rows for downstream = %d, want 0 — a reaction refusal must not invent a run row", runs)
	}
	d := deliveries(t, pool, "downstream")
	if len(d) != 1 || d[0].Result != "error" {
		t.Fatalf("deliveries = %+v, want one 'error'", d)
	}
	if !strings.Contains(d[0].Detail, "CRONOMICON_KEY_deploy_key") || !strings.Contains(d[0].Detail, "runner") {
		t.Errorf("delivery detail %q should name the key and the way out", d[0].Detail)
	}
}

// TestKeyBindingSkipIsAStandingRefusal pins the two properties of the refusal's
// skip row that its first version lacked (see Scheduler.standingRefusal). It
// carries no concurrency key, so it cannot become "the latest run" for a key
// another job shares and swallow that job's Forbid skip. And its episode ends at
// midnight: a key-bound job on ssh is refused for as long as it stays that way,
// missed-run detection accepts a skip only from the same day as the fire, and
// one row for the whole episode was reported as a missed run, with an alert,
// every day after the first.
func TestKeyBindingSkipIsAStandingRefusal(t *testing.T) {
	pool, _ := unboundFireDB(t)
	setExecutor(t, pool, "unscoped-job", "ssh")
	bindKey(t, pool, "unscoped-job")

	s := New(pool, quietLog(), nil)
	s.fire("git", "unscoped-job", "", "bash", "tax", "Forbid", "shared-key", "nightly", "")

	var key sql.NullString
	if err := pool.QueryRow(`SELECT concurrency_key FROM runs WHERE job_name='unscoped-job'`).Scan(&key); err != nil {
		t.Fatalf("no skip row: %v", err)
	}
	if key.Valid && key.String != "" {
		t.Errorf("the refusal's skip row carries concurrency_key %q, want none", key.String)
	}

	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	if _, err := pool.Exec(`UPDATE runs SET created_at = ?, started_at = ?, completed_at = ? WHERE job_name='unscoped-job'`,
		old, old, old); err != nil {
		t.Fatal(err)
	}
	s.fire("git", "unscoped-job", "", "bash", "tax", "Forbid", "shared-key", "nightly", "")
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='unscoped-job' AND status='skipped'`).Scan(&n)
	if n != 2 {
		t.Errorf("skip rows after a fire on a later day = %d, want 2 (one per day)", n)
	}
}
