package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// genKeyPEM returns an unencrypted OpenSSH ed25519 private key PEM.
func genKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

// genEncryptedKeyPEM returns a passphrase-protected ed25519 private key PEM.
func genEncryptedKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("hunter2"))
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

func TestInspectKeys(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("goodkey", genKeyPEM(t))         // valid → OK
	write("enckey", genEncryptedKeyPEM(t)) // passphrase → WARN
	write("garbage.pem", []byte("nope"))   // not a key → WARN, name stripped to "garbage"
	mapGood := write("mapfile", genKeyPEM(t))
	write("mapname", []byte("garbage")) // same NAME via key-dir, but key-map wins

	cfg := Config{
		KeyDir: dir,
		KeyMap: map[string]string{"mapname": mapGood},
	}
	names, warns := inspectKeys(cfg)

	nameSet := map[string]bool{}
	for _, n := range names {
		nameSet[n] = true
	}
	if !nameSet["goodkey"] {
		t.Errorf("goodkey should be an OK name: %v", names)
	}
	if !nameSet["mapname"] {
		t.Errorf("mapname (key-map, valid) should win over the garbage key-dir file: %v / %v", names, warns)
	}
	joined := strings.Join(warns, " | ")
	if !strings.Contains(joined, "enckey") || !strings.Contains(joined, "passphrase") {
		t.Errorf("expected a passphrase warning for enckey: %v", warns)
	}
	if !strings.Contains(joined, "garbage") {
		t.Errorf("expected an unusable-key warning for garbage: %v", warns)
	}
	// Never leak material: no warning/name should contain PEM bytes.
	if strings.Contains(joined, "PRIVATE KEY") {
		t.Errorf("inspectKeys must never surface key material: %v", warns)
	}
}

func TestInspectKeysIgnoresPubAndBareNonKeys(t *testing.T) {
	// A generated key writes NAME + NAME.pub; a healthy dir must not WARN on the
	// .pub sibling, nor on a bare non-key file a user dropped in.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "deploy"), genKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy.pub"), []byte("ssh-ed25519 AAAA... amadeus-runner:deploy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("keys live here"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, warns := inspectKeys(Config{KeyDir: dir})
	if len(warns) != 0 {
		t.Errorf("healthy key + .pub + README must not warn, got: %v", warns)
	}
	if len(names) != 1 || names[0] != "deploy" {
		t.Errorf("only 'deploy' should resolve, got: %v", names)
	}
	// A .pem/.key-suffixed corrupt file still warns (it declares key intent).
	if err := os.WriteFile(filepath.Join(dir, "broken.key"), []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, warns = inspectKeys(Config{KeyDir: dir})
	if len(warns) != 1 || !strings.Contains(warns[0], "broken") {
		t.Errorf("a corrupt .key file should warn, got: %v", warns)
	}
}

func TestDoctorAuthEnvPEMWinsOverKeyDir(t *testing.T) {
	// loadSigner precedence is key-map → env PEM → key-dir; the resolve report
	// must name the env when both an env PEM and a key-dir file exist.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DUAL_KEY"), genKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUAL_KEY", string(genKeyPEM(t)))
	checks, ok := DoctorAuth(context.Background(), Config{KeyDir: dir}, "DUAL_KEY", "")
	if !ok {
		t.Fatalf("should resolve: %v", checks)
	}
	if !checkHas(checks, "auth-resolve", CheckPass, "PEM in agent env") {
		t.Errorf("auth-resolve must name the env PEM (loadSigner's actual source), not the key-dir file: %v", checks)
	}
}

func TestCountKnownHostsEntries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "known_hosts")
	content := "# a comment\n\nhost1 ssh-ed25519 AAAA...\nhost2 ssh-rsa BBBB...\n   \n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := countKnownHostsEntries(p)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("entry count = %d, want 2 (comments/blank lines ignored)", n)
	}
	if _, err := countKnownHostsEntries(filepath.Join(dir, "nope")); err == nil {
		t.Errorf("missing file should error")
	}
}

func TestSplitAuthTarget(t *testing.T) {
	tests := []struct {
		in         string
		user, host string
		port       int
	}{
		{"deploy@web1", "deploy", "web1", 0},
		{"web1", "", "web1", 0},
		{"deploy@web1:2222", "deploy", "web1", 2222},
		{"web1:2222", "", "web1", 2222},
		{"root@10.0.0.1", "root", "10.0.0.1", 0},
	}
	for _, tc := range tests {
		u, h, p := splitAuthTarget(tc.in)
		if u != tc.user || h != tc.host || p != tc.port {
			t.Errorf("splitAuthTarget(%q) = (%q,%q,%d), want (%q,%q,%d)", tc.in, u, h, p, tc.user, tc.host, tc.port)
		}
	}
}

func TestDoctorAuthResolution(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "ansible_rh8_key")
	if err := os.WriteFile(keyFile, genKeyPEM(t), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	t.Run("key-dir hit, no target", func(t *testing.T) {
		checks, ok := DoctorAuth(ctx, Config{KeyDir: dir}, "ansible_rh8_key", "")
		if !ok {
			t.Fatalf("resolution should succeed: %v", checks)
		}
		if !checkHas(checks, "auth-resolve", CheckPass, "key-dir") {
			t.Errorf("auth-resolve should PASS naming key-dir: %v", checks)
		}
		if !checkHas(checks, "auth-key", CheckPass, "parsed OK") {
			t.Errorf("auth-key should PASS: %v", checks)
		}
	})

	t.Run("unresolvable name", func(t *testing.T) {
		checks, ok := DoctorAuth(ctx, Config{KeyDir: dir}, "nonesuch", "")
		if ok {
			t.Fatalf("unresolvable name should fail: %v", checks)
		}
		if !checkHas(checks, "auth-resolve", CheckFail, "no local key") {
			t.Errorf("auth-resolve should FAIL with the precise message: %v", checks)
		}
	})

	t.Run("env-PEM resolves for Go-SSH", func(t *testing.T) {
		t.Setenv("DOCTOR_ENV_KEY", string(genKeyPEM(t)))
		checks, ok := DoctorAuth(ctx, Config{}, "DOCTOR_ENV_KEY", "")
		if !ok {
			t.Fatalf("env-PEM should resolve: %v", checks)
		}
		if !checkHas(checks, "auth-resolve", CheckPass, "PEM in agent env") {
			t.Errorf("auth-resolve should note the env PEM: %v", checks)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		if _, ok := DoctorAuth(ctx, Config{}, "", ""); ok {
			t.Errorf("empty name should fail")
		}
	})
}

// checkHas reports whether checks contain a check with the given name+status
// whose detail contains sub.
func checkHas(checks []Check, name string, status CheckStatus, sub string) bool {
	for _, c := range checks {
		if c.Name == name && c.Status == status && strings.Contains(c.Detail, sub) {
			return true
		}
	}
	return false
}
