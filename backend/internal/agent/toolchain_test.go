package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

	if _, ok, late := lookPathBounded(context.Background(), "ansible-playbook"); !ok || late {
		t.Error("lookPathBounded should find a binary that is on PATH")
	}
	if _, ok, late := lookPathBounded(context.Background(), "definitely-not-a-real-binary-xyz"); ok || late {
		t.Error("lookPathBounded should report a missing binary as absent, and not as a timeout")
	}
}

// hangLookups makes every PATH lookup block until the test ends, as it does
// under a unit whose syscall filter kills the looking thread (systemd 239) or
// with a $PATH directory on a hung mount.
func hangLookups(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	prevLook, prevTimeout := lookPath, lookPathTimeout
	lookPath = func(string) (string, error) { <-release; return "", exec.ErrNotFound }
	lookPathTimeout = 20 * time.Millisecond
	t.Cleanup(func() { close(release); lookPath, lookPathTimeout = prevLook, prevTimeout })
}

func TestALookupThatNeverReturnsIsNotAnAbsentToolchain(t *testing.T) {
	// The agent still starts (it has the shell types), so "could not look" has
	// to be reported: otherwise an agent that has lost ansible this way is
	// indistinguishable from one that never had it.
	hangLookups(t)

	types, undetermined := detectRunTypes(context.Background())
	if want := []string{"bash", "perl", "powershell", "python"}; !slices.Equal(types, want) {
		t.Errorf("claims %v, want exactly %v", types, want)
	}
	if want := []string{"ansible", "terraform"}; !slices.Equal(undetermined, want) {
		t.Errorf("undetermined = %v, want %v", undetermined, want)
	}

	_, tc := detectCapabilities(context.Background(), Config{})
	if want := []string{"ansible", "terraform"}; !slices.Equal(tc.Undetermined, want) {
		t.Errorf("the registration detail reports %v as not looked for, want %v", tc.Undetermined, want)
	}
}

func TestAnAbsentToolchainIsNotReportedAsUndetermined(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, undetermined := detectRunTypes(context.Background()); len(undetermined) != 0 {
		t.Errorf("a lookup that answered \"not found\" was reported as unanswered: %v", undetermined)
	}
	_, tc := detectCapabilities(context.Background(), Config{})
	if len(tc.Undetermined) != 0 {
		t.Errorf("undetermined = %v on a host that simply has no toolchains", tc.Undetermined)
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
	caps, tc := detectCapabilities(context.Background(), cfg)

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
	caps, tc := detectCapabilities(context.Background(), cfg)
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
	_, tc := detectCapabilities(context.Background(), cfg)
	if len(tc.KeyNames) != 1 || tc.KeyNames[0] != "ansible_rh8_key" {
		t.Errorf("toolchains.KeyNames = %v, want [ansible_rh8_key]", tc.KeyNames)
	}
}

func TestDetectRunTypes(t *testing.T) {
	// The four shell types come with every agent: they run on the targets, so
	// this host's PATH does not decide them. Only bash and ansible-playbook
	// are here; perl, pwsh, python and terraform are absent.
	fakeRunTypeBins(t, "bash", "ansible-playbook")

	got := map[string]bool{}
	types, _ := detectRunTypes(context.Background())
	for _, rt := range types {
		got[rt] = true
	}
	want := []string{"bash", "perl", "powershell", "python", "ansible"}
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
	// Empty configured capabilities ⇒ detectRunTypes decides (D1: 1B).
	fakeRunTypeBins(t, "terraform")

	caps, _ := detectCapabilities(context.Background(), Config{})
	got := map[string]bool{}
	for _, c := range caps {
		got[c] = true
	}
	for _, rt := range []string{"bash", "perl", "powershell", "python", "terraform"} {
		if !got[rt] {
			t.Errorf("auto-detect must claim %q: %v", rt, caps)
		}
	}
	if got["ansible"] {
		t.Errorf("auto-detect must not claim a local toolchain this host lacks: %v", caps)
	}
}

func TestDetectCapabilitiesAutoDetectOnABareHost(t *testing.T) {
	// The slim runner image: no interpreter, no shell, nothing on PATH. It
	// used to find no run-types, refuse to register and restart for ever. An
	// agent dials its targets for shell jobs, so it claims them regardless.
	t.Setenv("PATH", t.TempDir())
	caps, _ := detectCapabilities(context.Background(), Config{})
	if want := []string{"bash", "perl", "powershell", "python"}; !slices.Equal(caps, want) {
		t.Errorf("a bare host claims %v, want exactly %v", caps, want)
	}
}

func TestExplicitCapabilitiesAreNotWidened(t *testing.T) {
	// Naming capabilities is how an operator narrows: the shell types are a
	// default for an agent that names none, never an addition to a list.
	t.Setenv("PATH", t.TempDir())
	caps, _ := detectCapabilities(context.Background(), Config{Capabilities: []string{"bash"}})
	if want := []string{"bash"}; !slices.Equal(caps, want) {
		t.Errorf("explicit capabilities became %v, want exactly %v", caps, want)
	}
}

func TestLocalToolchainProbesCoverExactlyTheLocalRunTypes(t *testing.T) {
	// A type is either run here (probed on this host) or on the targets
	// (granted): a type in both lists, or in neither, is decided wrongly.
	probed := map[string]bool{}
	for _, p := range localToolchainProbes {
		probed[p.runType] = true
		if !localRunTypes[p.runType] {
			t.Errorf("%q is probed on the agent's host but does not run there", p.runType)
		}
	}
	for rt := range localRunTypes {
		if !probed[rt] {
			t.Errorf("%q runs on the agent's host but nothing probes for it", rt)
		}
	}
	for _, rt := range shellRunTypes {
		if localRunTypes[rt] {
			t.Errorf("%q is granted as a shell type but runs on the agent's host", rt)
		}
	}
}

func TestDetectedRunTypesFeedConfigDigest(t *testing.T) {
	// Installing a toolchain and restarting must drift the declared-config
	// digest so the server requests a redeclare (plan 2 Phase 1: detection
	// changes ride the drift channel for free).
	digest := func() string {
		caps, _ := detectCapabilities(context.Background(), Config{})
		return runnerproto.ConfigDigest("r1", "Linux", caps, 5, "cronomicon", "test", runnerproto.ProtocolVersion)
	}
	binDir := fakeRunTypeBins(t, "bash")
	before := digest()
	// "Install" terraform, as a package manager would.
	if err := os.WriteFile(filepath.Join(binDir, "terraform"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if after := digest(); after == before {
		t.Error("digest must drift when a newly installed toolchain changes the detected run-types")
	}
}
