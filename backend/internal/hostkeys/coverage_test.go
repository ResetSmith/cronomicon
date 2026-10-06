package hostkeys

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
)

func TestTargetAndPattern(t *testing.T) {
	for _, c := range []struct {
		t               execspec.Target
		target, pattern string
	}{
		{execspec.Target{Name: "web01", Address: "10.0.0.5", Port: 22}, "10.0.0.5", "10.0.0.5"},
		{execspec.Target{Name: "web01", Address: "10.0.0.5"}, "10.0.0.5", "10.0.0.5"},
		{execspec.Target{Name: "web01", Address: "10.0.0.5", Port: 2222}, "10.0.0.5:2222", "[10.0.0.5]:2222"},
		// No address on the record: the name is what gets dialled.
		{execspec.Target{Name: "web01.example.com", Port: 22}, "web01.example.com", "web01.example.com"},
		{execspec.Target{Name: "v6", Address: "fd00::5", Port: 2222}, "[fd00::5]:2222", "[fd00::5]:2222"},
	} {
		if got := Target(c.t); got != c.target {
			t.Errorf("Target(%+v) = %q, want %q", c.t, got, c.target)
		}
		if got := Pattern(Target(c.t)); got != c.pattern {
			t.Errorf("Pattern(%q) = %q, want %q", Target(c.t), got, c.pattern)
		}
	}
	// Pattern is idempotent: the ledger stores patterns and re-derives them.
	if got := Pattern("[10.0.0.5]:2222"); got != "[10.0.0.5]:2222" {
		t.Errorf("Pattern is not idempotent: %q", got)
	}
}

// line builds a reported file line's patterns the way LoadTrusted does.
func line(hosts ...string) []filePattern {
	var out []filePattern
	for _, h := range hosts {
		p := filePattern{negate: strings.HasPrefix(h, "!")}
		p.host, p.port = knownhostsline.HostPort(strings.TrimPrefix(h, "!"))
		out = append(out, p)
	}
	return out
}

// loaderTrusts asks the runner's real verifier whether a file holding these
// lines knows the host — the thing the coverage report must never contradict
// in the "trusted" direction.
func loaderTrusts(t *testing.T, fileLines [][]string, pattern string) bool {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(pub)
	var b strings.Builder
	for _, hosts := range fileLines {
		b.WriteString(knownhostsline.Render(strings.Join(hosts, ","), key) + "\n")
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		t.Fatalf("loader: %v", err)
	}
	host, port := knownhostsline.HostPort(pattern)
	return cb(net.JoinHostPort(host, port), &net.TCPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 22}, key) == nil
}

// File lines are matched as the verifier matches them. Every case is checked
// against the real loader, so the table cannot drift from what a run does.
func TestFileLinesMatchLikeTheVerifier(t *testing.T) {
	file := [][]string{
		{"10.0.0.6"},
		{"*.dmz.example.com", "!bad.dmz.example.com"},
		{"10.9.0.?"},
		{"[10.8.0.*]:2222"},
		{"*.any"},
	}
	tr := Trusted{approved: map[string]string{}}
	for _, l := range file {
		tr.lines = append(tr.lines, line(l...))
	}
	for pattern, want := range map[string]bool{
		"10.0.0.6":            true,
		"10.0.0.7":            false,
		"a.dmz.example.com":   true,
		"dmz.example.com":     false, // the pattern needs the dot
		"bad.dmz.example.com": false, // negated on the same line
		"10.9.0.4":            true,
		"10.9.0.44":           false, // ? is exactly one character
		"[10.8.0.3]:2222":     true,
		"[10.8.0.3]:2200":     false,
		"10.8.0.3":            false, // the line names port 2222 only
		"[10.0.0.6]:2222":     false, // another port is another sshd
		"x.any":               true,
		"[x.any]:2222":        false, // an unbracketed wildcard means port 22
		"A.DMZ.EXAMPLE.COM":   false,
	} {
		got := tr.pattern(pattern) == StateInFile
		if got != want {
			t.Errorf("pattern(%q) in file = %v, want %v", pattern, got, want)
		}
		if real := loaderTrusts(t, file, pattern); real != want {
			t.Errorf("the verifier says %q trusted = %v; this table says %v", pattern, real, want)
		}
	}
}

func TestApprovedBeatsTheFile(t *testing.T) {
	tr := Trusted{approved: map[string]string{"10.0.0.5": StateApproved, "10.0.0.8": StateQueued}, lines: [][]filePattern{line("10.0.0.5"), line("10.0.0.6")}}
	for pattern, want := range map[string]string{"10.0.0.5": StateApproved, "10.0.0.6": StateInFile, "10.0.0.8": StateQueued, "10.0.0.9": StateNone} {
		if got := tr.State(ScopeHost{Pattern: pattern}); got != want {
			t.Errorf("State(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// A host behind a bastion is two verified hops. Trusting the target's key
// alone is not enough, and the state is the weaker of the two.
func TestTrustedStateBehindABastion(t *testing.T) {
	h := ScopeHost{Pattern: "10.50.0.9", ViaPattern: "jump-a"}
	for _, c := range []struct {
		host, bastion, want string
	}{
		{StateApproved, StateNone, StateNone},
		{StateNone, StateApproved, StateNone},
		{StateApproved, StateApproved, StateApproved},
		{StateApproved, StateInFile, StateInFile},
		{StateInFile, StateApproved, StateInFile},
		{StateApproved, StateQueued, StateQueued},
		{StateQueued, StateInFile, StateQueued},
	} {
		tr := Trusted{approved: map[string]string{}}
		for pattern, state := range map[string]string{h.Pattern: c.host, h.ViaPattern: c.bastion} {
			switch state {
			case StateInFile:
				tr.lines = append(tr.lines, line(pattern))
			case StateNone:
			default:
				tr.approved[pattern] = state
			}
		}
		if got := tr.State(h); got != c.want {
			t.Errorf("host %q + bastion %q = %q, want %q", c.host, c.bastion, got, c.want)
		}
	}
}
