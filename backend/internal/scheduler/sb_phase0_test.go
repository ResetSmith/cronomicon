package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// SB Phase 0 — characterization tests for defects the scope-bound-runners plan
// (.aidata/20261005-runners-update.md) found by reading and needed proven before
// it builds on them.
//
// The pin and executor tests PASS against their defect on purpose. They pin
// what the code does today so the change that fixes each one has a test to turn
// around rather than a claim to take on trust: Phase B (one executor resolver,
// keyed by identity and scope) inverts the executor test and replaces the pin
// test with its bound-scope equivalent; Phase C (the pin retired) deletes what
// is left of the pin test. The two identity tests guard a defect that is fixed.

// TestScheduledPinnedShellJobQueuesForSSH proves the pin is not enforced on a
// scheduled fire. RT-Q5 says a pinned run whose executor RESOLVES to ssh is
// rejected, and sshexec's claim query omits the pin predicate because "a pinned
// run never reaches this query". That holds for the manual and token triggers
// only (api.runJob). fire() resolves the executor with ResolveExecutor, which
// knows nothing of the pin, and the run-row writer inherits jobs.runner_tag in
// SQL regardless of executor — so a pinned shell job with no explicit
// `executor: runner` is queued for the control plane with its pin recorded and
// unenforced. internal/sshexec's TestSSHPoolClaimsPinnedRun is the other half.
func TestScheduledPinnedShellJobQueuesForSSH(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, concurrency_policy, enabled, runner_tag, synced_at)
		VALUES ('uid-pinned', 'pinned', 'bash', 'Allow', 1, 'vlan-dmz', 't')`); err != nil {
		t.Fatalf("seed pinned job: %v", err)
	}

	s := New(pool, quietLog(), nil)
	s.fire("git", "pinned", "uid-pinned", "bash", "prod", "Allow", "", "default", "")

	var status, executor string
	var pin sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT status, executor, runner_tag FROM runs WHERE job_name='pinned'`).
		Scan(&status, &executor, &pin); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued (the fire is not refused or skipped today)", status)
	}
	if executor != "ssh" {
		t.Errorf("executor = %q, want ssh (a shell job's run-type default)", executor)
	}
	if pin.String != "vlan-dmz" {
		t.Errorf("runner_tag = %q, want vlan-dmz (inherited onto a run no runner will ever claim)", pin.String)
	}
}

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

// TestResolveExecutorCannotTellSameNamedJobsApart proves the executor lookup
// still crosses job identities. ResolveExecutor reads jobs.executor by (name,
// source) and takes no uid at all, so two twins declaring different executors
// get one answer between them and one of them runs on an executor its definition
// did not ask for. Passing JobUID to the enqueue (above) does not fix this; the
// resolver needs the identity too (plan Phase B).
func TestResolveExecutorCannotTellSameNamedJobsApart(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool)

	got := fireTwins(t, pool, "executor")
	if got["scope-a"] != got["scope-b"] {
		t.Fatalf("twins resolved to different executors (%v): the lookup is identity-aware now — "+
			"invert this test to assert each run matches its own job's executor", got)
	}
}
