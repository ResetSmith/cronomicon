package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// KB — the cron and reaction halves of the producer conformance: a key-bound
// job whose fire resolves to the ssh executor is refused with the reason
// recorded, and the same job on the runner executor enqueues. The manual/token,
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
		t.Errorf("status = %q, want skipped — the ssh executor cannot deliver the key, so the run must not start", status)
	}
	if reason != runref.ReasonKeyBindingOnSSH {
		t.Errorf("queued_reason = %q, want %q", reason, runref.ReasonKeyBindingOnSSH)
	}

	// Once per episode, like every other skip reason.
	s.fire("git", "unscoped-job", "", "bash", "tax", "Allow", "unscoped-job", "nightly", "")
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='unscoped-job'`).Scan(&n)
	if n != 1 {
		t.Errorf("rows after a second fire = %d, want 1 (de-duped per episode)", n)
	}
}

func TestScheduledFireOfKeyBoundJobOnRunnerEnqueues(t *testing.T) {
	pool, _ := unboundFireDB(t)
	setExecutor(t, pool, "unscoped-job", "runner")
	bindKey(t, pool, "unscoped-job")

	s := New(pool, quietLog(), nil)
	s.fire("git", "unscoped-job", "", "bash", "tax", "Allow", "unscoped-job", "nightly", "")

	var status, executor string
	if err := pool.QueryRow(`SELECT status, COALESCE(executor,'') FROM runs WHERE job_name='unscoped-job'`).
		Scan(&status, &executor); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if status != "queued" || executor != "runner" {
		t.Errorf("status/executor = %q/%q, want queued/runner — the refusal must not reach the runner path", status, executor)
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
	if !strings.Contains(d[0].Detail, "AMADEUS_KEY_deploy_key") || !strings.Contains(d[0].Detail, "runner") {
		t.Errorf("delivery detail %q should name the key and the way out", d[0].Detail)
	}
}
