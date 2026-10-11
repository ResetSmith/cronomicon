package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// duringReload is a log handler that runs fn once, at the first line a reload
// writes for an entry it registers: after the reload has READ the tables and
// before it notes what it loaded. It is how a test puts a sync's commit in
// that window without a seam in the scheduler.
type duringReload struct {
	once sync.Once
	fn   func()
}

func (h *duringReload) Enabled(context.Context, slog.Level) bool { return true }
func (h *duringReload) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *duringReload) WithGroup(string) slog.Handler            { return h }
func (h *duringReload) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "scheduler: registered job schedule" {
		h.once.Do(h.fn)
	}
	return nil
}

// Until this test was inverted (present defect 33, found on 2026-10-09 by the
// review of 2.4.0's Phase R3; in the released code, where the stamp was the
// last sync's commit; TestGR0_ASyncThatCommitsDuringAReloadIsNotedAndNotLoaded
// pinned it) a reload read the entries and only afterwards noted the
// generation and the fingerprint it "loaded". A sync that committed between
// the two was noted and NOT loaded: the scheduler remembered the new
// generation while firing the old entries. The sync's own hook then asked for
// a reload at that generation, was told nothing changed, and did nothing; so
// did the five-minute backstop, every time, until something else changed the
// tables. A schedule the sync added did not fire, and one it removed went on
// firing.
//
// The window was the reload after the read: the swap of the entries and five
// small queries. It needed a reload under way (a save in the app, another
// repository's sync) at the moment a sync committed.
//
// Now the generation and the fingerprint are read BEFORE the entries, so what
// is remembered is never newer than what was loaded, and the hook's reload
// loads what the sync committed.
//
// The sync here changes the TIMING of an entry and adds none. The fingerprint
// counts the entries and does not see a timing, so the generation alone tells
// the hook that there is something to load: it is the generation's half of
// the fix that this test holds, and the test below holds the fingerprint's.
func TestASyncThatCommitsDuringAReloadIsLoadedByItsHook(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "first", "0 2 * * *", 0)

	h := &duringReload{fn: func() {
		// What a sync commits: the entry's new timing, and the generation.
		if _, err := pool.ExecContext(ctx, `UPDATE definition_schedules SET cron = '0 3 * * *' WHERE owner_name = 'j'`); err != nil {
			t.Errorf("change the timing: %v", err)
		}
		if _, err := pool.ExecContext(ctx, `UPDATE definitions_generation SET n = n + 1 WHERE id = 1`); err != nil {
			t.Errorf("advance the generation: %v", err)
		}
	}}
	s := New(pool, slog.New(h), nil)
	// The hour at which the one entry fires next.
	firesAt := func() int {
		t.Helper()
		es := s.cr.Entries()
		if len(es) != 1 {
			t.Fatalf("%d entries, want 1", len(es))
		}
		return es[0].Schedule.Next(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)).Hour()
	}
	before, fpBefore := s.currentGeneration(ctx), s.dbFingerprint(ctx)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := s.currentGeneration(ctx)
	if after == before {
		t.Fatalf("the sync did not commit during the reload: the test proves nothing")
	}
	if fp := s.dbFingerprint(ctx); fp != fpBefore {
		t.Fatalf("the fingerprint moved (%s to %s): the test no longer holds the generation's half alone", fpBefore, fp)
	}
	if h := firesAt(); h != 2 {
		t.Fatalf("the reload's entry fires at %d, want 2: the timing it read before the sync committed", h)
	}

	// The sync's hook, arriving after the reload has finished.
	s.ReloadIfChanged(ctx, after)
	if h := firesAt(); h != 3 {
		t.Fatalf("after the sync's hook the entry fires at %d, want 3: the timing the sync committed is not loaded", h)
	}
	// And nothing more to do: the backstop finds that what was loaded is what is there.
	reloads := s.reloads
	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	if s.reloads != reloads {
		t.Fatalf("the backstop reloaded again after the hook had loaded the sync's commit")
	}
}

// The other half of what a reload remembers. A write that does not advance the
// generation (a save in the app) is seen by the fingerprint of the tables, and
// one that landed during a reload was remembered as loaded in the same way.
// The backstop is what loads it.
func TestAnInAppWriteDuringAReloadIsLoadedByTheBackstop(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "first", "0 2 * * *", 0)

	h := &duringReload{fn: func() {
		seedSchedule(t, pool, "job", "j", "second", "0 3 * * *", 1)
	}}
	s := New(pool, slog.New(h), nil)
	before := s.currentGeneration(ctx)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after := s.currentGeneration(ctx); after != before {
		t.Fatalf("the generation moved from %s to %s: this test is about a write that does not move it", before, after)
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("the reload registered %d entries, want the one it read before the write", n)
	}

	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	if n := len(s.cr.Entries()); n != 2 {
		t.Fatalf("%d entries after the backstop, want 2: the entry written during the reload is not loaded", n)
	}
}
