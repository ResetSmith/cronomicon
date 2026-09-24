package redactdict

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// TTL is the floor on how stale a dictionary may be before a read rebuilds it
// even with no invalidation — the safety net for a source writer that forgot
// to call secrets.RedactionSourceChanged (AM-Q4). The conformance test is the
// primary guard; this is the backstop.
const TTL = 5 * time.Minute

// ErrUndecryptable reports a build that finished but could not open some of the
// encrypted values it found — a KEK version not configured, no KEK at all, or
// bad ciphertext. The dictionary returned alongside it is PARTIAL: everything
// it holds is right, but values exist that it cannot mask.
var ErrUndecryptable = errors.New("redaction dictionary is partial: encrypted values could not be decrypted")

// Build reads every global source and returns the dictionary. The sources:
//
//   - secrets.RedactionReport: stored secrets, stored SSH credentials and the
//     encrypted settings columns (one table shared with rewrap-secrets).
//   - env_vars, ALL scopes, multi-line values only — key material referenced
//     by name (sshexec.signerFromEnvVar). Single-line Variables are plaintext,
//     log-safe values (D7) and stay visible.
//
// Vault-sourced secret values are NOT here: they exist only at dispatch time,
// per run, and are the documented residual of the audit-stream masker.
//
// A partial result comes back WITH ErrUndecryptable so the caller can keep it
// (it is strictly better than nothing) while recording the outage. Any other
// error means the database itself failed and no dictionary is returned.
func Build(ctx context.Context, database *sql.DB, cfg *config.Config) (*Dictionary, error) {
	values, undecryptable, err := secrets.RedactionReport(ctx, database, cfg)
	if err != nil {
		return nil, err
	}
	var vals []string
	for _, v := range values {
		vals = AddValue(vals, v)
	}

	rows, err := database.QueryContext(ctx, `SELECT value FROM env_vars`)
	if err != nil {
		return nil, fmt.Errorf("query env_vars for redaction: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if strings.ContainsAny(v, "\r\n") {
			vals = AddValue(vals, v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	d := FromValues(vals)
	if undecryptable > 0 {
		return d, fmt.Errorf("%w: %d value(s)", ErrUndecryptable, undecryptable)
	}
	return d, nil
}

// Store holds the current dictionary and rebuilds it lazily.
//
// Readers never block on a rebuild: Get returns whatever is installed and, if
// that is stale, triggers a rebuild that at most one goroutine performs at a
// time (single-flight). The FIRST reader after Configure does block, because
// there is nothing older to hand back. A failed rebuild keeps the last good
// dictionary installed (AM-Q3(c)) and records the failure for the installer to
// act on; a partial rebuild (ErrUndecryptable) installs the partial dictionary
// — it is right about everything it holds — and records the outage the same way.
type Store struct {
	db  *sql.DB
	cfg *config.Config

	current atomic.Pointer[entry]
	stale   atomic.Bool
	gen     atomic.Uint64 // bumped on every Invalidate; lets a caller tell outages apart

	mu       sync.Mutex // serialises rebuilds
	building atomic.Bool
}

type entry struct {
	dict    *Dictionary
	builtAt time.Time
	// err is the rebuild outcome that produced (or failed to replace) dict:
	// nil for a complete build, ErrUndecryptable for a partial one, anything
	// else when the previous dict was kept because this build failed.
	err error
	gen uint64
}

// NewStore returns an unbuilt store. Nothing is read until the first Get.
func NewStore(database *sql.DB, cfg *config.Config) *Store {
	s := &Store{db: database, cfg: cfg}
	s.stale.Store(true)
	return s
}

// Invalidate marks the dictionary stale; the next Get rebuilds it. Cheap and
// safe to call from any goroutine, any number of times.
func (s *Store) Invalidate() {
	s.gen.Add(1)
	s.stale.Store(true)
}

// Get returns the dictionary to mask with right now. It is never nil after the
// first successful or partial build; before that it is nil (mask nothing) and
// the error says why. The second return is the standing outage, if any: nil
// when the installed dictionary is complete.
func (s *Store) Get(ctx context.Context) (*Dictionary, error) {
	cur := s.current.Load()
	needs := s.stale.Load() || cur == nil || time.Since(cur.builtAt) > TTL
	if !needs {
		return cur.dict, cur.err
	}
	if cur == nil {
		// Nothing to hand back: the first build blocks every caller until one
		// goroutine finishes it.
		s.rebuild(ctx)
		if cur = s.current.Load(); cur == nil {
			return nil, errors.New("redaction dictionary has never been built")
		}
		return cur.dict, cur.err
	}
	// Something is installed: serve it, and let ONE caller do the rebuild
	// inline. Concurrent callers see building=true and take the old entry.
	if s.building.CompareAndSwap(false, true) {
		s.rebuild(ctx)
		s.building.Store(false)
		cur = s.current.Load()
	}
	return cur.dict, cur.err
}

// Current returns the installed entry without triggering a rebuild — what
// status surfaces and tests read.
func (s *Store) Current() (dict *Dictionary, builtAt time.Time, err error) {
	cur := s.current.Load()
	if cur == nil {
		return nil, time.Time{}, errors.New("redaction dictionary has never been built")
	}
	return cur.dict, cur.builtAt, cur.err
}

// Generation is the invalidation counter — an installer keys its
// once-per-outage reporting on it.
func (s *Store) Generation() uint64 { return s.gen.Load() }

func (s *Store) rebuild(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Read the generation BEFORE the build so an Invalidate that lands during
	// it leaves stale=true and the next Get rebuilds again.
	gen := s.gen.Load()
	s.stale.Store(false)

	dict, err := Build(ctx, s.db, s.cfg)
	prev := s.current.Load()
	switch {
	case err == nil:
		s.current.Store(&entry{dict: dict, builtAt: time.Now(), gen: gen})
		metrics.RedactionDictionaryRebuilt("complete", dict.Len())
	case errors.Is(err, ErrUndecryptable):
		// Partial: install it — every value it holds is right — and carry the
		// outage on the entry so the installer can report it.
		s.current.Store(&entry{dict: dict, builtAt: time.Now(), err: err, gen: gen})
		metrics.RedactionDictionaryRebuilt("partial", dict.Len())
	default:
		// Failed: keep the last good dictionary (AM-Q3(c)), refresh nothing but
		// the error, and leave stale so the next Get retries. builtAt is kept
		// so the TTL keeps counting from the good build.
		if prev != nil {
			s.current.Store(&entry{dict: prev.dict, builtAt: prev.builtAt, err: err, gen: gen})
			metrics.RedactionDictionaryRebuilt("failed", prev.dict.Len())
		} else {
			metrics.RedactionDictionaryRebuilt("failed", 0)
		}
		s.stale.Store(true)
	}
}

// Default store wiring — the same shape as auditlog.SetSink: one process-wide
// instance, configured once in main, reachable without threading a dependency
// through every writer.

var std atomic.Pointer[Store]

// Configure installs the process-wide store over the given database and
// config, and registers Invalidate as the secrets change hook so every source
// writer refreshes it. Call once at boot, after the database is open.
func Configure(database *sql.DB, cfg *config.Config) *Store {
	s := NewStore(database, cfg)
	std.Store(s)
	secrets.SetRedactionChangeHook(s.Invalidate)
	return s
}

// MaskString masks s with the process-wide dictionary. Before Configure, or
// before the first build has produced anything, it returns s unchanged — the
// fail-open half of AM-Q3; the installer in main is what reports it.
func MaskString(s string) string {
	st := std.Load()
	if st == nil || s == "" {
		return s
	}
	d, _ := st.Get(context.Background())
	return d.RedactString(s)
}
