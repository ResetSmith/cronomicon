package runner

import (
	"testing"
	"time"
)

// FX-18 — `degraded` is a status the OpenAPI enum has always promised and the UI
// has always drawn a tile for, but nothing produced: every write is
// online/offline/draining and the CHECK constraint forbids anything else, so the
// tile read 0 forever.
//
// The gap it names is real. last_seen_at is written on every long-poll and the
// reaper waits cfg.RunnerOfflineAfter (5m by default) before calling a runner
// offline, so for up to five minutes a runner that has already died reads
// **Online** — in its row, in the tile counts, and to an operator deciding where
// to send work. `degraded` is that window, derived at read time rather than
// stored (storing it would need the CHECK constraint widened AND the reaper's
// `WHERE status IN ('online','draining')` sweep taught about it, or a degraded
// runner would never be offlined or deregistered).

func withStatusNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := statusNow
	statusNow = func() time.Time { return at }
	t.Cleanup(func() { statusNow = prev })
}

func TestDeriveStatus(t *testing.T) {
	nowT := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	withStatusNow(t, nowT)
	const offlineAfter = 5 * time.Minute
	stamp := func(ago time.Duration) *string {
		s := nowT.Add(-ago).Format(time.RFC3339)
		return &s
	}

	cases := []struct {
		name     string
		stored   string
		lastSeen *string
		want     string
	}{
		{"a fresh heartbeat is online", "online", stamp(30 * time.Second), "online"},
		{"just inside the window is still online", "online", stamp(degradedAfter - time.Second), "online"},
		{"a stale heartbeat is degraded", "online", stamp(degradedAfter + time.Second), "degraded"},
		// Past the offline threshold the reaper is about to take over, but until it
		// does, degraded is still truer than online.
		{"past the offline threshold is degraded, not online", "online", stamp(offlineAfter + time.Minute), "degraded"},
		// Registered but never polled: not fresh, and the reaper owns calling it offline.
		{"never polled is degraded", "online", nil, "degraded"},
		{"an unparseable heartbeat is degraded, matching the reaper", "online", new("not-a-time"), "degraded"},
		// Only `online` degrades — the other two are lifecycle states this must not
		// overwrite. A draining runner is already leaving; an offline one is reaped.
		{"draining is left alone however stale", "draining", stamp(time.Hour), "draining"},
		{"offline is left alone", "offline", stamp(time.Hour), "offline"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveStatus(tc.stored, tc.lastSeen, offlineAfter); got != tc.want {
				t.Errorf("deriveStatus(%q, stale) = %q, want %q", tc.stored, got, tc.want)
			}
		})
	}
}

// The derivation is presentation only: it must never be written back, or the
// CHECK constraint would reject it and the reaper's sweep would stop seeing the
// runner at all.
func TestDeriveStatusDoesNotChangeStoredStatus(t *testing.T) {
	svc := newTestService(t)
	nowT := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	withStatusNow(t, nowT)
	stale := nowT.Add(-10 * time.Minute).Format(time.RFC3339)
	insertRunnerWithHeartbeat(t, svc, "r-degraded", "runner-stale", "online", stale, stale)

	row := runnerRow{ID: "r-degraded", Name: "runner-stale", Status: "online", Capabilities: "[]", LastSeenAt: &stale}
	resp, err := rowToResponse(row, 5*time.Minute)
	if err != nil {
		t.Fatalf("rowToResponse: %v", err)
	}
	if resp.Status != "degraded" {
		t.Errorf("response status = %q, want degraded", resp.Status)
	}

	var stored string
	if err := svc.db.QueryRow(`SELECT status FROM runners WHERE id='r-degraded'`).Scan(&stored); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if stored != "online" {
		t.Errorf("stored status = %q, want it untouched at online", stored)
	}
}

//go:fix inline
