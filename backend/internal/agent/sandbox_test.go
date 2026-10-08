package agent

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestRunAbandonableHonorsDeadline is the regression guard for the startup-hang
// bug: probeSandbox blocked forever in cmd.Run() because a wedged systemd-run
// ignored the ctx-kill, so the agent never registered. runAbandonable must
// return at the deadline regardless of whether the child ever exits.
func TestRunAbandonableHonorsDeadline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses the unix `sleep` binary")
	}
	pctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	ok := runAbandonable(pctx, func() bool {
		cmd := exec.CommandContext(pctx, "sleep", "30")
		configureProcGroup(cmd)
		cmd.WaitDelay = time.Second
		return cmd.Run() == nil
	})
	elapsed := time.Since(start)

	if ok {
		t.Error("a sleep that never exits zero in time must report false")
	}
	if elapsed > 5*time.Second {
		t.Errorf("runAbandonable did not return at its deadline: took %v", elapsed)
	}
}

func TestSandboxWrap(t *testing.T) {
	argv := []string{"ansible-playbook", "site.yml"}

	// Unavailable ⇒ argv unchanged, not sandboxed.
	got, sb := sandboxWrap(Config{SandboxAvailable: false}, argv)
	if sb || len(got) != 2 || got[0] != "ansible-playbook" {
		t.Errorf("unavailable sandbox must pass argv through unchanged: %v sandboxed=%v", got, sb)
	}

	// Disabled ⇒ argv unchanged even when available.
	got, sb = sandboxWrap(Config{SandboxAvailable: true, NoSandbox: true}, argv)
	if sb || got[0] != "ansible-playbook" {
		t.Errorf("-no-sandbox must pass argv through: %v sandboxed=%v", got, sb)
	}

	// Available + caps ⇒ systemd-run scope wrapper with the resource props, then
	// the original argv after `--`.
	cfg := Config{SandboxAvailable: true, SandboxMemoryMax: "2G", SandboxCPUQuota: "150%", SandboxTasksMax: "512"}
	got, sb = sandboxWrap(cfg, argv)
	if !sb {
		t.Fatal("available sandbox must wrap")
	}
	joined := strings.Join(got, " ")
	for _, want := range []string{"systemd-run --scope", "--property=MemoryMax=2G", "--property=CPUQuota=150%", "--property=TasksMax=512", "-- ansible-playbook site.yml"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped argv missing %q: %s", want, joined)
		}
	}
	// The original argv must follow `--` intact.
	dash := -1
	for i, a := range got {
		if a == "--" {
			dash = i
		}
	}
	if dash < 0 || got[dash+1] != "ansible-playbook" || got[len(got)-1] != "site.yml" {
		t.Errorf("original argv not preserved after --: %v", got)
	}
}

func TestSandboxProvenance(t *testing.T) {
	// Sandboxed line names the caps.
	line := sandboxProvenance(Config{SandboxMemoryMax: "2G"}, true)
	if !strings.Contains(line, "systemd-run --scope") || !strings.Contains(line, "MemoryMax=2G") {
		t.Errorf("sandboxed provenance = %q", line)
	}
	// Unsandboxed + checkout ⇒ a LOUD warning.
	warn := sandboxProvenance(Config{AllowCheckout: true}, false)
	if !strings.Contains(warn, "WARNING") || !strings.Contains(warn, "UNSANDBOXED") {
		t.Errorf("unsandboxed checkout provenance must be a loud warning, got %q", warn)
	}
	// Unsandboxed body-only ⇒ a quieter note (no WARNING).
	note := sandboxProvenance(Config{AllowCheckout: false}, false)
	if strings.Contains(note, "WARNING") {
		t.Errorf("unsandboxed body-only note should not shout: %q", note)
	}
}

func TestDetectCapabilitiesSandboxedToken(t *testing.T) {
	// No ansible on PATH: only the flag-derived tokens matter here.
	t.Setenv("PATH", t.TempDir())
	caps, tc := detectCapabilities(context.Background(), Config{Capabilities: []string{"ansible"}, SandboxAvailable: true})
	found := false
	for _, c := range caps {
		if c == "sandboxed" {
			found = true
		}
	}
	if !found {
		t.Errorf("a sandbox-available runner must advertise the `sandboxed` token: %v", caps)
	}
	if !tc.Sandboxed {
		t.Errorf("toolchains.sandboxed must be true when available")
	}

	// Not available ⇒ no token.
	caps2, tc2 := detectCapabilities(context.Background(), Config{Capabilities: []string{"ansible"}, SandboxAvailable: false})
	for _, c := range caps2 {
		if c == "sandboxed" {
			t.Errorf("no sandbox ⇒ no `sandboxed` token: %v", caps2)
		}
	}
	if tc2.Sandboxed {
		t.Errorf("toolchains.sandboxed must be false when unavailable")
	}
}

// fakeSystemdRun installs a fake `systemd-run` that strips its own flags up to
// `--` and execs the remainder — so a wrapped run still executes, letting the
// test verify the wiring end-to-end without a real systemd manager.
func fakeSystemdRun(t *testing.T) {
	t.Helper()
	binDir := filepath.Dir(mustLookPathDir(t))
	script := "#!/bin/sh\nwhile [ \"$1\" != \"--\" ] && [ $# -gt 0 ]; do shift; done\nshift\nexec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "systemd-run"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// mustLookPathDir returns the dir of the fake ansible-playbook installed by
// fakeToolchain, so the fake systemd-run lands on the SAME PATH entry.
func mustLookPathDir(t *testing.T) string {
	t.Helper()
	p, _, _ := strings.Cut(os.Getenv("PATH"), ":")
	return filepath.Join(p, "ansible-playbook")
}

func TestRunLocalToolchainSandboxedExec(t *testing.T) {
	stateDir := t.TempDir()
	emit, lines := fakeToolchain(t, "echo RAN\n")
	fakeSystemdRun(t) // lands on the same PATH dir as the fake ansible-playbook

	m := &runnerproto.ManifestResponse{RunType: "ansible", Body: "- hosts: all\n", EnvPassthrough: []string{}}
	cfg := Config{StateDir: stateDir, SandboxAvailable: true, SandboxTasksMax: "256"}
	code := runLocalToolchain(context.Background(), m, cfg, emit)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; lines:\n%s", code, strings.Join(lines(), "\n"))
	}
	out := strings.Join(lines(), "\n")
	if !strings.Contains(out, "RAN") {
		t.Errorf("wrapped command did not run through the fake systemd-run:\n%s", out)
	}
	if !strings.Contains(out, "sandbox: systemd-run --scope") {
		t.Errorf("expected a sandboxed provenance line:\n%s", out)
	}
	assertNoRunDirs(t, stateDir)
}

// An agent that is not root is refused its scope by polkit, and that is what
// nearly every installed agent is. "No usable systemd-run" alone read as a
// missing binary; the probe has to hand back what systemd-run said, so the
// startup line and the doctor can tell a refusal from a missing manager.
func TestProbeSandboxSaysWhyNot(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the probe is Linux-only")
	}
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	fake := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, "systemd-run"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if ok, why := probeSandbox(context.Background()); ok || !strings.Contains(why, "not on $PATH") {
		t.Errorf("no systemd-run: probeSandbox() = %v, %q; want false and the missing binary named", ok, why)
	}

	const refusal = "Failed to start transient scope unit: Interactive authentication required."
	fake("echo >&2\necho '" + refusal + "' >&2\necho 'a second line' >&2\nexit 1\n")
	if ok, why := probeSandbox(context.Background()); ok || why != refusal {
		t.Errorf("refused scope: probeSandbox() = %v, %q; want false and systemd-run's own first line", ok, why)
	}

	// A failure that says nothing: the exit status is all there is to report.
	fake("exit 3\n")
	if ok, why := probeSandbox(context.Background()); ok || !strings.Contains(why, "exit status 3") {
		t.Errorf("silent failure: probeSandbox() = %v, %q; want false and the exit status", ok, why)
	}

	fake("exit 0\n")
	if ok, why := probeSandbox(context.Background()); !ok || why != "" {
		t.Errorf("scope created: probeSandbox() = %v, %q; want true and no reason", ok, why)
	}
}

// The one line an operator reads in the journal when an agent has no per-run
// sandbox, which is every agent the installer sets up. It has to carry what
// systemd-run answered and what bounds the agent instead: the guides quote
// both, and "no usable systemd-run" alone sent readers looking for a program.
func TestNoSandboxLineSaysWhyAndWhatToDo(t *testing.T) {
	const refusal = "Failed to start transient scope unit: Interactive authentication required."
	for _, tc := range []struct {
		name     string
		checkout bool
		level    string
		msg      string
	}{
		{"an ordinary agent", false, "level=INFO", "tier-2 sandbox unavailable"},
		// Checkout runs a repository's code on the runner: never quietly uncapped.
		{"an agent with checkout on", true, "level=WARN", "SANDBOX UNAVAILABLE"},
	} {
		var buf bytes.Buffer
		logNoSandbox(slog.New(slog.NewTextHandler(&buf, nil)), tc.checkout, refusal)
		line := buf.String()
		if n := strings.Count(strings.TrimSpace(line), "\n"); n != 0 {
			t.Errorf("%s: want one line, got %d:\n%s", tc.name, n+1, line)
		}
		for _, want := range []string{
			tc.level, tc.msg,
			`reason="` + refusal + `"`,
			"hint=",
			"systemctl set-property <unit> MemoryMax=",
			"--memory-max", "--cpu-quota", "--tasks-max",
			"limit the container",
		} {
			if !strings.Contains(line, want) {
				t.Errorf("%s: the line lacks %q:\n%s", tc.name, want, line)
			}
		}
	}
}

// The hint names the installer's flags. It must not outlive them: every flag
// it mentions is one runner-install.sh still parses.
func TestSandboxHintNamesFlagsTheInstallerTakes(t *testing.T) {
	installer, err := os.ReadFile(filepath.Join("..", "..", "deploy", "runner-install.sh"))
	if err != nil {
		t.Fatalf("reading the installer: %v", err)
	}
	flags := regexp.MustCompile(`--[a-z][a-z-]+`).FindAllString(sandboxHint, -1)
	if len(flags) != 3 {
		t.Fatalf("the hint names %v; want the three limit flags", flags)
	}
	for _, f := range flags {
		if !regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(f) + `\) `).Match(installer) {
			t.Errorf("the hint names %s, which runner-install.sh does not parse", f)
		}
	}
}
