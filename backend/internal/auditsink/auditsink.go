// Package auditsink writes the compliance audit stream to audit.log as JSON
// Lines (LU-10).
//
// # Why not slog
//
// This is a schema, not a log format. Consumers live outside this repo — a SIEM,
// a shipper, an auditor's script — and they need stable keys and stable types,
// which is exactly what a general-purpose logging handler does not promise.
// Routing through slog would also put audit records at the mercy of the process
// log's level, format and destination, none of which are the auditor's to
// change. So: one record per line, keys fixed by auditlog.Event, version stamped.
//
// # The database is authoritative
//
// The row and the line are not written in one transaction, so a crash between
// them can leave a row with no line. That asymmetry is deliberate and declared:
// the row goes first, a missing line is recoverable by re-exporting from the
// tables, and the reverse would not be. Treat this file as an export stream, not
// as the record of last resort.
//
// # Rotation
//
// Daily, by UTC date, rather than by size: an auditor asks "what happened on the
// 14th", and a date-named file answers that without reading anything. Generations
// are `audit.log.YYYYMMDD`, deliberately not ending in `.log`, so the run-log
// reaper leaves them alone — audit retention is its own, longer window and must
// not be governed by the run-log one.
package auditsink

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
)

const (
	dirMode  os.FileMode = 0o750
	fileMode os.FileMode = 0o640

	// errNoticeInterval rate-limits the failure report so a persistently
	// unwritable audit file cannot itself become the flood.
	errNoticeInterval = time.Minute
)

// Sink appends audit events to a date-rotated JSON Lines file.
type Sink struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// day is the UTC date (YYYYMMDD) whose records the open file holds.
	day string

	// report surfaces a write failure. Unlike the process-log sink this CAN log
	// through slog — audit records do not feed the process logger, so there is no
	// recursion — and it must: an audit stream that stops silently is worse than
	// one that was never enabled, because the gap looks like an absence of events.
	report     func(error)
	lastNotice time.Time

	now func() time.Time // seam for tests
}

// Options configures a Sink.
type Options struct {
	// Path is the live audit file. Empty means the sink is disabled and Write is
	// a no-op.
	Path string
	// Report is called (rate-limited) when a record cannot be written.
	Report func(error)
}

// New opens the sink. A failure to open is reported and leaves the sink
// disabled rather than failing construction — the audit stream is an export of
// the database, and losing it must not stop the server from serving.
func New(opts Options) *Sink {
	s := &Sink{report: opts.Report, now: time.Now}
	if s.report == nil {
		s.report = func(error) {}
	}
	if opts.Path != "" {
		if err := s.SetPath(opts.Path); err != nil {
			s.mu.Lock()
			s.notice(err)
			s.mu.Unlock()
		}
	}
	return s
}

// Path returns the live file path, or "" when the sink is disabled.
func (s *Sink) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// SetPath re-points the sink, closing any previous file. Empty disables it.
//
// Re-setting a path that is already open is a no-op; re-setting one whose open
// previously FAILED retries, so an operator who fixes a bad directory and saves
// the unchanged setting is not stuck until restart.
func (s *Sink) SetPath(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if path == s.path && (path == "" || s.f != nil) {
		return nil
	}
	s.closeLocked()
	s.path = path
	if path == "" {
		return nil
	}
	return s.openLocked()
}

// Close releases the file.
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.closeLocked()
	s.path = ""
	return err
}

func (s *Sink) closeLocked() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f, s.day = nil, ""
	return err
}

// openLocked opens the live file for append and establishes which UTC day its
// contents belong to.
//
// The day is taken from the file's mtime, not from the clock: a server that was
// down over midnight must rotate yesterday's records out on the way up, rather
// than appending today's to them and producing a file whose name lies about what
// is in it.
func (s *Sink) openLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), dirMode); err != nil {
		return fmt.Errorf("create audit log dir: %w", err)
	}
	day := s.now().UTC().Format("20060102")
	if st, err := os.Stat(s.path); err == nil && st.Size() > 0 {
		if prior := st.ModTime().UTC().Format("20060102"); prior != day {
			if rerr := s.rotateToLocked(prior); rerr != nil {
				return rerr
			}
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open audit log %s: %w", s.path, err)
	}
	s.f, s.day = f, day
	return nil
}

// rotateToLocked renames the live file to its dated generation. A generation
// that already exists is left alone and the live file is appended to it, so a
// same-day double rotation cannot silently discard records.
func (s *Sink) rotateToLocked(day string) error {
	gen := s.path + "." + day
	if _, err := os.Stat(gen); err == nil {
		return s.appendIntoLocked(gen)
	}
	if err := os.Rename(s.path, gen); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rotate audit log: %w", err)
	}
	return nil
}

// appendIntoLocked concatenates the live file onto an existing generation and
// removes it. Only reachable when a generation for that day already exists —
// a restart within the same day after a prior rotation.
func (s *Sink) appendIntoLocked(gen string) error {
	src, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read audit log for merge: %w", err)
	}
	dst, err := os.OpenFile(gen, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		return fmt.Errorf("open audit generation for merge: %w", err)
	}
	defer dst.Close()
	if _, err := dst.Write(src); err != nil {
		return fmt.Errorf("merge audit log: %w", err)
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove merged audit log: %w", err)
	}
	return nil
}

// Write appends one event, rotating first if the UTC date has changed.
//
// Safe to call from any goroutine, and safe to call on a disabled sink.
func (s *Sink) Write(ev auditlog.Event) {
	line, err := json.Marshal(ev)
	if err != nil {
		s.mu.Lock()
		s.notice(fmt.Errorf("marshal audit event: %w", err))
		s.mu.Unlock()
		return
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return
	}
	if s.f == nil {
		// A previous open or rotation failed. Retry on the same throttle as the
		// notice, so a transient fault heals instead of ending the stream for the
		// life of the process.
		if !s.retryDue() {
			return
		}
		if err := s.openLocked(); err != nil {
			s.notice(err)
			return
		}
	}
	if today := s.now().UTC().Format("20060102"); today != s.day {
		if err := s.rollLocked(today); err != nil {
			s.notice(err)
			// Keep going: appending today's record to yesterday's file is far
			// better than dropping it.
		}
	}
	if _, err := s.f.Write(line); err != nil {
		s.notice(fmt.Errorf("write audit event: %w", err))
	}
}

// rollLocked closes the live file, renames it to the day it holds, and reopens.
func (s *Sink) rollLocked(today string) error {
	prior := s.day
	if err := s.closeLocked(); err != nil {
		return err
	}
	if prior != "" {
		if err := s.rotateToLocked(prior); err != nil {
			// Reopen regardless so the stream continues.
			_ = s.openLocked()
			return err
		}
	}
	if err := s.openLocked(); err != nil {
		return err
	}
	s.day = today
	return nil
}

// retryDue reports whether enough time has passed to reattempt an open.
//
// It deliberately does NOT record the attempt: the open it gates is always
// followed by notice() on failure, and notice() is what advances the clock. That
// coupling is the point — a retry that SUCCEEDS leaves lastNotice untouched, so
// the next unrelated failure is reported immediately rather than being swallowed
// by a throttle the recovery had silently armed.
//
// Must be called with s.mu held.
func (s *Sink) retryDue() bool {
	now := s.now()
	if !s.lastNotice.IsZero() && now.Sub(s.lastNotice) < errNoticeInterval {
		return false
	}
	return true
}

// notice reports a failure, at most once per interval. Must hold s.mu.
func (s *Sink) notice(err error) {
	now := s.now()
	if !s.lastNotice.IsZero() && now.Sub(s.lastNotice) < errNoticeInterval {
		return
	}
	s.lastNotice = now
	s.report(err)
}
