package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"github.com/ResetSmith/cronomicon/internal/backup"
)

// smallDB writes a SQLite file that passes verifyRestoredDB (a runs table)
// holding marker, so a test can tell which database ended up at the target.
func smallDB(t *testing.T, path, marker string) {
	t.Helper()
	pool, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(`CREATE TABLE runs (id TEXT); INSERT INTO runs VALUES (?)`, marker); err != nil {
		t.Fatal(err)
	}
}

func markerOf(t *testing.T, path string) string {
	t.Helper()
	pool, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var m string
	if err := pool.QueryRow(`SELECT id FROM runs`).Scan(&m); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return m
}

func writeIdentity(t *testing.T, dir string, id *age.X25519Identity) string {
	t.Helper()
	p := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(p, []byte("# created: test\n# public key: "+id.Recipient().String()+"\n"+id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// 2.5.1 SG1.5: an encrypted snapshot needs --identity; a missing or wrong key
// is refused with the live DB untouched; the right key restores it; and the
// encrypted file itself is never modified.
func TestInstallEncryptedSnapshot(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "cronomicon.db")
	smallDB(t, live, "live")
	plain := filepath.Join(dir, "snap.db")
	smallDB(t, plain, "backup")
	right, wrong := mustKey(t), mustKey(t)
	sealed := filepath.Join(dir, "cronomicon-20261021.db.age")
	if _, err := backup.EncryptFile(plain, sealed, []string{right.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(sealed)

	if code := installSnapshot(sealed, "x", live, nil); code == 0 {
		t.Fatal("an encrypted snapshot restored without --identity")
	}
	if markerOf(t, live) != "live" {
		t.Fatal("a refused restore touched the live DB")
	}
	wrongDir := t.TempDir()
	if code := installSnapshot(sealed, "x", live, []string{writeIdentity(t, wrongDir, wrong)}); code == 0 {
		t.Fatal("a wrong key restored the snapshot")
	}
	if markerOf(t, live) != "live" {
		t.Fatal("a wrong key touched the live DB")
	}
	if _, err := os.Stat(live + ".restore-decrypted"); !os.IsNotExist(err) {
		t.Fatalf("a refused decrypt left its file (%v)", err)
	}
	rightDir := t.TempDir()
	if code := installSnapshot(sealed, "x", live, []string{writeIdentity(t, wrongDir, wrong), writeIdentity(t, rightDir, right)}); code != 0 {
		t.Fatalf("the right key among several: code %d", code)
	}
	if markerOf(t, live) != "backup" {
		t.Fatal("the live DB is not the backup after the restore")
	}
	if _, err := os.Stat(live + ".restore-decrypted"); !os.IsNotExist(err) {
		t.Fatalf("the decrypted copy was left behind (%v)", err)
	}
	after, _ := os.ReadFile(sealed)
	if !bytes.Equal(before, after) {
		t.Fatal("the encrypted snapshot was modified")
	}
}

// --file restores a local unencrypted copy as before, and an encrypted one
// renamed to anything is still recognised by its header.
func TestRestoreLocalFile(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "cronomicon.db")
	smallDB(t, live, "live")
	plain := filepath.Join(dir, "cronomicon-20261020.db")
	smallDB(t, plain, "plain-backup")
	if code := restoreLocalFile(plain, live, nil, true); code != 0 {
		t.Fatalf("unencrypted --file: code %d", code)
	}
	if markerOf(t, live) != "plain-backup" {
		t.Fatal("unencrypted --file did not restore")
	}

	k := mustKey(t)
	renamed := filepath.Join(dir, "copy-from-usb")
	other := filepath.Join(dir, "other.db")
	smallDB(t, other, "sealed-backup")
	if _, err := backup.EncryptFile(other, renamed, []string{k.Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	if code := restoreLocalFile(renamed, live, nil, true); code == 0 {
		t.Fatal("a renamed encrypted file restored without a key")
	}
	if code := restoreLocalFile(renamed, live, []string{writeIdentity(t, t.TempDir(), k)}, true); code != 0 {
		t.Fatalf("renamed encrypted --file: code %d", code)
	}
	if markerOf(t, live) != "sealed-backup" {
		t.Fatal("renamed encrypted --file did not restore")
	}
}

func mustKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
