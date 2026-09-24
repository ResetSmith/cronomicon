package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/ResetSmith/cronomicon/internal/backup"
	"github.com/ResetSmith/cronomicon/internal/config"
)

// runRestore implements `amadeus restore` (FU-3 Phase C): fetch a database
// snapshot from the configured S3 backup bucket and swap it into place, then
// verify it with PRAGMA integrity_check. It replaces the by-hand `aws s3 cp` +
// file-swap steps in backend/deploy/backup-restore.md, grounding the tooling in
// that runbook.
//
//	amadeus restore --list                 # show available snapshots
//	amadeus restore                        # restore the latest over AMADEUS_DB_PATH
//	amadeus restore --from amadeus-20260722.db --db /var/lib/amadeus/amadeus.db
//
// It reads the same AMADEUS_BACKUP_S3_* / AMADEUS_DB_PATH env the server uses.
// The server must be STOPPED first — the swap replaces the live .db and its
// -wal/-shm sidecars. A best-effort write-lock probe refuses an obviously-active
// DB, but is not a substitute for stopping the service. The KEK/OIDC keys are
// supplied out-of-band and are unaffected by the restore.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	from := fs.String("from", "", "snapshot object key or filename to restore (default: the latest snapshot)")
	dbPath := fs.String("db", "", "target DB path (default: AMADEUS_DB_PATH from config)")
	list := fs.Bool("list", false, "list available snapshots and exit")
	yes := fs.Bool("yes", false, "skip the interactive confirmation prompt")
	downloadTo := fs.String("download-to", "", "keep the downloaded snapshot at this path (default: a temp file next to the target, removed after swap)")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall timeout for the download")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore: load config:", err)
		return 1
	}
	// The restore path is a short-lived CLI invocation, not the server: it writes
	// to stdout only, so the sink is closed immediately and never given a file.
	log, sink := newLogger(cfg)
	defer sink.Close()

	dl, err := backup.NewDownloader(cfg, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if *list {
		snaps, err := dl.List(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore:", err)
			return 1
		}
		if len(snaps) == 0 {
			fmt.Printf("no snapshots found in bucket %q\n", dl.Bucket())
			return 0
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tSIZE\tLAST MODIFIED")
		for _, s := range snaps {
			fmt.Fprintf(tw, "%s\t%d\t%s\n", s.Key, s.Size, s.LastModified.UTC().Format(time.RFC3339))
		}
		tw.Flush()
		return 0
	}

	key := *from
	if key == "" {
		key, err = dl.LatestKey(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "restore:", err)
			return 1
		}
	}

	target := *dbPath
	if target == "" {
		target = cfg.DBPath
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "restore: no target DB path (set AMADEUS_DB_PATH or pass --db)")
		return 1
	}

	// Best-effort guard: refuse if the target DB is actively being written (a
	// running server). Not a full liveness check — the operator must still stop
	// amadeus first (the swap replaces the WAL). See dbLooksInUse.
	if why := dbLooksInUse(target); why != "" {
		fmt.Fprintf(os.Stderr, "restore: refusing — target DB %q looks in use (%s). Stop amadeus first.\n", target, why)
		return 1
	}

	if !*yes {
		fmt.Printf("Restore snapshot %q OVER %q?\n", key, target)
		fmt.Printf("  The existing DB and its -wal/-shm sidecars will be REPLACED. Ensure amadeus is stopped.\n")
		fmt.Print("Continue? [y/N] ")
		var resp string
		_, _ = fmt.Scanln(&resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Println("aborted")
			return 1
		}
	}

	dlPath := *downloadTo
	tempDownload := dlPath == ""
	if tempDownload {
		dlPath = target + ".restore-download"
	}
	fmt.Printf("downloading %q → %q …\n", key, dlPath)
	if err := dl.Download(ctx, key, dlPath); err != nil {
		fmt.Fprintln(os.Stderr, "restore:", err)
		return 1
	}

	// Verify the DOWNLOADED snapshot BEFORE touching the live DB. A corrupt or
	// truncated snapshot (e.g. a partial prior upload) must not destroy the
	// working database and then fail — the live DB is the only copy. If the
	// download is bad, abort here with the original untouched.
	if err := verifyRestoredDB(dlPath); err != nil {
		fmt.Fprintf(os.Stderr, "restore: downloaded snapshot failed verification — live DB left untouched: %v\n", err)
		if tempDownload {
			_ = os.Remove(dlPath)
		}
		return 1
	}

	// Swap: remove the live DB + WAL/SHM sidecars, then install the snapshot. The
	// snapshot is copied (not moved) so a `--download-to` path is preserved
	// regardless of filesystem; the temp download is removed after.
	for _, suffix := range []string{"-wal", "-shm", ""} {
		if err := os.Remove(target + suffix); err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "restore: remove %s: %v\n", target+suffix, err)
			return 1
		}
	}
	if err := copyFileContents(dlPath, target); err != nil {
		fmt.Fprintf(os.Stderr, "restore: install snapshot: %v\n", err)
		return 1
	}
	if tempDownload {
		_ = os.Remove(dlPath)
	}

	// Re-verify the installed copy (cheap; catches a swap-time write error).
	if err := verifyRestoredDB(target); err != nil {
		fmt.Fprintf(os.Stderr, "restore: WARNING — snapshot installed but integrity check failed: %v\n", err)
		return 1
	}

	fmt.Printf("restore: OK — %q restored from %q; PRAGMA integrity_check passed.\n", target, key)
	fmt.Println("Start amadeus to apply migrations, then re-supply the KEK/OIDC keys out-of-band.")
	return 0
}

// dbLooksInUse is a best-effort probe: it opens the target with a short busy
// timeout and tries to take the write lock (BEGIN IMMEDIATE). If a writer is
// active (a running server mid-write, or another restore) the lock times out and
// we report it. An empty return means "no active writer detected" — NOT a
// guarantee the server is stopped. Returns "" when the DB does not yet exist.
func dbLooksInUse(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return ""
	}
	dsn := "file:" + url.PathEscape(path) + "?" + url.Values{
		"_busy_timeout": {"500"},
		"_journal_mode": {"WAL"},
	}.Encode()
	pool, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return "" // can't tell; let the confirmation prompt cover it
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return ""
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "lock") || strings.Contains(strings.ToLower(err.Error()), "busy") {
			return "database is write-locked"
		}
		return ""
	}
	_, _ = conn.ExecContext(ctx, "ROLLBACK")
	return ""
}

// verifyRestoredDB opens the restored file read-only and runs the runbook's
// post-restore checks: PRAGMA integrity_check plus a sanity row count.
func verifyRestoredDB(path string) error {
	dsn := "file:" + url.PathEscape(path) + "?" + url.Values{
		"mode":          {"ro"},
		"_busy_timeout": {"2000"},
	}.Encode()
	pool, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("open restored db: %w", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result string
	if err := pool.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return fmt.Errorf("integrity_check: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(result)) != "ok" {
		return fmt.Errorf("integrity_check returned %q", result)
	}
	// Sanity: the runs table should be queryable (schema present). Absent rows is
	// fine; an error means the snapshot is not a usable amadeus DB.
	var n int
	if err := pool.QueryRowContext(ctx, "SELECT count(*) FROM runs").Scan(&n); err != nil {
		return fmt.Errorf("row-count sanity (SELECT FROM runs): %w", err)
	}
	fmt.Printf("verify: integrity_check=ok, runs=%d\n", n)
	return nil
}

// copyFileContents copies src to dst (used when os.Rename crosses filesystems).
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
