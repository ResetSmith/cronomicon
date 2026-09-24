package execspec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

func TestSafeReadRepoFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	body := []byte("#!/bin/bash\necho hi\n")
	if err := os.WriteFile(filepath.Join(root, "scripts", "ok.sh"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	// Uncapped read returns the full body, never truncated.
	data, trunc, err := execspec.SafeReadRepoFile(root, "scripts/ok.sh", 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if trunc || string(data) != string(body) {
		t.Fatalf("uncapped: trunc=%v data=%q", trunc, data)
	}

	// A cap below the file size returns exactly limit bytes and flags truncation.
	data, trunc, err = execspec.SafeReadRepoFile(root, "scripts/ok.sh", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !trunc || string(data) != string(body[:5]) {
		t.Fatalf("capped: trunc=%v data=%q", trunc, data)
	}

	// A cap exactly at the file size is NOT a truncation.
	data, trunc, err = execspec.SafeReadRepoFile(root, "scripts/ok.sh", int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if trunc || len(data) != len(body) {
		t.Fatalf("at-cap: trunc=%v len=%d", trunc, len(data))
	}

	// Path-traversal / absolute paths are rejected before any read.
	for _, bad := range []string{"../escape.sh", "/etc/passwd", "scripts/../../etc/passwd"} {
		if _, _, err := execspec.SafeReadRepoFile(root, bad, 0); err == nil {
			t.Errorf("expected rejection for %q, got nil", bad)
		}
	}

	// A symlink pointing outside the root is rejected by the post-EvalSymlinks Rel
	// re-check (the load-bearing guard against symlink escapes).
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "scripts", "link.sh")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execspec.SafeReadRepoFile(root, "scripts/link.sh", 0); err == nil {
		t.Error("expected symlink-escape rejection, got nil")
	}

	// A missing file is an error (not a panic / empty success).
	if _, _, err := execspec.SafeReadRepoFile(root, "scripts/nope.sh", 0); err == nil {
		t.Error("expected error for missing file, got nil")
	}
}
