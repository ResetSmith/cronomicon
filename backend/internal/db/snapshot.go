package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The nightly snapshot's files (A4; 2.5.1 SG1). In the backup directory:
//
//	cronomicon-YYYYMMDD.db                       unencrypted (no backup key set)
//	cronomicon-YYYYMMDD.db.age                   encrypted to the backup keys
//	cronomicon-YYYYMMDD.db.age.recipients        the keys' fingerprints
//
// The plaintext of an encrypted snapshot exists only in the 0700 staging
// directory beneath, for as long as the encryption takes, and is removed on
// every path (SG-6). The names are spelled here and in internal/backup
// (SealedSuffix, RecipientsSuffix), which this package must not import.
const (
	defaultBackupDir   = "/var/lib/cronomicon/backups"
	stagingDirName     = ".staging"
	snapshotPrefix     = "cronomicon-"
	plainSuffix        = ".db"
	sealedSuffix       = ".db.age"
	recipientsSuffix   = ".db.age.recipients"
	snapshotKeepDays   = 7
	lastSnapshotKey    = "backupLastSnapshot"
	snapshotDateLength = len("20060102")
)

func backupDir(p RetentionPolicy) string {
	if p.BackupDir != "" {
		return p.BackupDir
	}
	return defaultBackupDir
}

// clearStaging removes whatever a crash left in the staging directory: a
// plaintext snapshot must not outlive the sweep that wrote it. Called before
// every snapshot and once when retention starts.
func clearStaging(dir string, log *slog.Logger) {
	staging := filepath.Join(dir, stagingDirName)
	entries, err := os.ReadDir(staging)
	if err != nil {
		return
	}
	for _, e := range entries {
		if rerr := os.RemoveAll(filepath.Join(staging, e.Name())); rerr != nil {
			log.Warn("retention: could not remove a leftover staged snapshot", "file", e.Name(), "error", rerr)
		} else {
			log.Info("retention: removed a leftover staged snapshot", "file", e.Name())
		}
	}
}

// writeSnapshot writes today's snapshot and returns its path, and the
// fingerprints it was sealed to (none: not encrypted).
func writeSnapshot(ctx context.Context, pool *sql.DB, p RetentionPolicy, dir string, log *slog.Logger) (string, []string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", nil, fmt.Errorf("create backup dir %s: %w", dir, err)
	}
	staging := filepath.Join(dir, stagingDirName)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return "", nil, fmt.Errorf("create staging dir %s: %w", staging, err)
	}
	// An existing directory keeps its mode through MkdirAll.
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", nil, fmt.Errorf("restrict staging dir %s: %w", staging, err)
	}
	clearStaging(dir, log)

	name := snapshotPrefix + nowUTC().Format("20060102")
	base := filepath.Join(dir, name)
	plain := filepath.Join(staging, name+plainSuffix)
	// PP-M2: SQLite's VACUUM INTO refuses to overwrite an existing file, so a
	// second sweep on the same UTC day (boot catch-up, clock change, manual run)
	// would error AFTER the prune already ran — pruning rows but producing no
	// backup. Remove any prior same-day snapshot, in either form, first so the
	// write is idempotent (and a day's directory never holds both forms).
	for _, f := range []string{base + plainSuffix, base + sealedSuffix, base + recipientsSuffix} {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("remove stale snapshot %s: %w", f, err)
		}
	}
	defer os.Remove(plain) // every path: a sealed snapshot's plaintext never stays

	if _, err := pool.ExecContext(ctx, "VACUUM INTO ?", plain); err != nil {
		return "", nil, fmt.Errorf("vacuum into %s: %w", plain, err)
	}
	if err := os.Chmod(plain, 0o600); err != nil {
		return "", nil, fmt.Errorf("restrict snapshot %s: %w", plain, err)
	}
	if p.Seal != nil {
		fingerprints, err := p.Seal(ctx, plain, base+sealedSuffix)
		if err != nil {
			_ = os.Remove(base + sealedSuffix)
			_ = os.Remove(base + recipientsSuffix)
			return "", nil, fmt.Errorf("encrypt snapshot: %w", err)
		}
		if len(fingerprints) > 0 {
			return base + sealedSuffix, fingerprints, nil
		}
	}
	if err := os.Rename(plain, base+plainSuffix); err != nil {
		return "", nil, fmt.Errorf("move snapshot into %s: %w", dir, err)
	}
	return base + plainSuffix, nil, nil
}

// snapshotDate is the YYYYMMDD of a snapshot file's name, or "" for a file
// that is not one (only those three names are ever touched).
func snapshotDate(name string) string {
	if !strings.HasPrefix(name, snapshotPrefix) {
		return ""
	}
	rest := name[len(snapshotPrefix):]
	if len(rest) <= snapshotDateLength || !allDigits(rest[:snapshotDateLength]) {
		return ""
	}
	switch rest[snapshotDateLength:] {
	case plainSuffix, sealedSuffix, recipientsSuffix:
		return rest[:snapshotDateLength]
	}
	return ""
}

// pruneSnapshots removes local snapshot files older than 7 days so they don't
// accumulate on the volume indefinitely (PP-M5), in both forms.
func pruneSnapshots(dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := nowUTC().AddDate(0, 0, -snapshotKeepDays).Format("20060102")
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if d := snapshotDate(name); d != "" && d < cutoff {
			if rerr := os.Remove(filepath.Join(dir, name)); rerr != nil {
				log.Warn("retention: failed to remove old snapshot", "file", name, "error", rerr)
			} else {
				log.Info("retention: removed old snapshot", "file", name)
			}
		}
	}
}

// LastSnapshot is what the last successful backup was, served by
// GET /settings/backup: its file, when, and the fingerprints it was sealed to
// (empty: unencrypted).
type LastSnapshot struct {
	File       string   `json:"file"`
	At         string   `json:"at"`
	Recipients []string `json:"recipients"`
}

func persistLastSnapshot(ctx context.Context, pool *sql.DB, s LastSnapshot) {
	if s.Recipients == nil {
		s.Recipients = []string{}
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	_, _ = pool.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, lastSnapshotKey, string(b))
}

// ReadLastSnapshot returns the last successful backup, or nil when none has
// been recorded (an installation from before 2.5.1 records it from its first
// backup on).
func ReadLastSnapshot(ctx context.Context, pool *sql.DB) (*LastSnapshot, error) {
	var v sql.NullString
	err := pool.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, lastSnapshotKey).Scan(&v)
	if err == sql.ErrNoRows || (err == nil && !v.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s LastSnapshot
	if err := json.Unmarshal([]byte(v.String), &s); err != nil {
		return nil, fmt.Errorf("read %s: %w", lastSnapshotKey, err)
	}
	return &s, nil
}
