package main

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// 2.5.1 SG6.3 (the pre-release review's A7, A8): CRONOMICON_BACKUP_RECIPIENTS
// with one bad key was refused whole, logged, and the server started with no
// backup keys, so the boot backup was written and uploaded unencrypted. And
// the refusal of a private key carried the key into the boot log.

func sg6Pool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sg6.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func sg6Recipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String()
}

func TestSG6_ABadRecipientVariableStopsTheStart(t *testing.T) {
	ctx := context.Background()
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	good := sg6Recipient(t)

	// Set, not usable, and no list stored: the start is refused, naming the
	// variable, and nothing is stored.
	for _, raw := range []string{good + ",age1typo", " , ,"} {
		pool := sg6Pool(t)
		err := seedBackupKeys(ctx, pool, raw, logger)
		if err == nil || !strings.Contains(err.Error(), "CRONOMICON_BACKUP_RECIPIENTS") {
			t.Errorf("%q with no list stored = %v, want a refused start naming the variable", raw, err)
		}
		if keys, _ := settings.BackupRecipients(ctx, pool); len(keys) != 0 {
			t.Errorf("%q stored %v", raw, keys)
		}
	}

	// A list is stored (here, none on purpose: encryption turned off): the
	// variable never applies again, so a bad one is logged and the start goes on.
	pool := sg6Pool(t)
	if _, err := pool.Exec(`INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES('backup.recipients', '[]', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if err := seedBackupKeys(ctx, pool, good+",age1typo", logger); err != nil {
		t.Errorf("a bad variable beside a stored list = %v, want the start to go on", err)
	}
	if !strings.Contains(log.String(), "CRONOMICON_BACKUP_RECIPIENTS") {
		t.Errorf("a bad variable beside a stored list was not logged: %s", log.String())
	}

	// A good one seeds on the first start.
	pool = sg6Pool(t)
	if err := seedBackupKeys(ctx, pool, good, logger); err != nil {
		t.Fatal(err)
	}
	if keys, _ := settings.BackupRecipients(ctx, pool); len(keys) != 1 || keys[0] != good {
		t.Errorf("seeded %v, want [%s]", keys, good)
	}
}

func TestSG6_NoRefusalCarriesAPrivateKey(t *testing.T) {
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	body := id.String()[len("AGE-SECRET-KEY-"):]
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	for _, stored := range []bool{false, true} {
		pool := sg6Pool(t)
		if stored {
			if _, err := pool.Exec(`INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES('backup.recipients', ?, 't', 't')`,
				`["`+sg6Recipient(t)+`"]`); err != nil {
				t.Fatal(err)
			}
		}
		log.Reset()
		err := seedBackupKeys(ctx, pool, `"`+id.String()+`"`, logger)
		if err != nil && strings.Contains(strings.ToUpper(err.Error()), body) {
			t.Errorf("stored=%t: the refused start carries the private key: %v", stored, err)
		}
		if strings.Contains(strings.ToUpper(log.String()), body) {
			t.Errorf("stored=%t: the log line carries the private key: %s", stored, log.String())
		}
	}
}
