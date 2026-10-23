package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCheckHealth pins the three failure modes the doctor must distinguish:
// healthy, an SSO-proxy HTML login page, and a plain non-200.
func TestCheckHealth(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string // substring; "" = expect nil
	}{
		{"healthy", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200); _, _ = w.Write([]byte("ok")) }, ""},
		{"sso html intercept", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<!doctype html><html>login</html>"))
		}, "reverse proxy"},
		{"server error", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500); _, _ = w.Write([]byte("boom")) }, "HTTP 500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			c, err := NewClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			err = c.CheckHealth(context.Background())
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want nil, got %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestDoctorQuickPassesAgainstLiveServer checks the aggregate PASS path and the
// missing-token FAIL path, in quick mode (no toolchain/sandbox probes).
func TestDoctorQuickPassesAgainstLiveServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	dir := t.TempDir()

	cfg, err := Resolve([]string{"-server", srv.URL, "-name", "d", "-capabilities", "bash",
		"-registration-token", "crn_reg_x", "-identity-file", dir + "/id.json"}, noEnv)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	checks, ok := Doctor(context.Background(), cfg, true)
	if !ok {
		t.Errorf("expected all quick checks to pass, got %+v", checks)
	}
	if s := statusOf(checks, "server-reachable"); s != CheckPass {
		t.Errorf("server-reachable = %s, want PASS", s)
	}

	// Drop the token and remove the (absent) identity → registration FAIL.
	cfg.RegistrationToken = ""
	checks, ok = Doctor(context.Background(), cfg, true)
	if ok {
		t.Error("expected FAIL with no token and no identity")
	}
	if s := statusOf(checks, "registration"); s != CheckFail {
		t.Errorf("registration = %s, want FAIL", s)
	}
}

// The quick doctor runs before every start (ExecStartPre), inside the unit.
// A unit that kills the agent's PATH lookups passes every other check — the
// directories stat fine — so the lookup itself is one, and its failure says
// what to change.
func TestDoctorQuickFailsWhenAPathLookupNeverReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg, err := Resolve([]string{"-server", srv.URL, "-name", "d",
		"-registration-token", "crn_reg_x", "-identity-file", t.TempDir() + "/id.json"}, noEnv)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if checks, _ := Doctor(context.Background(), cfg, true); statusOf(checks, "path-lookup") != CheckPass {
		t.Fatalf("path-lookup must pass on a host whose lookups return: %+v", checks)
	}

	hangLookups(t)
	checks, ok := Doctor(context.Background(), cfg, true)
	if ok {
		t.Error("the doctor passed on a host whose PATH lookups never return")
	}
	if s := statusOf(checks, "path-lookup"); s != CheckFail {
		t.Errorf("path-lookup = %s, want FAIL", s)
	}
	for _, c := range checks {
		if c.Name == "path-lookup" && !strings.Contains(c.Detail, "SystemCallErrorNumber=EPERM") {
			t.Errorf("the failure must name the fix: %q", c.Detail)
		}
	}
}

// The unit that kills lookups kills only the ones that FIND a file, and most
// agents have no Ansible or Terraform to find. The check must fail there too:
// such an agent still loses systemd-run, and when it looked for
// ansible-playbook the doctor passed on RHEL 8.10 beside a sandbox probe that
// never returned.
func TestDoctorQuickFailsOnABrokenUnitWithNoLocalToolchain(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg, err := Resolve([]string{"-server", srv.URL, "-name", "d",
		"-registration-token", "crn_reg_x", "-identity-file", t.TempDir() + "/id.json"}, noEnv)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Setenv("PATH", t.TempDir()) // nothing installed: no local toolchain to find

	hangLookupsThatFindAFile(t)
	for _, p := range localToolchainProbes {
		for _, bin := range p.bins {
			if _, _, late := lookPathBounded(context.Background(), bin); late {
				t.Fatalf("the lookup of %s, which is not installed, must return on this host for the test to mean anything", bin)
			}
		}
	}
	checks, ok := Doctor(context.Background(), cfg, true)
	if ok {
		t.Error("the doctor passed under a unit that hangs every lookup that finds a file")
	}
	if s := statusOf(checks, "path-lookup"); s != CheckFail {
		t.Errorf("path-lookup = %s, want FAIL: %+v", s, checks)
	}
}

// The full doctor is what the installer runs last, so its `sandbox` line is
// where most administrators first learn their agent has none. It is a warning,
// never a failure (unsandboxed is a supported mode), and it has to say why and
// what bounds the agent instead: on an installed agent the reason is a refusal,
// not a missing program.
func TestDoctorSandboxCheckSaysWhyAndWhatToDo(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox probe is Linux-only")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	cfg, err := Resolve([]string{"-server", srv.URL, "-name", "d",
		"-registration-token", "crn_reg_x", "-identity-file", t.TempDir() + "/id.json"}, noEnv)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	binDir := t.TempDir()
	t.Setenv("PATH", binDir)
	fake := func(script string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(binDir, "systemd-run"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := func(c Config, quick bool) (Check, bool) {
		t.Helper()
		checks, ok := Doctor(context.Background(), c, quick)
		for _, ch := range checks {
			if ch.Name == "sandbox" {
				return ch, ok
			}
		}
		return Check{}, ok
	}

	const refusal = "Failed to start transient scope unit: Interactive authentication required."
	fake("echo '" + refusal + "' >&2\nexit 1\n")
	got, ok := sandbox(cfg, false)
	if got.Status != CheckWarn {
		t.Fatalf("sandbox = %q %q, want a WARN", got.Status, got.Detail)
	}
	if !ok {
		t.Error("an agent with no sandbox failed the doctor: unsandboxed is a supported mode")
	}
	for _, want := range []string{refusal, "runs execute unsandboxed", "systemctl set-property <unit> MemoryMax=", "--memory-max", "limit the container"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("the sandbox line lacks %q: %s", want, got.Detail)
		}
	}

	// No systemd-run at all (a container): a different reason, the same remedy.
	if err := os.Remove(filepath.Join(binDir, "systemd-run")); err != nil {
		t.Fatal(err)
	}
	if got, _ := sandbox(cfg, false); got.Status != CheckWarn || !strings.Contains(got.Detail, "systemd-run is not on $PATH") {
		t.Errorf("no systemd-run: sandbox = %q %q, want a WARN that names the missing program", got.Status, got.Detail)
	}

	fake("exit 0\n")
	if got, _ := sandbox(cfg, false); got.Status != CheckPass || strings.Contains(got.Detail, "set-property") {
		t.Errorf("a scope that can be created: sandbox = %q %q, want a plain PASS", got.Status, got.Detail)
	}

	// Turned off on purpose is said as that, with no probe and no remedy to offer.
	off := cfg
	off.NoSandbox = true
	if got, _ := sandbox(off, false); got.Status != CheckWarn || got.Detail != "disabled (NoSandbox)" {
		t.Errorf("NoSandbox: sandbox = %q %q", got.Status, got.Detail)
	}

	// The quick doctor (before every start) does not probe: it must stay fast.
	if got, _ := sandbox(cfg, true); got.Name != "" {
		t.Errorf("the quick doctor ran the sandbox check: %+v", got)
	}
}

func statusOf(checks []Check, name string) CheckStatus {
	for _, c := range checks {
		if c.Name == name {
			return c.Status
		}
	}
	return "MISSING"
}
