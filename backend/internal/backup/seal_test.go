package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func newKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// One key and two keys: each private key opens the snapshot on its own (SG-2),
// and the recipients file lists both fingerprints.
func TestEncryptDecryptRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.db")
	want := bytes.Repeat([]byte("SQLite format 3\x00 payload "), 50_000)
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	a, b := newKey(t), newKey(t)
	dst := filepath.Join(dir, "cronomicon-20261021.db.age")
	fps, err := EncryptFile(src, dst, []string{a.Recipient().String(), b.Recipient().String()})
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if len(fps) != 2 || fps[0] != Fingerprint(a.Recipient().String()) {
		t.Fatalf("fingerprints %v", fps)
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o600 {
		t.Fatalf("sealed mode %v", st.Mode().Perm())
	}
	sidecar, err := os.ReadFile(dst + RecipientsSuffix)
	if err != nil || strings.Fields(string(sidecar))[1] != fps[1] {
		t.Fatalf("recipients file %q (%v)", sidecar, err)
	}
	sealed, _ := os.ReadFile(dst)
	if bytes.Contains(sealed, []byte("SQLite format 3")) {
		t.Fatal("the sealed file contains the plaintext")
	}
	for i, id := range []age.Identity{a, b} {
		out := filepath.Join(dir, "restored.db")
		if err := DecryptFile(dst, out, []age.Identity{id}); err != nil {
			t.Fatalf("key %d: decrypt: %v", i, err)
		}
		got, _ := os.ReadFile(out)
		if !bytes.Equal(got, want) {
			t.Fatalf("key %d: plaintext differs", i)
		}
	}
}

// A key the snapshot was not sealed to is refused, and nothing is written.
func TestDecryptWrongKeyRefusedWritesNothing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "plain.db")
	_ = os.WriteFile(src, []byte("data"), 0o600)
	dst := filepath.Join(dir, "x.db.age")
	if _, err := EncryptFile(src, dst, []string{newKey(t).Recipient().String()}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.db")
	if err := DecryptFile(dst, out, []age.Identity{newKey(t)}); err == nil {
		t.Fatal("a wrong key opened the snapshot")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("a refused decrypt left %s (%v)", out, err)
	}
}

// What a person is likely to paste instead of a public key is refused with a
// sentence that says what it was.
func TestParseRecipientRefusals(t *testing.T) {
	priv := newKey(t)
	cases := map[string]string{
		"":                                     "empty",
		priv.String():                          "PRIVATE key",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA": "SSH keys",
		"age1pq1abcdef":                        "post-quantum",
		"age1notakey":                          "not an age public key",
	}
	for in, wantText := range cases {
		_, err := ParseRecipient(in)
		if err == nil || !errors.Is(err, ErrInvalidRecipient) || !strings.Contains(err.Error(), wantText) {
			t.Errorf("ParseRecipient(%.20q) = %v, want %q", in, err, wantText)
		}
	}
	good := "  " + priv.Recipient().String() + "\n"
	got, err := ParseRecipient(good)
	if err != nil || got != priv.Recipient().String() {
		t.Fatalf("a good key: %q, %v", got, err)
	}
}

// SG-7: an encrypted snapshot is uploaded as an opaque object with the
// fingerprints of its keys in the metadata; an unencrypted one as before.
func TestPutOptions(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, "cronomicon-20261021.db")
	if o, err := putOptions(plain); err != nil || o.ContentType != "application/x-sqlite3" || len(o.UserMetadata) != 0 {
		t.Fatalf("unencrypted: %+v %v", o, err)
	}
	sealed := filepath.Join(dir, "cronomicon-20261021.db.age")
	if _, err := putOptions(sealed); err == nil {
		t.Fatal("an encrypted snapshot without its recipients file was uploaded without them")
	}
	_ = os.WriteFile(sealed+RecipientsSuffix, []byte("aaaa\nbbbb\n"), 0o600)
	o, err := putOptions(sealed)
	if err != nil || o.ContentType != "application/octet-stream" || o.UserMetadata[recipientsMetadata] != "aaaa,bbbb" {
		t.Fatalf("encrypted: %+v %v", o, err)
	}
}

// A failed seal leaves neither the sealed file nor the recipients file.
func TestEncryptFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "x.db.age")
	if _, err := EncryptFile(filepath.Join(dir, "missing.db"), dst, []string{newKey(t).Recipient().String()}); err == nil {
		t.Fatal("encrypting a missing file succeeded")
	}
	for _, f := range []string{dst, dst + RecipientsSuffix} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s left behind (%v)", f, err)
		}
	}
}
