// Package logsink provides the writer that sits behind the process logger: a
// tee to stdout plus an optional size-rotated file whose path can be changed
// while the process is running (LU-4).
//
// Why a writer swap rather than rebuilding the logger. The *slog.Logger is
// stored on 12 structs across as many packages, with ~193 uses of s.log in
// internal/api alone — re-wiring them on a settings change is not tractable.
// Building the one slog handler over a mutable writer instead reaches 100% of
// server logging with no plumbing, which is also why SetPath can re-point the
// destination live (LU-5) without a restart.
//
// Stdout is always a tee, never a fallback. Containerised deployments have
// journald/docker collecting stdout today; silently moving output into a file
// they don't read would be a regression, so the file is strictly additive.
//
// # Not redacted
//
// Run logs pass through runner.NewRedactor with a documented fail-closed path.
// This writer has no redactor: anything a caller puts in a log line — including
// a credential that found its way into an error string — is written verbatim.
// That was already true of stdout; persisting it to disk raises the stakes and
// is recorded as accepted residual risk in deploy/security-review.md.
package logsink

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// DefaultMaxBytes is the size at which the live file is rotated.
	DefaultMaxBytes int64 = 64 << 20 // 64 MiB
	// DefaultKeep is how many rotated generations are retained when the operator
	// has expressed no preference. Total on-disk cost is bounded by
	// (Keep+1) * MaxBytes, which is what makes it safe to turn this on by
	// default now that LU-1 reaps the rest of the tree.
	//
	// Applied by the config layer, not by New — see Options.Keep.
	DefaultKeep = 5

	// dirMode/fileMode match the modes the run-log writers already use.
	dirMode  os.FileMode = 0o750
	fileMode os.FileMode = 0o640

	// errNoticeInterval rate-limits the stderr notice below.
	errNoticeInterval = time.Minute
)

// Options configures a Writer.
type Options struct {
	// Path is the live log file. Empty means file logging is off and the Writer
	// is a plain pass-through to the tee.
	Path string
	// MaxBytes is the rotation threshold. Zero or negative takes DefaultMaxBytes,
	// because a zero size cap has no coherent meaning — it would rotate on every
	// write.
	MaxBytes int64
	// Keep is the number of rotated generations retained, and is taken
	// LITERALLY: zero means keep none (the live file is truncated on rotate),
	// not "use the default". Zero is a meaningful answer here, unlike MaxBytes,
	// and an operator who sets AMADEUS_LOG_FILE_KEEP=0 must not silently get
	// five — a knob that quietly ignores what you set is the exact trap this
	// whole change set exists to close. Negative is clamped to zero.
	//
	// The default lives in the config layer (config.LogFileKeep), which is the
	// only place that can tell "unset" from "set to 0".
	Keep int
}

// Writer is an io.Writer that fans out to a tee (stdout) and an optional
// rotated file.
//
// Safe for concurrent use provided the tee is: Write hands the tee its bytes
// BEFORE taking the Writer's own mutex, deliberately, so the guaranteed
// destination never queues behind file I/O. os.Stdout satisfies this (one
// syscall); a bare bytes.Buffer does not.
type Writer struct {
	tee io.Writer

	mu       sync.Mutex
	f        *os.File
	path     string
	size     int64
	maxBytes int64
	keep     int

	// lastNotice throttles the stderr notice so a persistently unwritable log
	// file cannot itself become the flood. lastRetry does the same for the
	// reopen attempt in Write, so a permanently-gone path costs one open() per
	// interval rather than one per log line.
	lastNotice time.Time
	lastRetry  time.Time
	// now is a seam for tests.
	now func() time.Time
}

// New returns a Writer teeing to tee. A file-open failure is reported and the
// Writer degrades to tee-only rather than failing construction — losing the
// file copy must never be the reason the process can't start.
func New(tee io.Writer, opts Options) *Writer {
	if tee == nil {
		tee = io.Discard
	}
	w := &Writer{
		tee:      tee,
		maxBytes: opts.MaxBytes,
		keep:     opts.Keep,
		now:      time.Now,
	}
	if w.maxBytes <= 0 {
		w.maxBytes = DefaultMaxBytes
	}
	if w.keep < 0 {
		w.keep = 0
	}
	if opts.Path != "" {
		if err := w.SetPath(opts.Path); err != nil {
			w.mu.Lock()
			w.notice(err)
			w.mu.Unlock()
		}
	}
	return w
}

// Path returns the live file path, or "" when file logging is off.
func (w *Writer) Path() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.path
}

// SetPath re-points the file target, closing the previous file. An empty path
// turns file logging off. Re-setting the current path is a no-op, so a settings
// save that didn't actually change the directory doesn't churn the handle.
//
// On error the previous file is already closed and the Writer is left tee-only;
// that is deliberate. The alternative — keeping the old handle — would leave the
// process writing to a location the operator has just said they don't want,
// which is worse than a gap the error tells them about.
func (w *Writer) SetPath(path string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Re-setting the current path is a no-op ONLY when that path is actually
	// open — otherwise a settings save that re-sends an unchanged directory
	// would be unable to recover a file whose open previously failed, leaving
	// the operator with no way to force a retry short of a restart.
	if path == w.path && (path == "" || w.f != nil) {
		return nil
	}
	w.closeLocked()
	w.path = path
	if path == "" {
		return nil
	}
	return w.openLocked()
}

// Close releases the file. The Writer stays usable as a tee afterwards.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	err := w.closeLocked()
	w.path = ""
	return err
}

func (w *Writer) closeLocked() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f, w.size = nil, 0
	return err
}

// openLocked opens (or creates) w.path for append and records its current size
// so rotation accounts for content written by a previous process lifetime.
func (w *Writer) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(w.path), dirMode); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", w.path, err)
	}
	size := int64(0)
	if st, serr := f.Stat(); serr == nil {
		size = st.Size()
	}
	w.f, w.size = f, size
	return nil
}

// Write tees to stdout, then appends to the file, rotating first if this write
// would take it past the size cap.
//
// The tee's result is what's returned. A file-side failure is reported to stderr
// (throttled) and otherwise swallowed, because slog discards the error a handler
// returns from Write — surfacing it as an error would make it vanish, and
// returning a short count would make slog think stdout failed too.
func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.tee.Write(p)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		// File logging is configured but the handle is gone — a failed open, or a
		// rotation that closed the old file and could not open the new one. Retry,
		// throttled, so a transient problem (a full disk, a briefly unavailable
		// mount) self-heals instead of silently disabling the file for the rest of
		// the process's life. That failure mode is especially bad here: the whole
		// point of the file is being there when something goes wrong.
		if w.path == "" || !w.retryDue() {
			return n, err
		}
		if oerr := w.openLocked(); oerr != nil {
			w.notice(oerr)
			return n, err
		}
	}
	// Rotate on the boundary rather than after crossing it, so the cap is an
	// actual ceiling. A single write larger than the cap still goes to one file
	// — splitting a log line across generations would corrupt it.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if rerr := w.rotateLocked(); rerr != nil {
			w.notice(rerr)
		}
	}
	if w.f == nil {
		return n, err
	}
	fn, ferr := w.f.Write(p)
	w.size += int64(fn)
	if ferr != nil {
		w.notice(ferr)
	}
	return n, err
}

// rotateLocked shifts path.N-1 → path.N (dropping the oldest), moves the live
// file to path.1 and reopens.
//
// Rotated generations are named path.1 … path.N — deliberately NOT ending in
// ".log", so the LU-1 run-log reaper skips them. Process-log retention is this
// keep count, not the run-log retention window; the two answer different
// questions and shouldn't share a knob.
func (w *Writer) rotateLocked() error {
	if err := w.closeLocked(); err != nil {
		return err
	}
	if w.keep == 0 {
		// No generations retained: truncate in place on reopen.
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return w.openLocked()
	}
	// Drop the oldest, then shift the rest down. Descending order matters — the
	// other direction would overwrite each generation with its successor.
	if err := os.Remove(w.gen(w.keep)); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := w.keep - 1; i >= 1; i-- {
		if err := os.Rename(w.gen(i), w.gen(i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(w.path, w.gen(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return w.openLocked()
}

func (w *Writer) gen(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// retryDue reports whether enough time has passed to attempt another open, and
// records the attempt. Must be called with w.mu held.
func (w *Writer) retryDue() bool {
	now := w.now()
	if !w.lastRetry.IsZero() && now.Sub(w.lastRetry) < errNoticeInterval {
		return false
	}
	w.lastRetry = now
	return true
}

// notice reports a file-side problem to stderr, at most once per interval.
// It cannot log through slog: slog is what feeds this writer.
//
// Must be called with w.mu held — it mutates lastNotice.
func (w *Writer) notice(err error) {
	now := w.now()
	if !w.lastNotice.IsZero() && now.Sub(w.lastNotice) < errNoticeInterval {
		return
	}
	w.lastNotice = now
	fmt.Fprintf(os.Stderr, "amadeus: process log file unavailable (stdout unaffected): %v\n", err)
}
