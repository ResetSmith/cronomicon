// Why this file exists.
//
// logsink is the writer every server log line passes through (LU-4): one slog
// handler is built over it and 12 structs across as many packages hold the
// resulting *slog.Logger. That makes it the single point where the whole
// process's diagnostic output can be silently lost — and losing it is exactly
// the kind of failure that hides itself, because the thing that would report the
// problem is the thing that broke.
//
// So the properties pinned here are the ones an operator would only notice
// during an incident: that stdout survives every file-side fault (containers
// collect stdout; a file the deployment can't write must never take the logs
// away from journald), that rotation shifts generations in the direction that
// keeps the OLDEST content in the HIGHEST-numbered file rather than overwriting
// four generations with the newest one, that a single log line is never split
// across a rotation boundary, and that a live re-point (LU-5) neither truncates
// the file it is already writing nor drops the writes that race with it.
package logsink

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe tee. Writer calls tee.Write BEFORE taking its
// own mutex (deliberately — the tee is the guaranteed destination and must not
// queue behind file I/O), so the tee itself owns its synchronization. In
// production that tee is os.Stdout, whose Write is a single syscall.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// readFile reads a file that the test asserts must exist.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// mustNotExist asserts a generation was never created (or was reaped).
func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("%s: %s exists but should not", why, path)
	}
}

// captureStderr swaps os.Stderr for a pipe while fn runs. notice() writes to
// os.Stderr directly and cannot log through slog (slog is what feeds this
// writer), so the pipe is the only seam for asserting the notice fired.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	_ = r.Close()
	return string(out)
}

// TestWriteReachesBothTeeAndFile is the base contract: the file is strictly
// ADDITIVE to stdout, never a replacement. A deployment whose logs are collected
// from stdout (journald, docker) must keep seeing every line after file logging
// is switched on.
func TestWriteReachesBothTeeAndFile(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path})
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := tee.String(); got != "hello\n" {
		t.Errorf("tee = %q, want %q", got, "hello\n")
	}
	if got := readFile(t, path); got != "hello\n" {
		t.Errorf("file = %q, want %q", got, "hello\n")
	}
	if w.Path() != path {
		t.Errorf("Path() = %q, want %q", w.Path(), path)
	}
}

// TestNoPathIsPurePassThrough pins the default-off shape: with no path, the
// Writer must behave exactly like the bare stdout writer it replaced, creating
// nothing on disk. A stray file appearing in the working directory of an install
// that never asked for file logging would be a surprise at best, and on a
// read-only rootfs a startup failure at worst.
func TestNoPathIsPurePassThrough(t *testing.T) {
	dir := t.TempDir()
	tee := &syncBuffer{}
	w := New(tee, Options{})
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Write([]byte("only stdout\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := tee.String(); got != "only stdout\n" {
		t.Errorf("tee = %q, want the line", got)
	}
	if w.Path() != "" {
		t.Errorf("Path() = %q, want empty with no file configured", w.Path())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("pass-through writer created %d entries on disk, want 0: %v", len(entries), entries)
	}
}

// TestRotationKeepsOldestInHighestGeneration is the bug this test file exists
// for. The shift loop runs path.N-1 → path.N and must iterate DESCENDING; the
// ascending form compiles, passes any existence-only assertion, and silently
// overwrites every generation with the newest one — so an operator retaining
// "five generations" would in fact retain five copies of the same minute. The
// assertions are therefore on CONTENT, not on the presence of the files.
func TestRotationKeepsOldestInHighestGeneration(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path, MaxBytes: 10, Keep: 3})
	t.Cleanup(func() { _ = w.Close() })

	first, second, third := "aaaaaaaa\n", "bbbbbbbb\n", "cccccccc\n"

	// First write fills the live file (rotation only triggers once size > 0).
	if _, err := w.Write([]byte(first)); err != nil {
		t.Fatalf("write first: %v", err)
	}
	// Second write would cross the 10-byte cap → rotate before writing.
	if _, err := w.Write([]byte(second)); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if got := readFile(t, path+".1"); got != first {
		t.Errorf("after one rotation, %s.1 = %q, want the earlier content %q", path, got, first)
	}
	if got := readFile(t, path); got != second {
		t.Errorf("after one rotation, live file = %q, want the later content %q", got, second)
	}

	// Third write rotates again: the shift must be .1 → .2, so the OLDEST
	// content ends up in the HIGHEST-numbered generation.
	if _, err := w.Write([]byte(third)); err != nil {
		t.Fatalf("write third: %v", err)
	}
	if got := readFile(t, path+".2"); got != first {
		t.Errorf("%s.2 = %q, want the OLDEST content %q — the generation shift is running in the wrong direction", path, got, first)
	}
	if got := readFile(t, path+".1"); got != second {
		t.Errorf("%s.1 = %q, want %q", path, got, second)
	}
	if got := readFile(t, path); got != third {
		t.Errorf("live file = %q, want the newest content %q", got, third)
	}
}

// TestKeepBoundsGenerationsOnDisk pins the disk ceiling. The whole reason file
// logging can be on by default is that total cost is bounded by
// (Keep+1)*MaxBytes; if the oldest generation were not dropped, an install would
// accumulate a log file per rotation forever — the G1 growth this feature is
// supposed to stop.
func TestKeepBoundsGenerationsOnDisk(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path, MaxBytes: 10, Keep: 2})
	t.Cleanup(func() { _ = w.Close() })

	// Five writes ⇒ four rotations.
	for _, line := range []string{"11111111\n", "22222222\n", "33333333\n", "44444444\n", "55555555\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}

	if got := readFile(t, path); got != "55555555\n" {
		t.Errorf("live file = %q, want the newest line", got)
	}
	if got := readFile(t, path+".1"); got != "44444444\n" {
		t.Errorf("%s.1 = %q, want the previous line", path, got)
	}
	if got := readFile(t, path+".2"); got != "33333333\n" {
		t.Errorf("%s.2 = %q, want the line before that", path, got)
	}
	mustNotExist(t, path+".3", "Keep=2 must bound the tree at two rotated generations")
}

// TestKeepZeroRetainsNoGenerations pins that Keep is taken literally.
//
// This was a real bug: New treated Keep==0 as "unset, use DefaultKeep", so an
// operator who set CRONOMICON_LOG_FILE_KEEP=0 — a value config explicitly accepts,
// and the only value that means "no rotated copies" — silently got five. A knob
// that quietly ignores what you set is precisely the failure this change set
// exists to eliminate, so it must not be reintroduced. The default now lives in
// the config layer, which is the only place that can distinguish unset from 0.
func TestKeepZeroRetainsNoGenerations(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path, MaxBytes: 10, Keep: 0})
	t.Cleanup(func() { _ = w.Close() })

	for _, line := range []string{"11111111\n", "22222222\n", "33333333\n"} {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}

	if got := readFile(t, path); got != "33333333\n" {
		t.Errorf("live file = %q, want only the newest line (rotate truncates in place)", got)
	}
	mustNotExist(t, path+".1", "Keep=0 means keep NO generations, not DefaultKeep")
	// The tee is unaffected by the retention choice — it always sees everything.
	if got := tee.String(); got != "11111111\n22222222\n33333333\n" {
		t.Errorf("tee = %q, want every line regardless of file rotation", got)
	}
}

// TestSetPathRetriesAPathWhoseOpenFailed covers the recovery route for a
// misconfigured directory. SetPath assigns w.path before attempting the open, so
// after a failure the Writer reports a path it does not hold. The same-path
// early return then made re-sending that path a no-op — meaning an operator who
// fixed the underlying permissions and re-saved the unchanged setting had no way
// to force a re-attempt short of restarting the process. Re-setting is now a
// no-op only when the path is actually open.
func TestSetPathRetriesAPathWhoseOpenFailed(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked")
	// A regular file where the log's PARENT directory needs to be: MkdirAll fails.
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "cronomicon.log")

	tee := &syncBuffer{}
	w := New(tee, Options{MaxBytes: 1 << 20, Keep: 1})
	t.Cleanup(func() { _ = w.Close() })

	if err := w.SetPath(path); err == nil {
		t.Fatal("SetPath onto an unopenable path should report the failure")
	}
	// Clear the obstruction, as an operator fixing the mount would.
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := w.SetPath(path); err != nil {
		t.Fatalf("re-setting the same path after the fault must retry, got: %v", err)
	}
	if _, err := w.Write([]byte("recovered\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readFile(t, path); got != "recovered\n" {
		t.Errorf("file = %q, want the post-recovery line", got)
	}
}

// TestOversizedWriteIsNotSplitAcrossGenerations: a rotation mid-line would put
// half a JSON log record in one file and half in another, making BOTH
// unparseable by any log shipper. A single write larger than the cap is
// therefore written whole, accepting a temporary overshoot of the ceiling — the
// cap is a size target, log-line integrity is not negotiable.
func TestOversizedWriteIsNotSplitAcrossGenerations(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path, MaxBytes: 10, Keep: 2})
	t.Cleanup(func() { _ = w.Close() })

	big := strings.Repeat("A", 50) + "\n"
	bigger := strings.Repeat("B", 50) + "\n"
	if _, err := w.Write([]byte(big)); err != nil {
		t.Fatalf("write big: %v", err)
	}
	if _, err := w.Write([]byte(bigger)); err != nil {
		t.Fatalf("write bigger: %v", err)
	}

	if got := readFile(t, path+".1"); got != big {
		t.Errorf("%s.1 = %q, want the whole oversized line intact (len %d, got %d)", path, got, len(big), len(got))
	}
	if got := readFile(t, path); got != bigger {
		t.Errorf("live file = %q, want the whole second oversized line intact", got)
	}
}

// TestSetPathRepointsAndClosesPreviousFile is the LU-5 mechanism at the writer
// level: an operator moving the log directory must see subsequent output in the
// new place with no restart, and the file they moved away from must be CLOSED —
// a leaked handle would keep the old inode pinned (so freeing the disk by
// deleting it wouldn't) and leak one descriptor per settings save.
func TestSetPathRepointsAndClosesPreviousFile(t *testing.T) {
	tee := &syncBuffer{}
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.log")
	pathB := filepath.Join(dir, "nested", "b.log")

	w := New(tee, Options{Path: pathA})
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Write([]byte("before\n")); err != nil {
		t.Fatalf("write before: %v", err)
	}

	// SetPath also has to create a directory that doesn't exist yet — the
	// operator types a path in the settings form, they don't mkdir it.
	if err := w.SetPath(pathB); err != nil {
		t.Fatalf("SetPath(%s): %v", pathB, err)
	}
	if _, err := w.Write([]byte("after\n")); err != nil {
		t.Fatalf("write after: %v", err)
	}

	if w.Path() != pathB {
		t.Errorf("Path() = %q, want %q", w.Path(), pathB)
	}
	if got := readFile(t, pathA); got != "before\n" {
		t.Errorf("old file = %q, want only the pre-change content", got)
	}
	if got := readFile(t, pathB); got != "after\n" {
		t.Errorf("new file = %q, want only the post-change content", got)
	}
	if got := tee.String(); got != "before\nafter\n" {
		t.Errorf("tee = %q, want both lines — the tee is unaffected by a re-point", got)
	}
	// The previous handle is released, not merely forgotten.
	w.mu.Lock()
	stillOpen := w.f != nil && w.f.Name() == pathA
	w.mu.Unlock()
	if stillOpen {
		t.Errorf("the writer still holds a handle on %s after re-pointing", pathA)
	}
}

// TestSetPathToSamePathDoesNotReopen: the settings handler calls applyLogDir on
// every successful save, including the very common save that changed some other
// field. Churning the handle each time would be pointless work, and any reopen
// that ever became a truncating one (O_TRUNC, or a rotate-on-open) would erase
// the running log on an unrelated settings change. The no-op is asserted both by
// content and by handle identity, since a non-truncating reopen would leave the
// content assertion passing.
func TestSetPathToSamePathDoesNotReopen(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path})
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("write first: %v", err)
	}
	w.mu.Lock()
	before := w.f
	w.mu.Unlock()

	if err := w.SetPath(path); err != nil {
		t.Fatalf("SetPath(same): %v", err)
	}

	w.mu.Lock()
	after := w.f
	w.mu.Unlock()
	if before != after {
		t.Errorf("re-setting the current path reopened the file handle; it must be a no-op")
	}
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if got := readFile(t, path); got != "first\nsecond\n" {
		t.Errorf("file = %q, want both writes — re-setting the same path must not truncate", got)
	}
}

// TestSetPathEmptyDisablesFileLoggingButKeepsTee covers the operator turning
// file logging off. The failure that matters is the one where switching the file
// off also switches stdout off: the container then goes dark and the operator has
// no way back except a restart.
func TestSetPathEmptyDisablesFileLoggingButKeepsTee(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path})
	t.Cleanup(func() { _ = w.Close() })

	if _, err := w.Write([]byte("with file\n")); err != nil {
		t.Fatalf("write with file: %v", err)
	}
	if err := w.SetPath(""); err != nil {
		t.Fatalf("SetPath(\"\"): %v", err)
	}
	if _, err := w.Write([]byte("tee only\n")); err != nil {
		t.Fatalf("write tee only: %v", err)
	}

	if w.Path() != "" {
		t.Errorf("Path() = %q, want empty after disabling", w.Path())
	}
	if got := readFile(t, path); got != "with file\n" {
		t.Errorf("file = %q, want only the pre-disable line", got)
	}
	if got := tee.String(); got != "with file\ntee only\n" {
		t.Errorf("tee = %q, want both lines — disabling the file must never silence stdout", got)
	}
}

// TestReopenAppendsAndCountsExistingBytes simulates a process restart. Opening
// with O_TRUNC would destroy the log of the crash the operator is investigating;
// opening for append but starting the size counter at zero would let the file
// grow to MaxBytes on TOP of whatever was already there, once per restart — a
// service that restart-loops would then blow straight past the (Keep+1)*MaxBytes
// ceiling that makes this safe to enable by default.
func TestReopenAppendsAndCountsExistingBytes(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")

	firstLifetime := strings.Repeat("o", 59) + "\n" // 60 bytes
	w1 := New(tee, Options{Path: path, MaxBytes: 100, Keep: 2})
	if _, err := w1.Write([]byte(firstLifetime)); err != nil {
		t.Fatalf("write lifetime 1: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("close lifetime 1: %v", err)
	}

	// Second lifetime, same path.
	w2 := New(tee, Options{Path: path, MaxBytes: 100, Keep: 2})
	t.Cleanup(func() { _ = w2.Close() })

	short := strings.Repeat("n", 9) + "\n" // 10 bytes; 60+10 = 70 ≤ 100 ⇒ no rotation
	if _, err := w2.Write([]byte(short)); err != nil {
		t.Fatalf("write short: %v", err)
	}
	if got := readFile(t, path); got != firstLifetime+short {
		t.Errorf("file = %q, want the previous lifetime's content followed by the new line (reopen must APPEND)", got)
	}
	mustNotExist(t, path+".1", "an append below the cap must not rotate")

	// 70 + 50 > 100 ⇒ rotate. This only happens if the reopen counted the 60
	// pre-existing bytes; a zero-initialised counter would see 10+50 and not rotate.
	crossing := strings.Repeat("x", 49) + "\n" // 50 bytes
	if _, err := w2.Write([]byte(crossing)); err != nil {
		t.Fatalf("write crossing: %v", err)
	}
	if got := readFile(t, path+".1"); got != firstLifetime+short {
		t.Errorf("%s.1 = %q, want the rotated-out content — the reopen did not account for pre-existing size", path, got)
	}
	if got := readFile(t, path); got != crossing {
		t.Errorf("live file = %q, want only the post-rotation write", got)
	}
}

// TestUnopenablePathDegradesToTeeOnly is the availability property that makes
// this safe to default on: a read-only volume, a bad path in the settings form,
// or a missing mount must not panic, must not take stdout down with it, and must
// not stop New from returning a usable Writer — losing the file copy can never be
// the reason the process fails to start.
func TestUnopenablePathDegradesToTeeOnly(t *testing.T) {
	dir := t.TempDir()
	// A path whose parent is an existing FILE: MkdirAll fails with ENOTDIR.
	occupied := filepath.Join(dir, "occupied")
	if err := os.WriteFile(occupied, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("seed occupied path: %v", err)
	}
	bad := filepath.Join(occupied, "cronomicon.log")

	tee := &syncBuffer{}
	var w *Writer
	stderr := captureStderr(t, func() {
		w = New(tee, Options{Path: bad})
		if _, err := w.Write([]byte("still logging\n")); err != nil {
			t.Errorf("Write on a degraded writer: %v", err)
		}
	})
	t.Cleanup(func() { _ = w.Close() })

	if got := tee.String(); got != "still logging\n" {
		t.Errorf("tee = %q, want the line — a file-side failure must never cost stdout", got)
	}
	if !strings.Contains(stderr, "process log file unavailable") {
		t.Errorf("stderr = %q, want the operator-facing notice that the file is unavailable", stderr)
	}
	w.mu.Lock()
	haveFile := w.f != nil
	w.mu.Unlock()
	if haveFile {
		t.Errorf("writer holds a file handle despite the open having failed")
	}
}

// TestNoticeIsRateLimited: the notice reports a broken log file and cannot go
// through slog (slog feeds this writer), so it goes straight to stderr — which
// means an unwritable file would otherwise emit one stderr line per log line and
// become a bigger flood than the thing it is reporting. One notice per interval,
// and a fresh one once the interval has passed so a persistent fault stays
// visible rather than being reported once and forgotten.
func TestNoticeIsRateLimited(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path, MaxBytes: 1 << 20, Keep: 2})
	t.Cleanup(func() { _ = w.Close() })

	clock := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	w.mu.Lock()
	w.now = func() time.Time { return clock }
	// Simulate the handle dying underneath the process (a yanked NFS mount, an
	// fd closed by something else): every subsequent file write now fails while
	// the Writer still believes it has a file.
	_ = w.f.Close()
	w.mu.Unlock()

	burst := captureStderr(t, func() {
		for range 5 {
			if _, err := w.Write([]byte("line\n")); err != nil {
				t.Errorf("Write: %v", err)
			}
		}
	})
	if n := strings.Count(burst, "process log file unavailable"); n != 1 {
		t.Errorf("5 failing writes produced %d stderr notices, want exactly 1:\n%s", n, burst)
	}

	// Past the throttle interval the fault is reported again.
	w.mu.Lock()
	clock = clock.Add(errNoticeInterval + time.Second)
	w.mu.Unlock()
	later := captureStderr(t, func() {
		if _, err := w.Write([]byte("line\n")); err != nil {
			t.Errorf("Write: %v", err)
		}
	})
	if n := strings.Count(later, "process log file unavailable"); n != 1 {
		t.Errorf("after the throttle interval, got %d notices, want 1:\n%s", n, later)
	}

	// Through all of it stdout kept every line.
	if got := strings.Count(tee.String(), "line\n"); got != 6 {
		t.Errorf("tee saw %d lines, want 6 — a dead file handle must not cost stdout a single line", got)
	}
}

// TestConcurrentWritesAndSetPathLoseNoTeeBytes is the -race guard for LU-5. The
// settings handler re-points the writer from a request goroutine while every
// other goroutine in the process is logging through it; the file target may
// legitimately move mid-stream, but the tee is the destination that is always
// supposed to be there, so not one line may be dropped by the swap.
func TestConcurrentWritesAndSetPathLoseNoTeeBytes(t *testing.T) {
	tee := &syncBuffer{}
	dir := t.TempDir()
	w := New(tee, Options{Path: filepath.Join(dir, "a.log"), MaxBytes: 64, Keep: 2})
	t.Cleanup(func() { _ = w.Close() })

	const writers, perWriter = 8, 200
	stop := make(chan struct{})

	var repointer sync.WaitGroup
	repointer.Go(func() {
		paths := []string{
			filepath.Join(dir, "a.log"),
			filepath.Join(dir, "b.log"),
			filepath.Join(dir, "sub", "c.log"),
			"", // file logging off and back on again, mid-stream
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = w.SetPath(paths[i%len(paths)])
		}
	})

	var scribes sync.WaitGroup
	for range writers {
		scribes.Go(func() {
			for range perWriter {
				if _, err := w.Write([]byte("xxxxxxxxxxxxxxxx\n")); err != nil {
					t.Errorf("concurrent Write: %v", err)
					return
				}
			}
		})
	}
	scribes.Wait()
	close(stop)
	repointer.Wait()

	if got := strings.Count(tee.String(), "\n"); got != writers*perWriter {
		t.Errorf("tee received %d lines, want %d — a concurrent re-point dropped output", got, writers*perWriter)
	}
}

// TestWriteRetriesAOpenAfterTheIntervalAndSelfHeals covers the recovery path.
// Without it a single transient fault — a mount that came back a second later, a
// disk that was briefly full — would disable file logging for the entire life of
// the process, which is the worst possible failure for a file whose only job is
// to be there when something breaks. The retry is throttled on the same interval
// as the stderr notice, so a path that is permanently gone costs one open() a
// minute rather than one per log line.
func TestWriteRetriesAOpenAfterTheIntervalAndSelfHeals(t *testing.T) {
	dir := t.TempDir()
	occupied := filepath.Join(dir, "occupied")
	if err := os.WriteFile(occupied, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("seed occupied path: %v", err)
	}
	target := filepath.Join(occupied, "cronomicon.log")

	tee := &syncBuffer{}
	w := New(tee, Options{MaxBytes: 1 << 20, Keep: 2})
	t.Cleanup(func() { _ = w.Close() })

	clock := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	w.mu.Lock()
	w.now = func() time.Time { return clock }
	w.mu.Unlock()

	if err := w.SetPath(target); err == nil {
		t.Fatalf("SetPath(%s) succeeded, want an open failure (parent is a file)", target)
	}

	// First write after the failure: the retry fires immediately (nothing has
	// been attempted yet) and fails again, because the fault is still there.
	_ = captureStderr(t, func() {
		if _, err := w.Write([]byte("one\n")); err != nil {
			t.Errorf("Write: %v", err)
		}
	})
	w.mu.Lock()
	firstAttempt := w.lastRetry
	w.mu.Unlock()
	if !firstAttempt.Equal(clock) {
		t.Fatalf("lastRetry = %v, want the first write to have attempted a reopen at %v", firstAttempt, clock)
	}

	// The operator fixes the underlying problem…
	if err := os.Remove(occupied); err != nil {
		t.Fatalf("clear the occupying file: %v", err)
	}

	// …but within the throttle interval nothing is retried: no reopen attempt is
	// recorded and no file appears, so a permanently-broken path cannot turn into
	// an open() syscall per log line.
	_ = captureStderr(t, func() {
		for range 3 {
			if _, err := w.Write([]byte("throttled\n")); err != nil {
				t.Errorf("Write: %v", err)
			}
		}
	})
	w.mu.Lock()
	stillFirst := w.lastRetry.Equal(firstAttempt)
	w.mu.Unlock()
	if !stillFirst {
		t.Errorf("a reopen was attempted again inside the throttle interval; the retry must be rate-limited")
	}
	mustNotExist(t, target, "no file may appear before the retry interval has elapsed")

	// Past the interval the writer re-establishes the file on its own.
	w.mu.Lock()
	clock = clock.Add(errNoticeInterval + time.Second)
	w.mu.Unlock()
	if _, err := w.Write([]byte("healed\n")); err != nil {
		t.Fatalf("Write after the interval: %v", err)
	}
	if got := readFile(t, target); got != "healed\n" {
		t.Errorf("file = %q, want the post-recovery line — the writer did not self-heal", got)
	}

	// Every line reached stdout throughout, including the ones the file missed.
	if got := tee.String(); got != "one\nthrottled\nthrottled\nthrottled\nhealed\n" {
		t.Errorf("tee = %q, want all five lines", got)
	}
}

// TestWriteAfterCloseDoesNotResurrectTheFile: Close clears the path, which is
// what makes it mean "stop writing to this file" rather than "pause until the
// next line". Now that Write reopens a missing handle on its own, a Close that
// left the path set would have the shutdown path re-create — and keep appending
// to — a file the process has already released, including one an operator or the
// reaper deleted in between.
func TestWriteAfterCloseDoesNotResurrectTheFile(t *testing.T) {
	tee := &syncBuffer{}
	path := filepath.Join(t.TempDir(), "cronomicon.log")
	w := New(tee, Options{Path: path})

	if _, err := w.Write([]byte("before close\n")); err != nil {
		t.Fatalf("write before close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove closed log: %v", err)
	}

	if w.Path() != "" {
		t.Errorf("Path() = %q after Close, want empty", w.Path())
	}
	if _, err := w.Write([]byte("after close\n")); err != nil {
		t.Fatalf("write after close: %v", err)
	}
	mustNotExist(t, path, "a write after Close must not re-create the released log file")
	if got := tee.String(); got != "before close\nafter close\n" {
		t.Errorf("tee = %q, want both lines — Close releases the file, not the tee", got)
	}
}
