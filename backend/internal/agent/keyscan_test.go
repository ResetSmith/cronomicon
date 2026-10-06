package agent

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
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

func genKeyLine(t *testing.T, host string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), k
}

// removeKnownHosts takes away exactly the lines it is given — matched on the
// parsed host and key, so spacing and a comment do not hide one — and leaves
// everything else, including a multi-host line that carries the same key.
func TestRemoveKnownHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	web01, key01 := genKeyLine(t, "web01")
	web02, _ := genKeyLine(t, "web02")
	web03, _ := genKeyLine(t, "[10.0.0.3]:2222")
	shared := "web01,10.0.0.1 " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key01)))
	otherKeySameHost, _ := genKeyLine(t, "web01")
	content := strings.Join([]string{
		"# seeded by hand",
		web01 + "   added-by-ops", // a comment after the key
		web02,
		"",
		web03,
		shared,
		otherKeySameHost,
		"@revoked " + web01,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	n, err := removeKnownHosts(path, []string{web01, web03, "not a line", "@revoked " + web02})
	if err != nil || n != 2 {
		t.Fatalf("removed %d (%v), want 2", n, err)
	}
	got, _ := os.ReadFile(path)
	want := strings.Join([]string{
		"# seeded by hand", web02, "", shared, otherKeySameHost, "@revoked " + web01,
	}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("file after removal:\n%s\nwant:\n%s", got, want)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode changed to %v; the rewrite must keep the file's mode", st.Mode().Perm())
	}
	// A line with a many-word comment is still found and removed.
	chatty, _ := genKeyLine(t, "web07")
	if err := os.WriteFile(path, []byte(chatty+" added by bob on tuesday\n"+web02+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := removeKnownHosts(path, []string{chatty}); err != nil || n != 1 {
		t.Fatalf("a commented line was not removed: %d (%v)", n, err)
	}
	// A symlinked known_hosts is rewritten where it points and stays a link.
	link := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if n, err := removeKnownHosts(link, []string{web02}); err != nil || n != 1 {
		t.Fatalf("removal through a symlink: %d (%v)", n, err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file")
	}
	if left, _ := os.ReadFile(path); len(left) != 0 {
		t.Fatalf("the link's target was not rewritten: %q", left)
	}
	if err := os.WriteFile(path, []byte(web02+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Removing what is not there changes nothing, and a missing file is not an error.
	if n, err := removeKnownHosts(path, []string{web01}); err != nil || n != 0 {
		t.Errorf("second removal = %d (%v), want 0", n, err)
	}
	if n, err := removeKnownHosts(filepath.Join(t.TempDir(), "absent"), []string{web01}); err != nil || n != 0 {
		t.Errorf("missing file = %d (%v), want 0, nil", n, err)
	}
}

// readKnownHosts reports what each line trusts and never the hash of a hashed
// host; a line that does not parse trusts nothing and is not reported.
func TestReadKnownHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	plain, k1 := genKeyLine(t, "web01,10.0.0.1")
	hashed, k2 := genKeyLine(t, "|1|c2FsdHNhbHRzYWx0c2FsdHNhbHQ=|aGFzaGhhc2hoYXNoaGFzaGhhc2g=")
	ca, k3 := genKeyLine(t, "*.example.com")
	// The last line carries a comment with spaces in it: ssh.ParseKnownHosts
	// refuses that, the verifier trusts it, and so it must be reported.
	chatty, k4 := genKeyLine(t, "web09")
	content := "# comment\n" + plain + "\n\nthis is not a key line\n" + hashed + "\n@cert-authority " + ca + "\n" + chatty + " added by bob on tuesday\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, truncated, err := readKnownHosts(path)
	if err != nil || truncated || len(entries) != 4 {
		t.Fatalf("entries = %+v truncated=%v err=%v; want 4", entries, truncated, err)
	}
	want := []knownHostsEntry{
		{Line: 2, Hosts: "web01,10.0.0.1", KeyType: k1.Type(), Fingerprint: ssh.FingerprintSHA256(k1)},
		{Line: 5, Hashed: true, KeyType: k2.Type(), Fingerprint: ssh.FingerprintSHA256(k2)},
		{Line: 6, Hosts: "*.example.com", Marker: "cert-authority", KeyType: k3.Type(), Fingerprint: ssh.FingerprintSHA256(k3)},
		{Line: 7, Hosts: "web09", KeyType: k4.Type(), Fingerprint: ssh.FingerprintSHA256(k4)},
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, entries[i], want[i])
		}
	}
	if e, _, err := readKnownHosts(filepath.Join(t.TempDir(), "absent")); err != nil || e != nil {
		t.Errorf("a missing file should report nothing, got %v %v", e, err)
	}
}
