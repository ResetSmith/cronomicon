package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestRunProbeHonorsTimeout is the regression guard for the startup-hang bug: a
// slow/hung toolchain probe (here a 30s sleep) must be abandoned at the deadline
// and force-killed, not block the caller — otherwise registration never runs.
func TestRunProbeHonorsTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the unix `sleep` binary")
	}
	start := time.Now()
	out := runProbeWithTimeout(context.Background(), 100*time.Millisecond, "sleep", "30")
	elapsed := time.Since(start)

	if out != nil {
		t.Errorf("timed-out probe should return nil, got %q", out)
	}
	// Deadline is 100ms; WaitDelay backstop is 2s. Anything approaching 30s means
	// the probe did not honor the timeout. Allow generous slack for slow CI.
	if elapsed > 5*time.Second {
		t.Errorf("probe did not abandon at its deadline: took %v", elapsed)
	}
}

// TestLookPathBounded checks the bounded PATH lookup resolves a real binary and
// reports a missing one as absent — the timeout/abandon path (a $PATH dir on a
// hung mount) can't be exercised portably in a unit test, but runAbandonable's
// deadline test covers that same select shape.
func TestLookPathBounded(t *testing.T) {
	dir := fakeRunTypeBins(t, "ansible-playbook")
	t.Setenv("PATH", dir)

	if _, ok := lookPathBounded(context.Background(), "ansible-playbook"); !ok {
		t.Error("lookPathBounded should find a binary that is on PATH")
	}
	if _, ok := lookPathBounded(context.Background(), "definitely-not-a-real-binary-xyz"); ok {
		t.Error("lookPathBounded should report a missing binary as absent")
	}
}

// fakeRunTypeBins installs no-op executables with the given names into a fresh
// PATH dir (replacing PATH entirely so detection sees only them).
func fakeRunTypeBins(t *testing.T, names ...string) string {
	t.Helper()
	binDir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	return binDir
}

// fakeAnsibleBins installs a fake `ansible` (--version) and `ansible-galaxy`
// (collection list) on PATH so detection is deterministic without a real
// ansible. baseJSON is the `collection list --format json` output.
func fakeAnsibleBins(t *testing.T, coreLine, baseJSON string) {
	t.Helper()
	binDir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("ansible", "#!/bin/sh\necho '"+coreLine+"'\n")
	write("ansible-galaxy", `#!/bin/sh
if [ "$1" = "collection" ] && [ "$2" = "list" ]; then
cat <<'JSON'
`+baseJSON+`
JSON
exit 0
fi
exit 0
`)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func TestDetectCapabilities(t *testing.T) {
	fakeAnsibleBins(t, "ansible [core 2.16.3]",
		`{"/usr/base": {"community.vmware": {"version": "3.5.0"}, "ansible.posix": {"version": "1.5.4"}}}`)

	cfg := Config{
		Capabilities:      []string{"ansible", "terraform"},
		AllowCheckout:     true,
		VaultPasswordFile: "/etc/cronomicon/vault.pw",
	}
	caps, tc, err := detectCapabilities(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"ansible": true, "terraform": true, // configured run-types preserved
		"checkout": true, "vault": true, // flags → tokens
		"collection:community.vmware": true, "collection:ansible.posix": true,
	}
	got := map[string]bool{}
	for _, c := range caps {
		got[c] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("missing capability token %q; got %v", w, caps)
		}
	}
	if tc.AnsibleCore != "2.16.3" {
		t.Errorf("ansibleCore = %q, want 2.16.3", tc.AnsibleCore)
	}
	if !tc.Checkout || !tc.Vault {
		t.Errorf("toolchain flags: checkout=%v vault=%v, want both true", tc.Checkout, tc.Vault)
	}
	if tc.Collections["community.vmware"] != "3.5.0" {
		t.Errorf("collections detail missing version: %+v", tc.Collections)
	}
}

func TestDetectCapabilitiesNoAnsible(t *testing.T) {
	// Empty PATH dir: no ansible/ansible-galaxy → only configured run-types +
	// flag-derived tokens (no shell-out needed for those).
	t.Setenv("PATH", t.TempDir())
	cfg := Config{Capabilities: []string{"bash", "perl"}, AllowCheckout: false}
	caps, tc, err := detectCapabilities(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range caps {
		got[c] = true
	}
	if !got["bash"] || !got["perl"] {
		t.Errorf("configured run-types must survive detection: %v", caps)
	}
	if got["ansible"] || got["checkout"] || got["vault"] {
		t.Errorf("no ansible/flags ⇒ no such tokens: %v", caps)
	}
	if tc.AnsibleCore != "" {
		t.Errorf("no ansible ⇒ empty ansibleCore, got %q", tc.AnsibleCore)
	}
}

func TestDeclaredKeyNames(t *testing.T) {
	dir := t.TempDir()
	for _, fn := range []string{"prod", "staging.pem", "dev.key", "prod.pub", "README", "svc.key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, fn), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		KeyDir: dir,
		KeyMap: map[string]string{"mapped": "/keys/mapped.pem", "prod": "/keys/prod"},
	}
	got := declaredKeyNames(cfg)
	// Sorted, deduped (prod appears in both map + dir once), ONE suffix stripped
	// (svc.key.pem → svc.key, matching resolveKeyPath, NOT svc), .pub ignored,
	// README kept as a bare name (any file could be an authKeyEnvVar).
	want := []string{"README", "dev", "mapped", "prod", "staging", "svc.key"}
	if len(got) != len(want) {
		t.Fatalf("declaredKeyNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("declaredKeyNames[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	// Names only — never a path.
	for _, n := range got {
		if strings.Contains(n, "/") {
			t.Errorf("declaredKeyNames must be names, not paths: %q", n)
		}
	}
	// No keys → nil.
	if declaredKeyNames(Config{}) != nil {
		t.Errorf("no keys should yield nil")
	}
}

func TestDetectCapabilitiesPopulatesKeyNames(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ansible shell-outs needed
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ansible_rh8_key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Capabilities: []string{"bash"}, KeyDir: dir}
	_, tc, err := detectCapabilities(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(tc.KeyNames) != 1 || tc.KeyNames[0] != "ansible_rh8_key" {
		t.Errorf("toolchains.KeyNames = %v, want [ansible_rh8_key]", tc.KeyNames)
	}
}

func TestDetectRunTypes(t *testing.T) {
	// bash + python3 + ansible-playbook on PATH; pwsh/perl/terraform absent.
	// python (not python3) also present to prove the alternates dedupe.
	fakeRunTypeBins(t, "bash", "python3", "python", "ansible-playbook")

	got := map[string]bool{}
	for _, rt := range detectRunTypes(context.Background()) {
		got[rt] = true
	}
	want := []string{"bash", "python", "ansible"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing detected run-type %q; got %v", w, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("detected %v, want exactly %v", got, want)
	}
}

func TestDetectCapabilitiesAutoDetect(t *testing.T) {
	// Empty configured capabilities ⇒ the run-types are probed (D1: 1B).
	fakeRunTypeBins(t, "bash", "pwsh")

	caps, _, err := detectCapabilities(context.Background(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range caps {
		got[c] = true
	}
	if !got["bash"] || !got["powershell"] {
		t.Errorf("auto-detect must claim bash+powershell: %v", caps)
	}
	if got["python"] || got["ansible"] {
		t.Errorf("auto-detect must not claim absent toolchains: %v", caps)
	}
}

func TestDetectCapabilitiesAutoDetectNothing(t *testing.T) {
	// Empty capabilities AND an empty PATH ⇒ zero run-types is an error (an
	// unclaimable runner must not register).
	t.Setenv("PATH", t.TempDir())
	if _, _, err := detectCapabilities(context.Background(), Config{}); err == nil {
		t.Fatal("expected an error when auto-detect finds no run-type toolchains")
	}
}

func TestDetectedRunTypesFeedConfigDigest(t *testing.T) {
	// Installing a toolchain and restarting must drift the declared-config
	// digest so the server requests a redeclare (plan 2 Phase 1: detection
	// changes ride the drift channel for free).
	digest := func() string {
		caps, _, err := detectCapabilities(context.Background(), Config{})
		if err != nil {
			t.Fatal(err)
		}
		return runnerproto.ConfigDigest("r1", "Linux", caps, 5, "cronomicon", "test", runnerproto.ProtocolVersion)
	}
	binDir := fakeRunTypeBins(t, "bash")
	before := digest()
	// "Install" python, as a package manager would.
	if err := os.WriteFile(filepath.Join(binDir, "python3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if after := digest(); after == before {
		t.Error("digest must drift when a newly installed toolchain changes the detected run-types")
	}
}
