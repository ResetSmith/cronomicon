package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
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

// PINNED, present defect 33 (found on 2026-10-09 by the review of 2.4.0's
// Phase R3; in the released code, where the stamp was the last sync's commit).
//
// A reload reads the entries and only afterwards notes the generation and the
// fingerprint it "loaded". A sync that commits between the two is noted and
// NOT loaded: the scheduler remembers the new generation while firing the old
// entries. The sync's own hook then asks for a reload at that generation, is
// told nothing changed, and does nothing; so does the five-minute backstop,
// every time, until something else changes the tables. A schedule the sync
// added does not fire, and one it removed goes on firing.
//
// The window is the reload after the read: the swap of the entries and five
// small queries. It needs a reload under way (a save in the app, another
// repository's sync) at the moment a sync commits.
//
// When this is fixed (the generation and the fingerprint are read BEFORE the
// entries, so that what is remembered is never newer than what was loaded),
// this test is inverted: the second entry is registered by the hook's reload.
func TestGR0_ASyncThatCommitsDuringAReloadIsNotedAndNotLoaded(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "j", 1)
	seedSchedule(t, pool, "job", "j", "first", "0 2 * * *", 0)

	h := &duringReload{fn: func() {
		// What a sync commits: a schedule entry, and the generation.
		seedSchedule(t, pool, "job", "j", "second", "0 3 * * *", 1)
		if _, err := pool.ExecContext(ctx, `UPDATE definitions_generation SET n = n + 1 WHERE id = 1`); err != nil {
			t.Errorf("advance the generation: %v", err)
		}
	}}
	s := New(pool, slog.New(h), nil)
	before := s.currentGeneration(ctx)
	if err := s.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	after := s.currentGeneration(ctx)
	if after == before {
		t.Fatalf("the sync did not commit during the reload: the test proves nothing")
	}
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("the reload registered %d entries, want the one it read before the sync committed", n)
	}

	// The sync's hook, arriving after the reload has finished; then the backstop.
	s.ReloadIfChanged(ctx, after)
	s.ReloadIfChanged(ctx, s.currentGeneration(ctx))
	if n := len(s.cr.Entries()); n != 1 {
		t.Fatalf("PIN BROKEN: %d entries after the hook and the backstop. Present defect 33 is fixed: "+
			"invert this test (want 2, the entry the sync added)", n)
	}
	var rows int
	if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM definition_schedules`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("the tables hold %d schedule entries (%v), want the two", rows, err)
	}
}
