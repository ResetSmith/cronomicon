package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

// 2.5.1 SG6.3 (the pre-release review's A8): a private key pasted in any form
// but the bare line was refused with age's error, which quotes the input, so
// the refusal carried the private key: into the 422, the card and, from
// CRONOMICON_BACKUP_RECIPIENTS, the boot log.
func TestSG6_NoRefusalCarriesAPrivateKey(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec := id.String()
	body := sec[len("AGE-SECRET-KEY-"):]
	carries := func(msg string) bool { return strings.Contains(strings.ToUpper(msg), body) }
	for _, c := range []struct{ name, in string }{
		{"bare", sec},
		{"quoted", `"` + sec + `"`},
		{"single-quoted", `'` + sec + `'`},
		{"lower case", strings.ToLower(sec)},
		{"key=", "key=" + sec},
		{"the keygen file on one line", "# created: 2026-10-21T10:00:00Z # public key: " + id.Recipient().String() + " " + sec},
	} {
		_, err := ParseRecipient(c.in)
		switch {
		case err == nil:
			t.Errorf("%s: accepted", c.name)
		case carries(err.Error()):
			t.Errorf("%s: the refusal carries the private key: %v", c.name, err)
		case !strings.Contains(err.Error(), "PRIVATE key"):
			t.Errorf("%s: refused as %q, want the PRIVATE key message", c.name, err)
		}
	}
	// No refusal echoes what was pasted, whatever it was.
	for _, in := range []string{"age1typo", `"age1typo"`, "not a key at all"} {
		if _, err := ParseRecipient(in); err == nil || strings.Contains(err.Error(), in) {
			t.Errorf("%q: refusal %v, want one that does not repeat the input", in, err)
		}
	}
	// A stored key that is not a public key (written before this check, or by
	// hand) is named by its fingerprint when the seal refuses it.
	src := filepath.Join(t.TempDir(), "src.db")
	if err := os.WriteFile(src, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = EncryptFile(src, filepath.Join(t.TempDir(), "out.db.age"), []string{`"` + sec + `"`})
	if err == nil || carries(err.Error()) || !strings.Contains(err.Error(), Fingerprint(`"`+sec+`"`)) {
		t.Errorf("EncryptFile with a private key stored = %v, want a refusal naming its fingerprint and not the key", err)
	}
}
