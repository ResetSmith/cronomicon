package scheduler

import (
	"context"
	"database/sql"
	"testing"
)

// SB Phase 0 — characterization tests for defects the scope-bound-runners plan
// (.aidata/20261005-runners-update.md) found by reading and needed proven before
// it builds on them.
//
// Every test here PASSES against its defect on purpose. They pin what the code
// does today so the change that fixes each one has a test to turn around rather
// than a claim to take on trust: Phase B (one executor resolver, keyed by
// identity and scope) inverts the executor test and replaces the pin test with
// its bound-scope equivalent; Phase C (the pin retired) deletes what is left of
// the pin test.

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

// TestScheduledFireStampsSiblingIdentityForSameNamedJob proves a scheduled run
// of one same-named job is recorded as its sibling. fire() receives the job's
// uid and passes it on every skip-record path, but the EnqueueParams for the run
// that actually executes omits JobUID, so resolveEnqueueUID falls back to the
// (name, source) pair — "whichever row SQLite returns", in that function's own
// words, and its claim that every real producer passes the uid is not true of
// this one. Everything the run-row writer snapshots by uid follows the wrong
// job: the run fired for one twin on its scope carries the other twin's
// identity, script reference and content hash.
//
// Found while writing the executor test below; it is not part of the plan and
// is pinned here so it is not lost. Order-independent: it asserts the two runs
// are indistinguishable, not which twin won.
func TestScheduledFireStampsSiblingIdentityForSameNamedJob(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool)

	for _, column := range []string{"job_uid", "script_ref", "content_hash"} {
		got := fireTwins(t, pool, column)
		if got["scope-a"] != got["scope-b"] {
			t.Fatalf("%s differs between the twins' runs (%v): the fire is identity-aware now — "+
				"invert this test to assert each run carries its own job's %s", column, got, column)
		}
		if _, err := pool.Exec(`DELETE FROM runs`); err != nil {
			t.Fatalf("reset runs: %v", err)
		}
	}
}

// TestResolveExecutorCannotTellSameNamedJobsApart proves the executor lookup
// crosses job identities independently of the defect above. ResolveExecutor
// reads jobs.executor by (name, source) and takes no uid at all, so two twins
// declaring different executors get one answer between them and one of them runs
// on an executor its definition did not ask for. Passing JobUID to the enqueue
// does not fix this; the resolver needs the identity too (plan Phase B).
func TestResolveExecutorCannotTellSameNamedJobsApart(t *testing.T) {
	pool := mustPool(t)
	seedTwins(t, pool)

	got := fireTwins(t, pool, "executor")
	if got["scope-a"] != got["scope-b"] {
		t.Fatalf("twins resolved to different executors (%v): the lookup is identity-aware now — "+
			"invert this test to assert each run matches its own job's executor", got)
	}
}
