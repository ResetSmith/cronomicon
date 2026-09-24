// Tests for the compliance audit stream (LU-10).
//
// Why this file is worth its length: audit.log is read by people and systems
// outside this repo — a SIEM, a shipper, an auditor answering "what happened on
// the 14th" — and every property they rely on is invisible from the call site.
// A line that is not independently parseable, a generation named for the wrong
// day, a rotation that discards the file it was supposed to preserve, or a sink
// that goes quiet after one transient error all look identical to a healthy
// stream from inside the process. Existence-only assertions pass straight
// through most of those, so the rotation tests here assert on CONTENT: which
// records ended up in which file, in what order.
package auditsink

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

// fakeClock drives rotation deterministically. It is mutex-guarded because the
// concurrency test reads it from the sink's goroutines while the test body may
// still hold a reference; without the mutex `-race` would flag the seam itself
// rather than the code under test.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// day1/day2 straddle a UTC midnight. Both are mid-morning/mid-afternoon so no
// assertion here is accidentally sensitive to the local zone.
var (
	day1    = time.Date(2026, 3, 14, 14, 0, 0, 0, time.UTC)
	day2    = time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)
	day1Gen = "20260314"
	day2Gen = "20260315"
)

// newTestSink builds a sink on a controlled clock. It bypasses New because New
// hard-wires time.Now and opens the file before a test could swap the seam in —
// and open-time behaviour (the down-over-midnight rotation) is exactly what
// several of these tests are about.
func newTestSink(t *testing.T, path string, clock *fakeClock, report func(error)) *Sink {
	t.Helper()
	if report == nil {
		report = func(error) {}
	}
	s := &Sink{report: report, now: clock.Now}
	t.Cleanup(func() { _ = s.Close() })
	if path != "" {
		if err := s.SetPath(path); err != nil {
			t.Fatalf("SetPath(%s): %v", path, err)
		}
	}
	return s
}

// event builds a recognisable record. Actor is the marker every content
// assertion keys on, because it survives the JSON round-trip verbatim.
func event(actor string, at time.Time) auditlog.Event {
	return auditlog.Event{
		V:      auditlog.EventVersion,
		At:     at.Format(time.RFC3339),
		Source: "auth",
		Kind:   auditlog.AuthLogin,
		Actor:  actor,
	}
}

// readEvents parses every line of a JSON Lines file. It fails the test on the
// first unparseable line: a consumer reads this file line-by-line, so one
// malformed record is not a cosmetic defect, it stops the shipper.
func readEvents(t *testing.T, path string) []auditlog.Event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []auditlog.Event
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var ev auditlog.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("%s line %d is not parseable JSON (%v): %q", path, i+1, err, line)
		}
		out = append(out, ev)
	}
	return out
}

// actorsIn is the content assertion helper: which records landed in this file,
// in order.
func actorsIn(t *testing.T, path string) []string {
	t.Helper()
	var out []string
	for _, ev := range readEvents(t, path) {
		out = append(out, ev.Actor)
	}
	return out
}

func wantActors(t *testing.T, path string, want []string, why string) {
	t.Helper()
	got := actorsIn(t, path)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s\n  %s holds %v\n  want %v", why, filepath.Base(path), got, want)
	}
}

// writeRaw plants a pre-existing audit file with a given mtime — the fixture for
// "the process was down over midnight". mtime is what openLocked judges, so it
// is the whole fixture.
func writeRaw(t *testing.T, path string, mtime time.Time, evs ...auditlog.Event) {
	t.Helper()
	var buf strings.Builder
	for _, ev := range evs {
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// TestWriteEmitsOneParseableVersionedLine pins the file format itself. JSON
// Lines is a contract with consumers outside this repo: one record per line,
// each independently parseable, each carrying the schema version they branch on.
// A record spread over several lines, or one missing "v", breaks every consumer
// at once and is invisible from inside the process.
func TestWriteEmitsOneParseableVersionedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	clock := newClock(day1)
	s := newTestSink(t, path, clock, nil)

	s.Write(event("alice@corp.example", day1))

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("one event produced %d newlines, want exactly 1 (JSON Lines is one record per line): %q", n, b)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(string(b), "\n")), &raw); err != nil {
		t.Fatalf("the written line is not parseable JSON (%v): %q", err, b)
	}
	if v, ok := raw["v"].(float64); !ok || int(v) != auditlog.EventVersion {
		t.Errorf(`"v" = %v, want %d — consumers branch on the schema version`, raw["v"], auditlog.EventVersion)
	}
	if raw["actor"] != "alice@corp.example" {
		t.Errorf("actor = %v, want alice@corp.example", raw["actor"])
	}
}

// TestRotationNamesTheGenerationAfterTheDayItHolds is the rotation bug that
// matters. Rotating on a UTC date change is easy; naming the generation after
// the day whose RECORDS it contains — yesterday — rather than after the day the
// rotation happened to run is the part that goes wrong. An auditor asking "what
// happened on the 14th" reads audit.log.20260314 and must find the 14th's
// records there, so this asserts on content, not on the file existing.
func TestRotationNamesTheGenerationAfterTheDayItHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	clock := newClock(day1)
	s := newTestSink(t, path, clock, nil)

	s.Write(event("day1-first", day1))
	s.Write(event("day1-second", day1))

	clock.Set(day2)
	s.Write(event("day2-only", day2))

	wantActors(t, path+"."+day1Gen,
		[]string{"day1-first", "day1-second"},
		"the dated generation must hold the records of the day it is NAMED for")
	wantActors(t, path,
		[]string{"day2-only"},
		"the live file must hold only today's records after a rotation")

	if _, err := os.Stat(path + "." + day2Gen); err == nil {
		t.Errorf("a generation named for TODAY (%s) exists — today's records belong in the live file", day2Gen)
	}
}

// TestOpenRotatesWhenTheProcessWasDownOverMidnight is the case a naive
// "check the clock on write" implementation gets wrong, and it is the common
// case in production: the server restarts overnight, finds yesterday's file, and
// appends today's records to it. Nothing errors, nothing looks broken — but the
// live file now mixes two days and, when it eventually rotates, the whole lot is
// filed under today's date. The 14th's evidence is then in the 15th's file.
//
// The day must therefore come from the file's mtime, not from the clock.
func TestOpenRotatesWhenTheProcessWasDownOverMidnight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	writeRaw(t, path, day1, event("written-before-the-restart", day1))

	clock := newClock(day2)
	s := newTestSink(t, path, clock, nil)
	s.Write(event("written-after-the-restart", day2))

	wantActors(t, path+"."+day1Gen,
		[]string{"written-before-the-restart"},
		"the pre-restart records must be rotated into YESTERDAY's generation on open")
	wantActors(t, path,
		[]string{"written-after-the-restart"},
		"the reopened live file must contain only today's records")
}

// TestSameDaySecondRotationMergesRatherThanDiscards covers the appendIntoLocked
// path: restart twice in one day after a rotation has already produced a
// generation for the previous day. os.Rename would silently clobber the existing
// generation, destroying records that were already safely filed — a data-loss
// bug that leaves behind a file of exactly the right name, so existence checks
// and even line-count sanity checks would not notice.
func TestSameDaySecondRotationMergesRatherThanDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	gen := path + "." + day1Gen

	// A generation for day 1 already exists (an earlier rotation filed it)...
	writeRaw(t, gen, day1, event("already-rotated", day1))
	// ...and the live file holds more of day 1's records, written after it.
	writeRaw(t, path, day1, event("written-after-that-rotation", day1))

	clock := newClock(day2)
	s := newTestSink(t, path, clock, nil)
	s.Write(event("day2-record", day2))

	wantActors(t, gen,
		[]string{"already-rotated", "written-after-that-rotation"},
		"a second rotation into an existing generation must MERGE, never discard")
	wantActors(t, path,
		[]string{"day2-record"},
		"the live file must be fresh after the merge")
}

// TestDisabledSinkWritesNothingAtAll pins the off switch. The sink is optional —
// an install that has not configured a path must not have files appear in the
// working directory (or anywhere) as a side effect of ordinary auditing, and
// Write must not panic on a nil file handle in the process's hottest audit
// paths.
func TestDisabledSinkWritesNothingAtAll(t *testing.T) {
	dir := t.TempDir()
	clock := newClock(day1)

	// Never configured.
	s := newTestSink(t, "", clock, nil)
	s.Write(event("dropped", day1))
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("a never-configured sink created %v in %s, want nothing", entries, dir)
	}

	// Configured, then disabled: subsequent writes must not reach the old file.
	path := filepath.Join(dir, "audit.log")
	if err := s.SetPath(path); err != nil {
		t.Fatalf("SetPath: %v", err)
	}
	s.Write(event("kept", day1))
	if err := s.SetPath(""); err != nil {
		t.Fatalf("SetPath(\"\") should disable cleanly: %v", err)
	}
	if got := s.Path(); got != "" {
		t.Errorf("Path() = %q after SetPath(\"\"), want empty", got)
	}
	s.Write(event("dropped-after-disable", day1))

	wantActors(t, path, []string{"kept"},
		"a disabled sink must not append to the file it used to hold open")
}

// TestUnopenablePathReportsAndRecoversOnceFixed is the availability property.
// An audit stream that stops silently is worse than one that was never enabled,
// because the gap reads as an absence of events. So a bad path must (a) not
// panic — this runs inside the login path, (b) surface through Report so an
// operator learns about it, and (c) heal on its own once the obstruction is
// cleared, rather than staying dead until someone restarts the process.
func TestUnopenablePathReportsAndRecoversOnceFixed(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked") // a regular file where a directory must be
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	path := filepath.Join(blocker, "audit.log")

	var mu sync.Mutex
	var reported []error
	report := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, err)
	}

	clock := newClock(day1)
	s := &Sink{report: report, now: clock.Now}
	t.Cleanup(func() { _ = s.Close() })

	// SetPath must return the failure rather than panic, and the sink stays up.
	if err := s.SetPath(path); err == nil {
		t.Fatal("SetPath onto a path whose parent is a regular file should fail")
	}
	s.Write(event("lost-while-broken", day1)) // must not panic; retries, fails, reports

	mu.Lock()
	n := len(reported)
	mu.Unlock()
	if n == 0 {
		t.Fatal("an unopenable audit path was never reported — the stream would go silent unnoticed")
	}

	// Operator clears the obstruction. The retry is on the notice throttle, so
	// nothing changes until the interval has passed...
	if err := os.Remove(blocker); err != nil {
		t.Fatalf("remove blocker: %v", err)
	}
	clock.Advance(errNoticeInterval + time.Second)
	s.Write(event("recovered", day1))

	wantActors(t, path, []string{"recovered"},
		"the sink must reopen and resume once the obstruction is cleared — not stay dead for the life of the process")
}

// TestReportIsRateLimited stops the failure report becoming the flood it is
// meant to warn about. A persistently unwritable audit file is hit on every
// single audited action; an unthrottled report would fill the process log (and,
// with a notifier attached, page continuously) for a condition that only needs
// saying once a minute.
func TestReportIsRateLimited(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	var mu sync.Mutex
	var count int
	clock := newClock(day1)
	s := newTestSink(t, path, clock, func(error) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	// Force every subsequent write to fail at the syscall, which is the path
	// that calls notice on each attempt.
	s.mu.Lock()
	_ = s.f.Close()
	s.mu.Unlock()

	for range 20 {
		s.Write(event("flood", day1))
	}
	mu.Lock()
	after := count
	mu.Unlock()
	if after != 1 {
		t.Fatalf("20 consecutive write failures produced %d reports, want 1 within the throttle window", after)
	}

	clock.Advance(errNoticeInterval + time.Second)
	s.Write(event("flood", day1))
	mu.Lock()
	after = count
	mu.Unlock()
	if after != 2 {
		t.Errorf("reports = %d after the throttle window elapsed, want 2 — a lasting fault must keep being reported, just not on every event", after)
	}
}

// TestConcurrentWritesAndSetPathLoseNoLines is the property that makes the sink
// safe to call from anywhere. Audit writes happen on every request goroutine
// while an operator can re-point the file from the settings panel at the same
// moment. Two failures are possible and both are silent: a dropped record (the
// file understates what happened) and an interleaved one (two records shredded
// into unparseable lines, which takes the shipper down for every event after
// it). Run under -race.
func TestConcurrentWritesAndSetPathLoseNoLines(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "audit-a.log")
	pathB := filepath.Join(dir, "audit-b.log")

	// The real clock here, deliberately: openLocked compares the file's mtime
	// (real time) against s.now(), so a frozen fake clock would make every
	// re-open look like a day boundary and turn this into a rotation test.
	s := New(Options{Path: pathA, Report: func(err error) { t.Errorf("unexpected sink failure: %v", err) }})
	t.Cleanup(func() { _ = s.Close() })

	const writers, perWriter = 8, 60
	var writersWG, flipperWG sync.WaitGroup
	stop := make(chan struct{})

	flipperWG.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := pathA
			if i%2 == 1 {
				p = pathB
			}
			if err := s.SetPath(p); err != nil {
				t.Errorf("SetPath during concurrent writes: %v", err)
				return
			}
		}
	})

	for w := range writers {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for i := range perWriter {
				s.Write(event(actorID(w, i), time.Now().UTC()))
			}
		}(w)
	}

	writersWG.Wait()
	close(stop)
	flipperWG.Wait()

	// Every record must be somewhere under dir, exactly once, and every line must
	// be independently parseable (readEvents fails the test otherwise).
	seen := map[string]int{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, ev := range readEvents(t, filepath.Join(dir, e.Name())) {
			seen[ev.Actor]++
		}
	}
	for w := range writers {
		for i := range perWriter {
			id := actorID(w, i)
			if seen[id] != 1 {
				t.Fatalf("record %s appears %d times across %s, want exactly 1 (a concurrent SetPath lost or duplicated it)", id, seen[id], dir)
			}
		}
	}
	if len(seen) != writers*perWriter {
		t.Errorf("distinct records = %d, want %d", len(seen), writers*perWriter)
	}
}

func actorID(w, i int) string {
	return "w" + strconv.Itoa(w) + "-" + strconv.Itoa(i)
}
