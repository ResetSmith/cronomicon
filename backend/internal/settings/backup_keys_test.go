package settings

import (
	"context"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/ResetSmith/cronomicon/internal/backup"
)

func ageKey(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

// SG-8 and SG-4: turning encryption on needs the acknowledgement, adding a
// second key does not, and turning it off needs the confirmation.
func TestSetBackupRecipientsAcknowledgements(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	k1, k2 := ageKey(t), ageKey(t)

	if _, err := SetBackupRecipients(ctx, pool, []string{k1}, false, false, "ana"); !errors.Is(err, ErrBackupKeyCustody) {
		t.Fatalf("first key without the acknowledgement: %v", err)
	}
	if keys, _ := BackupRecipients(ctx, pool); len(keys) != 0 {
		t.Fatalf("a refused first key was stored: %v", keys)
	}
	got, err := SetBackupRecipients(ctx, pool, []string{k1}, true, false, "ana")
	if err != nil || len(got.Recipients) != 1 || got.LastModifiedBy != "ana" {
		t.Fatalf("first key: %+v, %v", got, err)
	}
	if _, err := SetBackupRecipients(ctx, pool, []string{k1, k2, k1}, false, false, "bo"); err != nil {
		t.Fatalf("second key: %v", err)
	}
	if keys, _ := BackupRecipients(ctx, pool); len(keys) != 2 {
		t.Fatalf("repeat not dropped: %v", keys)
	}
	if _, err := SetBackupRecipients(ctx, pool, nil, false, false, "bo"); !errors.Is(err, ErrBackupUnencryptedConfirm) {
		t.Fatalf("removing the last keys without the confirmation: %v", err)
	}
	if _, err := SetBackupRecipients(ctx, pool, nil, false, true, "bo"); err != nil {
		t.Fatalf("turning encryption off: %v", err)
	}
	var details string
	if err := pool.QueryRowContext(ctx, `SELECT details FROM change_log WHERE action='backup-keys-updated' ORDER BY rowid LIMIT 1`).Scan(&details); err != nil {
		t.Fatalf("no change log row: %v", err)
	}
	if !strings.Contains(details, "acknowledgedKeyCustody=true") || !strings.Contains(details, backup.Fingerprint(k1)) {
		t.Fatalf("first change log row %q", details)
	}
}

// A malformed key is a validation error naming what was pasted.
func TestSetBackupRecipientsRefusesAPrivateKey(t *testing.T) {
	pool := openTestPool(t)
	id, _ := age.GenerateX25519Identity()
	_, err := SetBackupRecipients(context.Background(), pool, []string{id.String()}, true, false, "ana")
	if !errors.Is(err, ErrValidation) || !errors.Is(err, backup.ErrInvalidRecipient) || !strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("private key: %v", err)
	}
}

// The environment seeds once, and never over a stored list (even an empty one
// an administrator saved).
func TestSeedBackupRecipientsFromEnvOnce(t *testing.T) {
	pool := openTestPool(t)
	ctx := context.Background()
	k1, k2 := ageKey(t), ageKey(t)
	seeded, err := SeedBackupRecipientsFromEnv(ctx, pool, k1+",\n"+k2)
	if err != nil || !seeded {
		t.Fatalf("seed: %v %v", seeded, err)
	}
	if keys, _ := BackupRecipients(ctx, pool); len(keys) != 2 {
		t.Fatalf("seeded %v", keys)
	}
	if _, err := SetBackupRecipients(ctx, pool, nil, false, true, "ana"); err != nil {
		t.Fatal(err)
	}
	if seeded, err := SeedBackupRecipientsFromEnv(ctx, pool, k1); err != nil || seeded {
		t.Fatalf("second seed: %v %v", seeded, err)
	}
	if keys, _ := BackupRecipients(ctx, pool); len(keys) != 0 {
		t.Fatalf("the seed overwrote a saved list: %v", keys)
	}
	if _, err := SeedBackupRecipientsFromEnv(ctx, openTestPool(t), "not-a-key"); err == nil {
		t.Fatal("an invalid seed was accepted")
	}
}
