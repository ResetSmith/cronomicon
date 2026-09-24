package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// noEnv is a getenv that returns nothing (clean environment).
func noEnv(string) string { return "" }

// envMap returns a getenv backed by a map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDefaults(t *testing.T) {
	cfg, err := Resolve([]string{
		"-server", "https://srv:8080/",
		"-name", "r1",
		"-capabilities", "bash,perl",
	}, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.ServerURL != "https://srv:8080" { // trailing slash trimmed
		t.Errorf("ServerURL = %q", cfg.ServerURL)
	}
	if cfg.OS != "Linux" {
		t.Errorf("default OS = %q, want Linux", cfg.OS)
	}
	if cfg.PollInterval != 60*time.Second {
		t.Errorf("default poll interval = %v, want 60s (D7)", cfg.PollInterval)
	}
	if cfg.Inventory != "amadeus" {
		t.Errorf("default inventory = %q", cfg.Inventory)
	}
	if got := cfg.Capabilities; len(got) != 2 || got[0] != "bash" || got[1] != "perl" {
		t.Errorf("capabilities = %v", got)
	}
}

// TestResolvePrecedence verifies flags > env > file > defaults.
func TestResolvePrecedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(file, []byte(`{
		"serverUrl": "https://file-server",
		"name": "file-name",
		"capabilities": ["bash"],
		"maxConcurrent": 1,
		"pollInterval": "10s"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	env := envMap(map[string]string{
		"CRONOMICON_RUNNER_SERVER":         "https://env-server",
		"CRONOMICON_RUNNER_MAX_CONCURRENT": "2",
		// name not in env → should keep file value
	})

	// Flag overrides env for server; env overrides file for maxConcurrent; file
	// supplies name (no env/flag); flag supplies pollInterval.
	cfg, err := Resolve([]string{
		"-config", file,
		"-server", "https://flag-server",
		"-poll-interval", "5s",
	}, env)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.ServerURL != "https://flag-server" {
		t.Errorf("server: flag should win, got %q", cfg.ServerURL)
	}
	if cfg.MaxConcurrent != 2 {
		t.Errorf("maxConcurrent: env should beat file, got %d", cfg.MaxConcurrent)
	}
	if cfg.Name != "file-name" {
		t.Errorf("name: file should supply, got %q", cfg.Name)
	}
	if cfg.PollInterval != 5*time.Second {
		t.Errorf("pollInterval: flag should win, got %v", cfg.PollInterval)
	}
}

func TestResolveValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"missing server", []string{"-name", "r", "-capabilities", "bash"}},
		{"missing name", []string{"-server", "s", "-capabilities", "bash"}},
		{"bad os", []string{"-server", "s", "-name", "r", "-capabilities", "bash", "-os", "Mac"}},
		{"bad inventory", []string{"-server", "s", "-name", "r", "-capabilities", "bash", "-inventory", "nope"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(tc.args, noEnv); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestResolveServerURLScheme(t *testing.T) {
	// A schemeless server value (env-set, or a non-bash installer that skipped
	// the prepend guard) must self-heal to https:// rather than reaching
	// net/http as `unsupported protocol scheme ""`. An explicit scheme is kept.
	ok := []struct {
		name, in, want string
	}{
		{"bare host", "amadeus.example.com", "https://amadeus.example.com"},
		{"bare host:port", "amadeus.example.com:8080", "https://amadeus.example.com:8080"},
		{"https preserved + slash trimmed", "https://srv/", "https://srv"},
		{"http preserved (dev/local)", "http://localhost:8080", "http://localhost:8080"},
		{"surrounding whitespace trimmed", "  amadeus.example.com  ", "https://amadeus.example.com"},
	}
	for _, tc := range ok {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Resolve([]string{"-server", tc.in, "-name", "r", "-capabilities", "bash"}, noEnv)
			if err != nil {
				t.Fatalf("Resolve(%q) errored: %v", tc.in, err)
			}
			if cfg.ServerURL != tc.want {
				t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, tc.want)
			}
		})
	}

	bad := []struct{ name, in string }{
		{"unusable scheme", "ftp://amadeus.example.com"},
		{"scheme but no host", "https:///only-a-path"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve([]string{"-server", tc.in, "-name", "r", "-capabilities", "bash"}, noEnv); err == nil {
				t.Errorf("Resolve(%q) should have rejected the server URL", tc.in)
			}
		})
	}
}

func TestResolveEmptyCapabilitiesIsAutoDetect(t *testing.T) {
	// Unset capabilities is no longer a config error — it means "probe the
	// host's toolchains at startup" (D1: 1B). detectCapabilities owns the
	// zero-run-types failure instead.
	cfg, err := Resolve([]string{"-server", "s", "-name", "r"}, noEnv)
	if err != nil {
		t.Fatalf("unset capabilities must resolve (auto-detect): %v", err)
	}
	if len(cfg.Capabilities) != 0 {
		t.Errorf("capabilities = %v, want empty (auto-detect sentinel)", cfg.Capabilities)
	}
}

func TestParseKeyMap(t *testing.T) {
	m, err := parseKeyMap("PROD_KEY=/keys/prod.pem,DEV_KEY=/keys/dev")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m["PROD_KEY"] != "/keys/prod.pem" || m["DEV_KEY"] != "/keys/dev" {
		t.Errorf("key map = %v", m)
	}
	if _, err := parseKeyMap("bogus-no-equals"); err == nil {
		t.Error("expected error for malformed key-map entry")
	}
}

// TestConfigFileUnknownFieldRejected ensures a typo'd config key is loud.
func TestConfigFileUnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(file, []byte(`{"servrUrl":"oops"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve([]string{"-config", file, "-server", "s", "-name", "n", "-capabilities", "bash"}, noEnv); err == nil {
		t.Error("expected error for unknown config field")
	}
}

// RX.9 (Phase 1) — the scoped-env knobs resolve through file, env, and flag
// layers like every other setting.
func TestResolveScopedEnvKnobs(t *testing.T) {
	// Defaults: no extras, SSH_AUTH_SOCK kept.
	cfg, err := Resolve([]string{
		"-server", "https://srv", "-name", "r1", "-capabilities", "ansible",
	}, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(cfg.EnvBaseExtra) != 0 || cfg.ExcludeSSHAuthSock {
		t.Errorf("defaults: EnvBaseExtra=%v ExcludeSSHAuthSock=%v", cfg.EnvBaseExtra, cfg.ExcludeSSHAuthSock)
	}

	// File layer.
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(file, []byte(`{
		"serverUrl": "https://srv", "name": "r1", "capabilities": ["ansible"],
		"envBaseExtra": ["ANSIBLE_CONFIG"], "excludeSshAuthSock": true
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Resolve([]string{"-config", file}, noEnv)
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if len(cfg.EnvBaseExtra) != 1 || cfg.EnvBaseExtra[0] != "ANSIBLE_CONFIG" || !cfg.ExcludeSSHAuthSock {
		t.Errorf("file layer: EnvBaseExtra=%v ExcludeSSHAuthSock=%v", cfg.EnvBaseExtra, cfg.ExcludeSSHAuthSock)
	}

	// Env layer overrides file; flag layer overrides env.
	env := envMap(map[string]string{
		"CRONOMICON_RUNNER_ENV_BASE_EXTRA":        "A,B",
		"CRONOMICON_RUNNER_EXCLUDE_SSH_AUTH_SOCK": "false",
	})
	cfg, err = Resolve([]string{"-config", file, "-env-base-extra", "C"}, env)
	if err != nil {
		t.Fatalf("resolve env+flag: %v", err)
	}
	if len(cfg.EnvBaseExtra) != 1 || cfg.EnvBaseExtra[0] != "C" {
		t.Errorf("flag should win: EnvBaseExtra=%v", cfg.EnvBaseExtra)
	}
	if cfg.ExcludeSSHAuthSock {
		t.Errorf("env layer should override file: ExcludeSSHAuthSock=%v", cfg.ExcludeSSHAuthSock)
	}
}

func TestResolveAuthBridgeKnobs(t *testing.T) {
	// Defaults: bridge on (NoAuthBridge false), no ssh-common-args override.
	cfg, err := Resolve([]string{
		"-server", "https://srv", "-name", "r1", "-capabilities", "ansible",
	}, noEnv)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.NoAuthBridge || cfg.AnsibleSSHCommonArgs != "" {
		t.Errorf("defaults: NoAuthBridge=%v AnsibleSSHCommonArgs=%q", cfg.NoAuthBridge, cfg.AnsibleSSHCommonArgs)
	}

	// File layer sets both.
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.json")
	if err := os.WriteFile(file, []byte(`{
		"serverUrl": "https://srv", "name": "r1", "capabilities": ["ansible"],
		"noAuthBridge": true, "ansibleSshCommonArgs": "-o ProxyJump=bastion"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Resolve([]string{"-config", file}, noEnv)
	if err != nil {
		t.Fatalf("resolve file: %v", err)
	}
	if !cfg.NoAuthBridge || cfg.AnsibleSSHCommonArgs != "-o ProxyJump=bastion" {
		t.Errorf("file layer: NoAuthBridge=%v AnsibleSSHCommonArgs=%q", cfg.NoAuthBridge, cfg.AnsibleSSHCommonArgs)
	}

	// Env overrides file; flag overrides env.
	env := envMap(map[string]string{
		"CRONOMICON_RUNNER_NO_AUTH_BRIDGE":          "false",
		"CRONOMICON_RUNNER_ANSIBLE_SSH_COMMON_ARGS": "-o Ciphers=aes256-gcm@openssh.com",
	})
	cfg, err = Resolve([]string{"-config", file, "-ansible-ssh-common-args", "-o StrictHostKeyChecking=yes"}, env)
	if err != nil {
		t.Fatalf("resolve env+flag: %v", err)
	}
	if cfg.NoAuthBridge {
		t.Errorf("env layer should override file: NoAuthBridge=%v", cfg.NoAuthBridge)
	}
	if cfg.AnsibleSSHCommonArgs != "-o StrictHostKeyChecking=yes" {
		t.Errorf("flag should win: AnsibleSSHCommonArgs=%q", cfg.AnsibleSSHCommonArgs)
	}
}
