package knownhostsline

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func testKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// loads reports whether the runner's verifier accepts a file holding line.
func loads(t *testing.T, line string) bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := knownhosts.New(path)
	return err == nil
}

// The property this package exists for: a pattern ValidPattern accepts renders
// to a line the runner's verifier loads. One line it cannot load fails the
// whole file, and with it every run on that runner.
func TestValidPatternAgreesWithTheLoader(t *testing.T) {
	key := testKey(t)
	for _, c := range []struct {
		host  string
		valid bool
	}{
		{"web01", true},
		{"10.0.0.5", true},
		{"[10.0.0.5]:2222", true},
		{"[fd00::5]:2222", true},
		{"fd00::5", true},
		{"|1|c2FsdHNhbHRzYWx0c2FsdHNhbHQ=|aGFzaGhhc2hoYXNoaGFzaGhhc2g=", true},
		// Not one host.
		{"*.example.com", false}, {"web0?", false}, {"!web01", false}, {"web01,web02", false},
		// Would break the file.
		{"", false}, {"[", false}, {"[x", false}, {"[x]y", false}, {"[a:b", false}, {"[]:22", false},
		{"[web01]:0", false}, {"[web01]:port", false}, {"x]", false}, {"a[b", false},
		{"|1|abc", false}, {"|1|", false}, {"|1||", false}, {"|1|a|b|c", false}, {"|1|!!|??", false}, {"a|b", false},
		{"web 01", false}, {"web01\n", false},
	} {
		if got := ValidPattern(c.host); got != c.valid {
			t.Errorf("ValidPattern(%q) = %v, want %v", c.host, got, c.valid)
		}
		if c.valid && !loads(t, Render(c.host, key)) {
			t.Errorf("%q is accepted here but the loader refuses the line", c.host)
		}
	}
}

// Parse reads what the loader reads — including a comment with spaces in it,
// which ssh.ParseKnownHosts refuses and the loader trusts.
func TestParseReadsWhatTheLoaderTrusts(t *testing.T) {
	key := testKey(t)
	base := Render("web01,10.0.0.1", key)
	for _, line := range []string{base, base + " ops", base + " added by bob on tuesday", "  " + base + "\t# x", "@revoked " + base, "@cert-authority " + base + " the corp CA"} {
		l, err := Parse(line)
		if err != nil {
			t.Errorf("Parse(%q): %v", line, err)
			continue
		}
		if strings.Join(l.Hosts, ",") != "web01,10.0.0.1" || ssh.FingerprintSHA256(l.Key) != ssh.FingerprintSHA256(key) {
			t.Errorf("Parse(%q) = %+v", line, l)
		}
		if !loads(t, line) {
			t.Errorf("the loader refuses %q, so the test is wrong about what it trusts", line)
		}
	}
	if l, _ := Parse("@revoked " + base); l.Marker != "revoked" {
		t.Errorf("marker = %q", l.Marker)
	}
	for _, bad := range []string{"", "web01", "web01 ssh-ed25519", "web01 ssh-ed25519 not-base64!", "web01 ssh-ed25519 AAAA", "web01 ssh-rsa " + strings.Fields(base)[2]} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string][2]string{
		"web01":           {"web01", "22"},
		"[web01]:2222":    {"web01", "2222"},
		"[fd00::5]:2222":  {"fd00::5", "2222"},
		"*.example.com":   {"*.example.com", "22"},
		"[10.8.0.*]:2222": {"10.8.0.*", "2222"},
	} {
		if h, p := HostPort(in); h != want[0] || p != want[1] {
			t.Errorf("HostPort(%q) = %q %q, want %v", in, h, p, want)
		}
	}
}
