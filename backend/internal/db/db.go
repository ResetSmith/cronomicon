// Package db owns the SQLite connection and schema migrations (T1/T4).
//
// One file, one process (T3), so we run with foreign keys enforced and WAL for
// concurrent readers. The pool allows several connections (see maxOpenConns):
// WAL serves reads concurrently, while writes are serialized by SQLite itself
// (BEGIN IMMEDIATE + busy_timeout), so "database is locked" stays a bounded wait
// rather than an error. Revisited only if it ever becomes HA (V2).
package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// maxOpenConns caps the connection pool. It must exceed the deepest nested-query
// fan-out of any single handler (an open rows iterator + its per-row follow-up
// query = 2) with comfortable headroom for concurrent operators. WAL keeps
// concurrent reads cheap; writes serialize at the SQLite layer regardless.
const maxOpenConns = 16

// Open opens (and creates if missing) the SQLite database with production pragmas.
func Open(path string) (*sql.DB, error) {
	// SQLite creates the database file but not its parent directory. Create it so
	// a fresh path "just works" (the prod default /var/lib/amadeus is a mounted
	// volume; a dev box may point CRONOMICON_DB_PATH anywhere). A clear error here
	// beats the opaque "unable to open database file" SQLite returns otherwise.
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory %q (set CRONOMICON_DB_PATH to a writable path): %w", dir, err)
		}
	}

	dsn := "file:" + url.PathEscape(path) + "?" + (url.Values{
		"_journal_mode": {"WAL"},       // concurrent readers + single writer
		"_busy_timeout": {"5000"},      // wait up to 5s rather than erroring on lock
		"_foreign_keys": {"on"},        // enforce FK integrity (S3)
		"_synchronous":  {"NORMAL"},    // WAL-safe durability/perf tradeoff
		"_txlock":       {"immediate"}, // take the write lock at BEGIN to avoid upgrade deadlocks
	}).Encode()

	pool, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Allow concurrent connections. WAL lets readers run concurrently with each
	// other and with a single writer; writes are still effectively serialized by
	// SQLite (BEGIN IMMEDIATE via _txlock takes the write lock up front, and
	// _busy_timeout waits rather than erroring on contention). A pool of 1 is NOT
	// viable here: several list handlers issue a per-row follow-up query while
	// their outer result set is still streaming, which would self-deadlock on a
	// single shared connection (the nested query waits for the connection the
	// open rows iterator holds). Capping above that nesting depth avoids it.
	pool.SetMaxOpenConns(maxOpenConns)
	pool.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	return pool, nil
}
