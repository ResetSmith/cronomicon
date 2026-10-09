package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"
)

// A reload that fails changes nothing: what was firing goes on firing.
//
// Until this test was inverted (present defect 29, found by the review of
// 2.4.0's Phase R2; TestGR0_AReloadThatFailsLeavesTheSchedulerEmpty pinned it)
// a reload removed every cron entry first and read them back afterwards. If
// the read failed, the scheduler was left with NO entries, and it did not
// remember that: what it compares to decide whether to reload (the commit and
// a fingerprint of the tables) was as it had been. So the next caller that
// found nothing changed did nothing, the five-minute backstop included. Every
// schedule had stopped firing, and one log line said a reload had failed.
//
// The read fails when the context it is given has ended, which is a request
// that was abandoned: the reload after an in-app write goes on the request's
// context.
func TestAReloadThatFailsLeavesTheEntriesFiring(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	s := New(pool, quietLog(), nil)
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("entries after the first reload = %d, want 1", n)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	firstNext := s.cr.Entries()[0].Schedule.Next(at)

	// A reload on a context that has ended.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Reload(gone); err == nil {
		t.Fatalf("a reload on a context that has ended reported success")
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("entries after a reload that failed = %d, want the one that was firing", n)
	}
	if got := s.cr.Entries()[0].Schedule.Next(at); !got.Equal(firstNext) {
		t.Errorf("the entry left by the failed reload fires next at %s, was %s", got, firstNext)
	}
	// The hook and the backstop, with nothing changed: still one entry.
	s.ReloadIfChanged(gone, s.currentGeneration(ctx))
	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	if n := len(s.cr.Entries()); n != 1 {
		t.Errorf("entries after the hook and the backstop = %d, want 1", n)
	}
}

// A reload that succeeds still REPLACES the entries: a changed timing fires at
// the new time, an entry whose row has gone is gone, a new one is added, and
// none is registered twice.
func TestAReloadReplacesTheEntries(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	s := New(pool, quietLog(), nil)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nexts := func() map[time.Time]int {
		t.Helper()
		out := map[time.Time]int{}
		for _, e := range s.cr.Entries() {
			out[e.Schedule.Next(at).UTC()]++
		}
		return out
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	before := nexts()
	if len(s.cr.Entries()) != 1 {
		t.Fatalf("entries = %d, want 1", len(s.cr.Entries()))
	}

	// The same tables, reloaded three times: one entry, not three.
	for i := 0; i < 3; i++ {
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("entries after three reloads of the same tables = %d, want 1", n)
	}

	// The timing changes.
	if _, err := pool.ExecContext(ctx, `UPDATE definition_schedules SET cron = '0 5 * * *' WHERE owner_name = 'j'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	after := nexts()
	if len(s.cr.Entries()) != 1 {
		t.Fatalf("entries after the timing changed = %d, want 1", len(s.cr.Entries()))
	}
	for when := range after {
		if before[when] != 0 {
			t.Errorf("after the timing changed the entry still fires at %s", when)
		}
	}

	// A second schedule is added, then the first is removed.
	seedSchedule(t, pool, "job", "j", "extra", "0 7 * * *", 1)
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cr.Entries()); n != 2 {
		t.Fatalf("entries after a schedule was added = %d, want 2", n)
	}
	if _, err := pool.ExecContext(ctx, `DELETE FROM definition_schedules WHERE owner_name = 'j' AND name = 'default'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("entries after a schedule was removed = %d, want 1", n)
	}
	// Every schedule removed: the scheduler is empty because the tables are.
	if _, err := pool.ExecContext(ctx, `DELETE FROM definition_schedules`); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s.cr.Entries()); n != 0 {
		t.Errorf("entries with no schedule in the tables = %d, want 0", n)
	}
}

// A timezone change builds a new engine and reloads into it. If that reload
// fails the new engine has no entries (the old one's went with the old
// engine), and the backstop's next pass puts them back. The code's comment
// always said so; it was not true, because what the backstop compares had not
// moved, and it found nothing to do.
func TestATimezoneRebuildWhoseReloadFailsIsRetriedByTheBackstop(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "default", "0 2 * * *", 0)
	s := New(pool, quietLog(), nil)
	if err := s.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { <-s.cr.Stop().Done() }()

	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.RebuildWithLocation(gone, loc); err == nil {
		t.Fatalf("a rebuild whose reload could not read reported success")
	}
	if n := len(s.cr.Entries()); n != 0 {
		t.Fatalf("entries in the new engine after its reload failed = %d, want 0 (this test is about what happens next)", n)
	}
	// The backstop's pass, with the commit and the tables as they were.
	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	if n := len(s.cr.Entries()); n != 1 {
		t.Errorf("entries after the backstop's pass = %d, want the schedule back", n)
	}
}

// Reloads asked for at the same moment leave every schedule in the engine
// ONCE.
//
// Until this test was inverted (present defect 31, found while fixing defect
// 29; TestGR0_TwoReloadsAtOnceRegisterEveryEntryTwice pinned it) nothing
// serialised reloads. Each removed the entries and then registered the set it
// had read, so two that overlapped could both remove and then both register:
// every schedule was in the engine twice, and a job on the default concurrency
// policy was enqueued twice at each of its times, until the next reload. That
// next reload was not the backstop's (the generation and the tables were as the
// scheduler remembered them); it was whenever a definition was next saved or a
// sync next landed. On the released code two reloads at once doubled the
// entries in 147 rounds of 150.
//
// A reload is asked for by every in-app save, by the hook after every sync,
// and by the backstop, each on its own goroutine.
func TestReloadsAtOnceLeaveEachEntryOnce(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	const schedules = 20
	for i := 0; i < schedules; i++ {
		seedSchedule(t, pool, "job", "j", "s"+string(rune('a'+i)), "0 2 * * *", i)
	}
	s := New(pool, quietLog(), nil)
	for round := 1; round <= 150; round++ {
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				if g%2 == 0 {
					_ = s.Reload(ctx)
				} else {
					s.ReloadIfChanged(ctx, "") // an empty generation forces a reload
				}
			}(g)
		}
		wg.Wait()
		if n := len(s.cr.Entries()); n != schedules {
			t.Fatalf("round %d: four reloads at once left %d entries for %d schedules", round, n, schedules)
		}
	}
	// A reload that reads a CHANGED table while another is under way: whichever
	// swaps last read last, so the entries are the table's.
	for round := 1; round <= 50; round++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = s.Reload(ctx) }()
		go func() {
			defer wg.Done()
			if _, err := pool.ExecContext(ctx, `DELETE FROM definition_schedules WHERE name = 'sa'`); err != nil {
				t.Errorf("delete: %v", err)
			}
			_ = s.Reload(ctx)
		}()
		wg.Wait()
		if n := len(s.cr.Entries()); n != schedules-1 {
			t.Fatalf("round %d: after a schedule was removed and reloaded, %d entries, want %d", round, n, schedules-1)
		}
		seedSchedule(t, pool, "job", "j", "sa", "0 2 * * *", 0)
		if err := s.Reload(ctx); err != nil || len(s.cr.Entries()) != schedules {
			t.Fatalf("round %d: after the schedule was put back: err=%v, entries=%d", round, err, len(s.cr.Entries()))
		}
	}
}
