package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// SB Phase 0 found two defects in how a scheduled fire identifies its job, both
// invisible while job names were unique and both real once R2 let two agencies
// hold same-named jobs. The tests here were written to pin each defect, and now
// guard its fix: the run carries the job that was fired (9f7dfda), and the
// executor is that job's own (execspec.ResolveExecutor).
//
// A third characterization test lived here until the runner-tag pin was retired:
// it showed a pinned shell job on a schedule being queued for ssh with its pin
// recorded and unenforced. Its replacement — a scope bound to runners IS
// honoured on a scheduled fire — is scope_executor_test.go.

// seedTwins seeds two cronomicon jobs that share a name, which R2 allows across
// agencies, differing in everything a run snapshots from its job. Each is fired
// on its own scope so the two runs can be told apart without trusting job_uid,
// which is one of the columns under test.
func seedTwins(t *testing.T, pool *sql.DB) {
	t.Helper()
	for _, j := range [][4]string{
		{"uid-twin-a", "scripts/a.sh", "hash-a", "runner"},
		{"uid-twin-b", "scripts/b.sh", "hash-b", "ssh"},
	} {
		if _, err := pool.ExecContext(context.Background(), `
			INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled,
			                  script_ref, content_hash, executor, synced_at)
			VALUES (?, 'twin', 'cronomicon', 'bash', 'Allow', 1, ?, ?, ?, 't')`,
			j[0], j[1], j[2], j[3]); err != nil {
			t.Fatalf("seed twin %s: %v", j[0], err)
		}
	}
}

// fireTwins fires each twin by its own uid on its own scope and returns the
// requested column of the resulting run, keyed by scope.
func fireTwins(t *testing.T, pool *sql.DB, column string) map[string]string {
	t.Helper()
	s := New(pool, quietLog(), nil)
	s.fire("cronomicon", "twin", "uid-twin-a", "bash", "scope-a", "Allow", "", "default", "")
	s.fire("cronomicon", "twin", "uid-twin-b", "bash", "scope-b", "Allow", "", "default", "")

	got := map[string]string{}
	rows, err := pool.QueryContext(context.Background(),
		`SELECT scope, COALESCE(`+column+`, '') FROM runs WHERE job_name = 'twin'`)
	if err != nil {
		t.Fatalf("fetch runs: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, v string
		if err := rows.Scan(&scope, &v); err != nil {
			t.Fatalf("scan run: %v", err)
		}
		got[scope] = v
	}
	if len(got) != 2 {
		t.Fatalf("got runs on %d scopes, want 2: %v", len(got), got)
	}
	return got
}

// TestScheduledFireCarriesItsOwnJobIdentity guards the fix for a defect Phase 0
// found: fire() passed the job's uid on every skip-record path but omitted it
// from the EnqueueParams of the run that executes, so resolveEnqueueUID fell
// back to the (name, source) pair and a scheduled run of one same-named job was
// recorded as its sibling — the other job's identity, script reference and
// content hash, on this job's scope. Everything the run-row writer snapshots by
// uid must follow the job that was fired.
func TestScheduledFireCarriesItsOwnJobIdentity(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool)

	for column, want := range map[string]map[string]string{
		"job_uid":      {"scope-a": "uid-twin-a", "scope-b": "uid-twin-b"},
		"script_ref":   {"scope-a": "scripts/a.sh", "scope-b": "scripts/b.sh"},
		"content_hash": {"scope-a": "hash-a", "scope-b": "hash-b"},
	} {
		got := fireTwins(t, pool, column)
		for scope, w := range want {
			if got[scope] != w {
				t.Errorf("%s on %s = %q, want %q (the run must carry its own job, not its sibling)",
					column, scope, got[scope], w)
			}
		}
		if _, err := pool.Exec(`DELETE FROM runs`); err != nil {
			t.Fatalf("reset runs: %v", err)
		}
	}
}

// TestQueuedFireCarriesItsOwnJobIdentity is the same guard for a fire parked
// behind a Queue-policy gate: the pending row's owner_uid, and the frozen params
// promotion later enqueues from, must both name the job that was fired.
func TestQueuedFireCarriesItsOwnJobIdentity(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool)

	queued, err := TryQueue(context.Background(), pool, EnqueueParams{
		JobName: "twin", JobSource: "cronomicon", JobUID: "uid-twin-b",
		RunType: "bash", Scope: "scope-b", ConcurrencyKey: "uid-twin-b",
	})
	if err != nil || !queued {
		t.Fatalf("TryQueue = %v, %v; want queued", queued, err)
	}
	var owner, params string
	if err := pool.QueryRow(`SELECT COALESCE(owner_uid,''), params_json FROM pending_runs`).
		Scan(&owner, &params); err != nil {
		t.Fatalf("fetch pending row: %v", err)
	}
	if owner != "uid-twin-b" {
		t.Errorf("owner_uid = %q, want uid-twin-b", owner)
	}
	var frozen EnqueueParams
	if err := json.Unmarshal([]byte(params), &frozen); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if frozen.JobUID != "uid-twin-b" {
		t.Errorf("frozen JobUID = %q, want uid-twin-b", frozen.JobUID)
	}
}

// Phase 0 of SB found that the executor was read by (name, source), so two
// same-named jobs declaring different executors got one answer between them.
// Since 2.3.0 there is no answer to get: a job's `executor` is not read, and
// both twins are written for the runner executor (LR-42).
func TestScheduledFireIgnoresTheJobsExecutor(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool) // one twin says runner, the other says ssh

	got := fireTwins(t, pool, "executor")
	if got["scope-a"] != "runner" || got["scope-b"] != "runner" {
		t.Errorf("executors = %v, want runner for both", got)
	}
}
