package settings

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestSetScopeRunnersRefusesAnUnauthorizedAddition pins the seam that ties the
// API's per-runner authorization to the write. The handler authorizes the
// runners it believes are being ADDED and hands the writer that set; the writer
// re-derives the additions inside its own transaction. If the two disagree —
// another operator unbound a runner in between, so a runner the caller listed
// as "already bound" is now an addition nobody gated — the write must be
// refused, not carried out on the strength of a check that never happened.
func TestSetScopeRunnersRefusesAnUnauthorizedAddition(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scoperunners.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('s1','shared','cronomicon','t')`)
	for _, id := range []string{"r-a", "r-b"} {
		exec(`INSERT INTO runners(id, name, status, registered_at, created_at) VALUES(?, ?, 'online', 't', 't')`, id, id)
	}

	// The caller was authorized for r-a only. r-b is in the list because, when the
	// caller looked, it was already bound — and it no longer is.
	authorized := map[string]bool{"r-a": true}
	_, err = SetScopeRunners(ctx, pool, "s1", []string{"r-a", "r-b"}, "ops@example",
		func(id string) bool { return authorized[id] })
	if !errors.Is(err, ErrBindingsChanged) {
		t.Fatalf("SetScopeRunners = %v, want ErrBindingsChanged", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners`).Scan(&n); err != nil || n != 0 {
		t.Errorf("binding rows after the refused write = %d (err %v), want 0 — nothing may be half-applied", n, err)
	}

	// A runner that IS still bound needs no fresh authorization to stay.
	exec(`INSERT INTO scope_runners(scope_id, runner_id, runner_name, bound_at) VALUES('s1','r-b','r-b','t')`)
	sc, err := SetScopeRunners(ctx, pool, "s1", []string{"r-a", "r-b"}, "ops@example",
		func(id string) bool { return authorized[id] })
	if err != nil {
		t.Fatalf("SetScopeRunners with r-b already bound: %v", err)
	}
	if len(sc.BoundRunners) != 2 {
		t.Errorf("bound runners = %+v, want both", sc.BoundRunners)
	}
}

// TestBoundScopeWithWaitingRunsCannotBeRenamedOrDeleted. A run carries its scope
// by NAME. Rename or delete a bound scope while runs wait under that name and no
// row answers to it any more: the runs read as unrestricted and any runner in
// the agency may claim them. Both edits are refused until the work drains — and
// only then: an unbound scope, a bound scope with nothing waiting, and an edit
// that does not change the name are all unaffected.
func TestBoundScopeWithWaitingRunsCannotBeRenamedOrDeleted(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "scopebusy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	mk := func(name string) string {
		t.Helper()
		sc, err := CreateScope(ctx, pool, LocalScopeInput{Scope: name, Hosts: []string{"h1"}}, "ops@example")
		if err != nil {
			t.Fatalf("create scope %s: %v", name, err)
		}
		return sc.ID
	}
	queue := func(id, scope, executor string) {
		exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		      VALUES (?, 'j', 'bash', ?, 'queued', 'seed', 'manual', ?, 't')`, id, scope, executor)
	}
	bind := func(scopeID string) {
		exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES (?, 'r1', 'runner-1', 't')`, scopeID)
	}
	rename := func(id, to string) error {
		_, _, err := UpdateScope(ctx, pool, id, LocalScopeInput{Scope: to, Hosts: []string{"h1"}}, "ops@example")
		return err
	}
	isBusy := func(err error) bool {
		var busy *ErrBoundScopeBusy
		return errors.As(err, &busy)
	}

	// Unbound, with a queued run: free to rename.
	open := mk("open")
	queue("run-open", "open", "runner")
	if err := rename(open, "open-2"); err != nil {
		t.Errorf("renaming an UNBOUND scope with a queued run = %v, want it allowed", err)
	}

	// Bound, nothing waiting: free to rename. An ssh run does not count — no
	// runner claims it, so nothing about it changes.
	idle := mk("idle")
	bind(idle)
	queue("run-ssh", "idle", "ssh")
	if err := rename(idle, "idle-2"); err != nil {
		t.Errorf("renaming a bound scope with only an ssh run queued = %v, want it allowed", err)
	}

	// Bound, with a queued runner run.
	busy := mk("dmz")
	bind(busy)
	queue("run-dmz", "dmz", "runner")
	if err := rename(busy, "dmz-web"); !isBusy(err) {
		t.Errorf("renaming a bound scope with a queued run = %v, want ErrBoundScopeBusy", err)
	}
	if _, err := DeleteScope(ctx, pool, busy, "ops@example"); !isBusy(err) {
		t.Errorf("deleting a bound scope with a queued run = %v, want ErrBoundScopeBusy", err)
	}
	var name string
	if err := pool.QueryRow(`SELECT name FROM scopes WHERE id = ?`, busy).Scan(&name); err != nil || name != "dmz" {
		t.Errorf("after the refusals the scope is %q (err %v), want it untouched as dmz", name, err)
	}
	// An edit that keeps the name is not a rename.
	if _, _, err := UpdateScope(ctx, pool, busy, LocalScopeInput{Scope: "dmz", Hosts: []string{"h1", "h2"}}, "ops@example"); err != nil {
		t.Errorf("editing the hosts of a busy bound scope = %v, want it allowed", err)
	}

	// A parked run counts too: it is enqueued later under the same name.
	exec(`UPDATE runs SET status = 'success' WHERE id = 'run-dmz'`)
	exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, status)
	      VALUES ('p1', 'job', 'j', 'cronomicon', 'dmz', 't', 'ops@example', 't', 'pending')`)
	if err := rename(busy, "dmz-web"); !isBusy(err) {
		t.Errorf("renaming a bound scope with a parked run = %v, want ErrBoundScopeBusy", err)
	}

	// Drained: both edits go through.
	exec(`DELETE FROM pending_runs`)
	if err := rename(busy, "dmz-web"); err != nil {
		t.Errorf("renaming once nothing is waiting = %v, want it allowed", err)
	}
	if found, err := DeleteScope(ctx, pool, busy, "ops@example"); err != nil || !found {
		t.Errorf("deleting once nothing is waiting = %v, %v; want it allowed", found, err)
	}
}
