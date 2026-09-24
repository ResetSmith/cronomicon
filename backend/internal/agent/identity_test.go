package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.json")

	// Missing file → (nil, nil): first run, must register.
	got, err := loadIdentity(path)
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil identity for missing file, got %+v", got)
	}

	want := Identity{ID: "run-123", APIKey: "amt_run_secret"}
	if err := saveIdentity(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}

	// File perms must be 0600 (holds a bearer key).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity file perms = %o, want 600", perm)
	}

	got, err = loadIdentity(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || *got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestLoadIdentityCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A corrupt (but present) file is an error — re-registering would orphan the
	// existing runners row.
	if _, err := loadIdentity(path); err == nil {
		t.Error("expected error for corrupt identity file")
	}
}

func TestLoadIdentityIncomplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.json")
	if err := os.WriteFile(path, []byte(`{"id":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadIdentity(path); err == nil {
		t.Error("expected error for identity missing apiKey")
	}
}

func TestDiscardIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id.json")
	if err := saveIdentity(path, Identity{ID: "a", APIKey: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := discardIdentity(path); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("identity file should be gone after discard")
	}
	// Discarding a missing file is a no-op, not an error.
	if err := discardIdentity(path); err != nil {
		t.Errorf("discard missing: %v", err)
	}
}
