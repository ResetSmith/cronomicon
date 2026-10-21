package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/backup"
)

// The backup keys (2.5.1 SG1): the age public keys every nightly snapshot is
// encrypted to. One setting, "backup.recipients", a JSON list, kept by a global
// administrator in Settings → Backups. Encryption is optional (SG-4): an empty
// list means backups are written unencrypted, as before 2.5.1.
//
// The list is public keys only. The private keys that open a backup never
// reach the server, so losing every one of them loses every encrypted backup;
// turning encryption on therefore needs an acknowledgement that says so
// (SG-8), and turning it off a confirmation.

const backupRecipientsKey = "backup.recipients"

var (
	// ErrBackupKeyCustody refuses the first key without the acknowledgement
	// (SG-8). Handlers answer 422 `acknowledgement_required`.
	ErrBackupKeyCustody = fmt.Errorf("%w: turning on encrypted backups needs the acknowledgement that a lost private key makes every encrypted backup unreadable", ErrValidation)
	// ErrBackupUnencryptedConfirm refuses removing the last key without the
	// confirmation. Handlers answer 422 `confirmation_required`.
	ErrBackupUnencryptedConfirm = fmt.Errorf("%w: removing the last key turns encryption off: confirm that later backups are written unencrypted", ErrValidation)
)

// BackupRecipient is one key as the card shows it.
type BackupRecipient struct {
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
}

// BackupKeys is the stored list with who last changed it.
type BackupKeys struct {
	Recipients     []BackupRecipient `json:"recipients"`
	LastModifiedBy string            `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string            `json:"lastModifiedAt,omitempty"`
}

// BackupRecipients reads the keys a backup is sealed to. A read error is an
// error, never an empty list: the sweep would otherwise write an unencrypted
// backup because the database hiccupped.
func BackupRecipients(ctx context.Context, database *sql.DB) ([]string, error) {
	k, err := GetBackupKeys(ctx, database)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(k.Recipients))
	for _, r := range k.Recipients {
		out = append(out, r.Key)
	}
	return out, nil
}

// GetBackupKeys reads the stored list with its fingerprints.
func GetBackupKeys(ctx context.Context, database *sql.DB) (BackupKeys, error) {
	var raw, by, at sql.NullString
	err := database.QueryRowContext(ctx,
		`SELECT value, last_modified_by, last_modified_at FROM settings WHERE key = ?`, backupRecipientsKey).Scan(&raw, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupKeys{Recipients: []BackupRecipient{}}, nil
	}
	if err != nil {
		return BackupKeys{}, fmt.Errorf("read %s: %w", backupRecipientsKey, err)
	}
	var keys []string
	if raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &keys); err != nil {
			return BackupKeys{}, fmt.Errorf("read %s: %w", backupRecipientsKey, err)
		}
	}
	out := BackupKeys{Recipients: make([]BackupRecipient, 0, len(keys)), LastModifiedBy: by.String, LastModifiedAt: at.String}
	for _, k := range keys {
		out.Recipients = append(out.Recipients, BackupRecipient{Key: k, Fingerprint: backup.Fingerprint(k)})
	}
	return out, nil
}

// NormalizeBackupRecipients validates each key and drops repeats, keeping the
// order given. A refusal wraps ErrValidation and backup.ErrInvalidRecipient.
func NormalizeBackupRecipients(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, r := range in {
		k, err := backup.ParseRecipient(r)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrValidation, err)
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out, nil
}

// SetBackupRecipients replaces the list. acknowledged is SG-8's custody
// acknowledgement, required when the stored list is empty and the new one is
// not; confirmUnencrypted is required to empty a non-empty list. The change is
// audited under Global with the fingerprints added and removed.
func SetBackupRecipients(ctx context.Context, database *sql.DB, recipients []string, acknowledged, confirmUnencrypted bool, actor string) (BackupKeys, error) {
	next, err := NormalizeBackupRecipients(recipients)
	if err != nil {
		return BackupKeys{}, err
	}
	prev, err := BackupRecipients(ctx, database)
	if err != nil {
		return BackupKeys{}, err
	}
	switch {
	case len(prev) == 0 && len(next) > 0 && !acknowledged:
		return BackupKeys{}, ErrBackupKeyCustody
	case len(prev) > 0 && len(next) == 0 && !confirmUnencrypted:
		return BackupKeys{}, ErrBackupUnencryptedConfirm
	}
	b, err := json.Marshal(next)
	if err != nil {
		return BackupKeys{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		backupRecipientsKey, string(b), actor, now); err != nil {
		return BackupKeys{}, fmt.Errorf("write %s: %w", backupRecipientsKey, err)
	}
	audit(ctx, database, agencyid.Global, actor, "Settings", "backup-keys-updated", "Backups",
		backupKeysChange(prev, next, acknowledged && len(prev) == 0 && len(next) > 0))
	return GetBackupKeys(ctx, database)
}

// backupKeysChange is the change log's details: fingerprints only, so the
// record names keys the way the card does.
func backupKeysChange(prev, next []string, acknowledged bool) string {
	var added, removed []string
	for _, k := range next {
		if !slices.Contains(prev, k) {
			added = append(added, backup.Fingerprint(k))
		}
	}
	for _, k := range prev {
		if !slices.Contains(next, k) {
			removed = append(removed, backup.Fingerprint(k))
		}
	}
	parts := []string{fmt.Sprintf("keys=%d", len(next))}
	if len(added) > 0 {
		parts = append(parts, "added="+strings.Join(added, ","))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed="+strings.Join(removed, ","))
	}
	if acknowledged {
		parts = append(parts, "acknowledgedKeyCustody=true")
	}
	if len(next) == 0 {
		parts = append(parts, "encryption=off")
	}
	return strings.Join(parts, " ")
}

// SeedBackupRecipientsFromEnv stores CRONOMICON_BACKUP_RECIPIENTS (commas or
// newlines) on the first boot that finds no stored list, and never after. The
// operator who sets the environment holds the private keys; an invalid key
// refuses the seed (nothing stored) so the boot log says why. Reports whether
// it stored anything.
func SeedBackupRecipientsFromEnv(ctx context.Context, database *sql.DB, raw string) (bool, error) {
	if strings.TrimSpace(raw) == "" {
		return false, nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	keys, err := NormalizeBackupRecipients(fields)
	if err != nil {
		return false, err
	}
	if len(keys) == 0 {
		return false, nil
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return false, err
	}
	const actor = "system (env bootstrap)"
	res, err := database.ExecContext(ctx, `
		INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(key) DO NOTHING`, backupRecipientsKey, string(b), actor, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	audit(ctx, database, agencyid.Global, actor, "Settings", "backup-keys-updated", "Backups", backupKeysChange(nil, keys, false))
	return true, nil
}
