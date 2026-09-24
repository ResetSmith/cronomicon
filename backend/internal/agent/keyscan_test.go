package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendKnownHostsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	l1 := "web01 ssh-ed25519 AAAAkey1"
	l2 := "web02 ssh-rsa AAAAkey2"

	// First append creates the file (0640) and writes both lines.
	if err := appendKnownHosts(path, []string{l1, l2}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("known_hosts mode = %v (err %v), want 0640", fi.Mode().Perm(), err)
	}

	// Re-appending the same lines + a new one must NOT duplicate (idempotent).
	if err := appendKnownHosts(path, []string{l1, l2, "web03 ssh-ed25519 AAAAkey3"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	body := string(b)
	if strings.Count(body, l1) != 1 || strings.Count(body, l2) != 1 {
		t.Errorf("duplicate entries after re-append:\n%s", body)
	}
	if !strings.Contains(body, "web03") {
		t.Errorf("new entry not appended:\n%s", body)
	}
}

func TestEnsureKnownHostsFileCreatesEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "known_hosts") // dir doesn't exist yet
	if err := ensureKnownHostsFile(path); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() != 0 || fi.Mode().Perm() != 0o640 {
		t.Fatalf("ensureKnownHostsFile: %v (size %d, mode %v)", err, fi.Size(), fi.Mode().Perm())
	}
}
