package agent

import (
	"os"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestMaterializeKeys (D8): delivered key material lands in 0600 files off the run
// tree, with a bare-name key-map and an CRONOMICON_KEY_<name> env map pointing at them;
// cleanup wipes every file and removes the directory.
func TestMaterializeKeys(t *testing.T) {
	keys := []runnerproto.ManifestKey{
		{Name: "deploy_key", Reference: "CRONOMICON_KEY_deploy_key", Material: "PEM-DEPLOY"},
		{Name: "backup_key", Reference: "CRONOMICON_KEY_backup_key", Material: "PEM-BACKUP"},
	}
	keyMap, keyEnv, cleanup, err := materializeKeys(keys)
	if err != nil {
		t.Fatalf("materializeKeys: %v", err)
	}

	// key-map is keyed by bare NAME; env map by the derived REFERENCE; both point at
	// the same on-disk file, which contains exactly the material at mode 0600.
	for _, k := range keys {
		path := keyMap[k.Name]
		if path == "" || path != keyEnv[k.Reference] {
			t.Fatalf("key %q: keyMap=%q keyEnv=%q mismatch", k.Name, path, keyEnv[k.Reference])
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %q: %v", path, err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("key file mode = %v, want 0600", fi.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if want := k.Material + "\n"; err != nil || string(data) != want {
			t.Errorf("key file content = %q (%v), want %q", data, err, want)
		}
	}

	// Grab a path, then cleanup must remove it (and the whole dir).
	somePath := keyMap["deploy_key"]
	cleanup()
	if _, err := os.Stat(somePath); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove key file (stat err=%v)", err)
	}
}

// TestMaterializeKeysNormalizesLineEndings: material that Go's validate-on-save
// accepts but OpenSSH refuses — CRLF endings, no trailing newline — is written as
// canonical LF text with a final newline, because ansible dials with the SYSTEM ssh.
func TestMaterializeKeysNormalizesLineEndings(t *testing.T) {
	const want = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
	for name, material := range map[string]string{
		"crlf":          "-----BEGIN OPENSSH PRIVATE KEY-----\r\nAAAA\r\n-----END OPENSSH PRIVATE KEY-----\r\n",
		"no_trailing":   "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----",
		"crlf_and_none": "-----BEGIN OPENSSH PRIVATE KEY-----\r\nAAAA\r\n-----END OPENSSH PRIVATE KEY-----",
		"already_clean": want,
	} {
		keyMap, _, cleanup, err := materializeKeys([]runnerproto.ManifestKey{
			{Name: "k", Reference: "CRONOMICON_KEY_k", Material: material},
		})
		if err != nil {
			t.Fatalf("%s: materializeKeys: %v", name, err)
		}
		data, rerr := os.ReadFile(keyMap["k"])
		if rerr != nil || string(data) != want {
			t.Errorf("%s: key file content = %q (%v), want %q", name, data, rerr, want)
		}
		cleanup()
	}
}

// TestMaterializeKeysEmpty: no delivered keys ⇒ nil maps and a safe no-op cleanup.
func TestMaterializeKeysEmpty(t *testing.T) {
	keyMap, keyEnv, cleanup, err := materializeKeys(nil)
	if err != nil || keyMap != nil || keyEnv != nil {
		t.Fatalf("empty materializeKeys = %v, %v, err=%v", keyMap, keyEnv, err)
	}
	cleanup() // must not panic
}

// TestMergeKeyMapDeliveredWins (D8): a delivered key overrides the runner's own
// key-map entry for the same name; other entries are preserved; the base map is not
// mutated (a fresh map is returned).
func TestMergeKeyMapDeliveredWins(t *testing.T) {
	base := map[string]string{"deploy_key": "/etc/agent/deploy", "other": "/etc/agent/other"}
	delivered := map[string]string{"deploy_key": "/run/keys/key-0"}
	merged := mergeKeyMap(base, delivered)

	if merged["deploy_key"] != "/run/keys/key-0" {
		t.Errorf("delivered did not win: %q", merged["deploy_key"])
	}
	if merged["other"] != "/etc/agent/other" {
		t.Errorf("unrelated key-dir entry lost: %q", merged["other"])
	}
	if base["deploy_key"] != "/etc/agent/deploy" {
		t.Errorf("base map was mutated: %q", base["deploy_key"])
	}
	// nil delivered → base returned unchanged.
	if got := mergeKeyMap(base, nil); got["deploy_key"] != "/etc/agent/deploy" {
		t.Errorf("nil-delivered merge altered base: %q", got["deploy_key"])
	}
}
