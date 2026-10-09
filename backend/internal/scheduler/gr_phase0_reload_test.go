package scheduler

import (
	"context"
	"testing"
)

// Present defect 29, found on 2026-10-09 by the review of 2.4.0's Phase R2;
// pinned, NOT fixed. It passes on the code as it is.
//
// A reload removes every cron entry first and reads them back afterwards. If
// the read fails, the scheduler is left with NO entries, and it does not
// remember that: what it compares to decide whether to reload (the commit and
// a fingerprint of the tables) is as it was. So the next caller that finds
// nothing changed does nothing, and that includes the five-minute backstop.
// Every schedule has stopped firing, and one log line says a reload failed.
//
// The read fails when the context it is given has ended. Callers that hand it
// a request's context: the reload after an in-app write that the fingerprint
// does not notice (a restore from the recycle bin, a purge), when the client
// goes away in that moment. Until Phase R2's registry commit the hook after a
// Git sync was another, when a scope resync's client went away during the
// sync's transaction; that hook is no longer called on a context that can end.
func TestGR0_AReloadThatFailsLeavesTheSchedulerEmpty(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	if _, err := pool.ExecContext(ctx, `UPDATE git_repos SET last_sha = 'sha1' WHERE id = 'global'`); err != nil {
		t.Fatal(err)
	}
	s := New(pool, quietLog(), nil)
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("entries after the first reload = %d, want 1", n)
	}

	// A reload on a context that has ended.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Reload(gone); err == nil {
		t.Fatalf("a reload on a context that has ended reported success")
	}
	// The callers that would put it right, with nothing changed since.
	s.ReloadIfChanged(ctx, "sha1")            // the next sync's hook, same commit
	s.ReloadIfChanged(ctx, s.currentSHA(ctx)) // the backstop

	switch n := len(s.cr.Entries()); n {
	case 0:
		// Today: the schedule no longer fires, and nothing will reload it until
		// a definition or the commit changes.
	case 1:
		t.Errorf("the scheduler recovered its entry: this is fixed, and the test is to be inverted")
	default:
		t.Errorf("entries after the failed reload = %d", n)
	}
	// What does bring it back: a reload that is asked for outright.
	if err := s.Reload(ctx); err != nil || len(s.cr.Entries()) != 1 {
		t.Errorf("an outright reload: err=%v, entries=%d; want the entry back", err, len(s.cr.Entries()))
	}
}
