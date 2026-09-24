package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadSignerResolutionOrderAndErrors locks the three-step resolution order
// (key-map → env-PEM → key-dir) that the shared resolveKeyPath refactor must
// preserve, plus the Phase-1 error messages that name every place checked.
func TestLoadSignerResolutionOrderAndErrors(t *testing.T) {
	t.Run("empty name", func(t *testing.T) {
		r := &sshRunner{}
		if _, err := r.loadSigner(""); err == nil || !strings.Contains(err.Error(), "no authKeyEnvVar") {
			t.Errorf("empty name should error clearly, got %v", err)
		}
	})

	t.Run("env-PEM step runs before key-dir", func(t *testing.T) {
		// A garbage PEM in the env proves step 2 is consulted (and BEFORE key-dir):
		// the error is the parse failure, not a key-dir "not found".
		const name = "TEST_ENV_KEY"
		t.Setenv(name, "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-base64\n-----END OPENSSH PRIVATE KEY-----")
		r := &sshRunner{keyDir: t.TempDir()}
		_, err := r.loadSigner(name)
		if err == nil || !strings.Contains(err.Error(), "parse key from env") {
			t.Errorf("env-PEM should be attempted before key-dir, got %v", err)
		}
	})

	t.Run("no key-dir configured", func(t *testing.T) {
		r := &sshRunner{}
		_, err := r.loadSigner("SOME_KEY")
		if err == nil || !strings.Contains(err.Error(), "no key-dir configured") {
			t.Errorf("error should flag the unset key-dir, got %v", err)
		}
		if !strings.Contains(err.Error(), "SOME_KEY") {
			t.Errorf("error should name the credential, got %v", err)
		}
	})

	t.Run("key-dir configured but file absent", func(t *testing.T) {
		dir := t.TempDir()
		r := &sshRunner{keyDir: dir}
		_, err := r.loadSigner("SOME_KEY")
		if err == nil || !strings.Contains(err.Error(), "under key-dir") {
			t.Errorf("error should flag the missing file under key-dir, got %v", err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error should name the key-dir path, got %v", err)
		}
	})

	t.Run("key-map file that fails to parse is reached", func(t *testing.T) {
		// A key-map entry pointing at a non-key file proves step 1 is consulted
		// first: the error is the file parse failure, not "no local key".
		dir := t.TempDir()
		bad := filepath.Join(dir, "bad.pem")
		if err := os.WriteFile(bad, []byte("not a key"), 0o600); err != nil {
			t.Fatal(err)
		}
		r := &sshRunner{keyMap: map[string]string{"K": bad}}
		_, err := r.loadSigner("K")
		if err == nil || !strings.Contains(err.Error(), "parse key file") {
			t.Errorf("key-map file should be reached and parsed first, got %v", err)
		}
	})
}
