package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestVerifyRestoredDB proves the post-restore integrity check passes on a real
// migrated DB and fails on a non-DB file (FU-3 Phase C).
func TestVerifyRestoredDB(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "amadeus.db")
	pool, err := db.Open(good)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool.Close()

	if err := verifyRestoredDB(good); err != nil {
		t.Errorf("verifyRestoredDB(migrated) = %v, want nil", err)
	}

	bad := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(bad, []byte("not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestoredDB(bad); err == nil {
		t.Error("verifyRestoredDB(garbage) = nil, want error")
	}
}

// TestDbLooksInUse returns "" for a nonexistent target and for a closed DB.
func TestDbLooksInUse(t *testing.T) {
	dir := t.TempDir()
	if why := dbLooksInUse(filepath.Join(dir, "nope.db")); why != "" {
		t.Errorf("dbLooksInUse(missing) = %q, want \"\"", why)
	}
	p := filepath.Join(dir, "closed.db")
	pool, err := db.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	pool.Close()
	if why := dbLooksInUse(p); why != "" {
		t.Errorf("dbLooksInUse(closed) = %q, want \"\" (no active writer)", why)
	}
}

// TestCopyFileContents round-trips bytes and creates parent dirs.
func TestCopyFileContents(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, []byte("snapshot-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "nested", "dst.bin")
	if err := copyFileContents(src, dst); err != nil {
		t.Fatalf("copyFileContents: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "snapshot-bytes" {
		t.Errorf("copied contents = %q", got)
	}
}
