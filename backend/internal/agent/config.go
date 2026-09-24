// Package agent holds the testable logic for the Cronomicon runner agent
// (cmd/amadeus-runner) — the out-of-process worker that registers with the
// server, long-polls for assigned runs, and executes them. The thin main.go
// wires this package to flags/signals.
//
// Key-custody posture is credential model (b) (runners-update.md D1): the
// server NEVER ships KEK-decrypted private-key bytes. The manifest carries host
// *references only* (ManifestTarget.AuthKeyEnvVar is the NAME of an env var /
// key file); the agent resolves those names to its OWN local private keys. No
// secret or KEK material ever flows server→agent.
package agent

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// jsonUnmarshalStrict unmarshals JSON rejecting unknown fields so a typo in the
// config file is a loud error rather than a silently-ignored setting.
func jsonUnmarshalStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Config is the fully-resolved runtime configuration for the agent. It is
// populated from (lowest→highest precedence) built-in defaults, an optional
// config file, environment variables (CRONOMICON_RUNNER_*), and command-line
// flags. See Resolve for the precedence rules.
type Config struct {
	// ServerURL is the base URL of the Cronomicon server, e.g. https://amadeus:8080.
	ServerURL string
	// CACertPath is an optional path to a PEM CA bundle to trust for TLS. Empty
	// uses the system trust store.
	CACertPath string
	// RegistrationToken is the shared registration bearer token (amt_reg_*).
	RegistrationToken string

	// Name is this runner's display name (must be stable across restarts so the
	// identity file keeps pairing to the same runners row).
	Name string
	// OS is "Linux" or "Windows" (validated server-side).
	OS string
	// Capabilities is the run-type vocabulary this runner claims (e.g.
	// bash,perl,powershell or ansible,terraform). Only matching runs are claimed.
	// Empty means auto-detect: the run-types are probed from the host's
	// toolchains at startup (D1: 1B); set explicitly to narrow the claim set.
	Capabilities []string
	// MaxConcurrent bounds simultaneously-executing runs on this agent.
	MaxConcurrent int
	// Inventory is the inventory-canonicality mode (D8): "amadeus" (manifest
	// carries resolved targets) or "local" (agent resolves scope→hosts itself).
	Inventory string

	// IdentityFile is where {id, apiKey} is persisted so a restart resumes the
	// same runners row instead of orphaning it (R2.4).
	IdentityFile string

	// PollInterval is the heartbeat/poll cadence (D7 default 60s). The agent
	// keeps polling at this cadence even while executing (R2.2).
	PollInterval time.Duration

	// KeyDir, if set, is a directory the agent searches for a private key named
	// <authKeyEnvVar> (or <authKeyEnvVar>.pem / .key) when no explicit mapping
	// exists. The agent holds its OWN keys (model b).
	KeyDir string
	// KeyMap maps an authKeyEnvVar NAME to a local private-key file path. Takes
	// precedence over KeyDir. Parsed from a "NAME=path,NAME2=path2" string.
	KeyMap map[string]string

	// KnownHostsFile is an optional OpenSSH known_hosts file used for strict
	// host-key verification when the manifest does not pin a host key
	// (documented onboarding mode). Empty ⇒ trust-on-first-use against this file
	// is disabled and verification falls back per host (see exec.go).
	KnownHostsFile string

	// AnsibleSSHCommonArgs, when set, is injected as ANSIBLE_SSH_COMMON_ARGS for
	// ansible runs INSTEAD of the default known_hosts args the auth bridge would
	// synthesize (Phase 1, D-C). For ProxyJump/cipher-restricted estates that must
	// own the whole arg string. Empty ⇒ the bridge synthesizes
	// "-o UserKnownHostsFile=<KnownHostsFile> -o StrictHostKeyChecking=yes". Either
	// way an operator value already present in the child env (manifest Env /
	// -env-base-extra / passthrough) is never overridden.
	AnsibleSSHCommonArgs string

	// NoAuthBridge disables the Phase 1 auth bridge: resolving a passthrough key
	// NAME to a local key FILE for ansible, and injecting ANSIBLE_SSH_COMMON_ARGS
	// to verify against the agent's known_hosts. Escape hatch for estates that
	// manage their own ansible SSH/key config; runs then behave as pre-Phase-1
	// (inventory must hand-author the key + known_hosts). Default off (bridge on).
	NoAuthBridge bool

	// LocalInventoryFile is the agent's own inventory used in "local" mode to
	// resolve a scope name → host list (T-b network-isolated segments). The
	// format is documented in resolveLocalScope (exec_local.go).
	LocalInventoryFile string

	// StateDir is a writable directory for transient run artifacts (e.g. the
	// Ansible inventory file materialized from a manifest for `-i`). Empty ⇒
	// os.TempDir(). The systemd unit runs ProtectSystem=strict with a single
	// writable StateDirectory; point this there (or rely on the STATE_DIRECTORY
	// env, which is used as the default when this is unset).
	StateDir string

	// LogRetryBudget bounds log-POST resume attempts before giving up and letting
	// the server mark the run log_stream_lost (R2.3).
	LogRetryBudget int

	// FanOut bounds parallel SSH targets within a single run (default 4).
	FanOut int

	// EnvBaseExtra is extra env-var NAMES (beyond the built-in safe-list) the
	// agent forwards from its own environment into a scoped local-toolchain
	// child env (RX.9). Operator-declared, visible in config — never silently
	// inherited. Names only; values resolve from the agent's env at run time.
	EnvBaseExtra []string

	// ExcludeSSHAuthSock drops SSH_AUTH_SOCK from the scoped child env base
	// safe-list. It is in the default base because ansible needs it for
	// agent-based SSH auth to targets, but it is a capability grant (signing
	// access to every key the runner's ssh-agent holds) — runners using
	// per-host key files instead should set this (RX.9/§6.1).
	ExcludeSSHAuthSock bool

	// ── Checkout mode (ansible-update.md Phase 2, RX.1/RX.6/RX.11) ──────────────

	// AllowCheckout opts this runner into runner-side pinned checkout. An agent
	// without it REFUSES any manifest carrying a checkout spec (defense in depth,
	// same shape as the local-mode inventory refusal). Default off.
	AllowCheckout bool
	// CheckoutRepos is the allowlist of repo identities (clone URLs) this runner
	// may check out. A checkout manifest whose repo is not in this list is
	// refused — a server compromise cannot point the runner at an arbitrary repo
	// (RX.6). Empty ⇒ no repo is allowed (checkout effectively disabled even
	// with AllowCheckout).
	CheckoutRepos []string
	// AllowWatch opts this runner into file-arrival watching (ET-D). Off by
	// default: an agent that watches directories is reading a filesystem on the
	// server's instruction, and that has to be an explicit local choice.
	AllowWatch bool
	// WatchPaths is the allowlist of directory roots this runner may watch.
	//
	// It is the security half of the feature and NOT optional in practice: the
	// SERVER supplies the globs, so without a local bound a careless or
	// compromised server could point a runner at a sensitive directory and learn
	// what lands there. An empty allowlist permits NOTHING, matching the
	// scope-grant rule that empty means zero access rather than everything.
	WatchPaths []string
	// MirrorDir is where per-repo bare mirrors are kept (persistent, fetch-updated
	// — RX.3). Empty ⇒ <StateDir>/mirrors (or <os.TempDir>/amadeus-mirrors).
	MirrorDir string
	// CheckoutToken is the read-only deploy credential the agent uses to fetch the
	// playbooks repo (RX.11 — per-runner GitLab deploy token, read_repository
	// only). Runner-local, NEVER shipped by the server. Empty ⇒ anonymous fetch
	// (works for a public/local repo). CheckoutTokenFile takes precedence.
	CheckoutToken string
	// CheckoutTokenFile is a file holding the deploy token (provisioned like
	// secrets.env). Read at resolve time; its trimmed contents override
	// CheckoutToken.
	CheckoutTokenFile string

	// GalaxyServer overrides the ansible-galaxy source for per-run requirements
	// installs (Phase 3, P6). Empty ⇒ ansible's default (public
	// galaxy.ansible.com). Point it at a private mirror/hub to keep installs
	// off the public internet.
	GalaxyServer string

	// VaultPasswordFile is a runner-local file holding the Ansible Vault
	// password (RX.13). Provisioned like secrets.env; NEVER shipped by the
	// server. When a checkout manifest arrives with UsesVault, the agent passes
	// `--vault-password-file <this>` to ansible-playbook; if UsesVault arrives and
	// this is unset, the run is refused loudly. Its value should also be
	// registered as an Cronomicon stored secret so the log redactor masks it.
	VaultPasswordFile string

	// ── Tier 2 sandbox (ansible-update.md Phase 5, RX.10/§6.3) ──────────────────

	// NoSandbox disables the per-run systemd-run scope wrapper even when a usable
	// systemd-run is present (escape hatch for environments where the wrapper
	// misbehaves). The run then executes unsandboxed and is reported as such.
	NoSandbox bool
	// SandboxMemoryMax / SandboxCPUQuota / SandboxTasksMax are the per-run cgroup
	// resource caps applied via the scope (systemd MemoryMax/CPUQuota/TasksMax
	// values, e.g. "2G", "150%", "512"). Empty ⇒ that dimension is uncapped.
	SandboxMemoryMax string
	SandboxCPUQuota  string
	SandboxTasksMax  string
	// SandboxAvailable is set at startup by probeSandbox (NOT a flag): true iff a
	// usable systemd-run scope can be created. Gates wrapping, the `sandboxed`
	// capability token, and the per-run provenance line.
	SandboxAvailable bool
}

// defaultConfig returns the built-in defaults (lowest precedence).
func defaultConfig() Config {
	return Config{
		OS:             "Linux",
		MaxConcurrent:  5,
		Inventory:      "amadeus",
		IdentityFile:   "amadeus-runner-identity.json",
		PollInterval:   60 * time.Second, // D7
		KeyMap:         map[string]string{},
		LogRetryBudget: 5,
		FanOut:         4,
	}
}

// Resolve builds the agent config from defaults, an optional config file, env
// vars (CRONOMICON_RUNNER_*), and flags, in that order of increasing precedence.
//
// args is the flag argument slice (os.Args[1:]); getenv is the environment
// lookup (os.Getenv in production, injectable in tests). The precedence is:
// flags override env override file override defaults — a value is only taken
// from a lower layer when no higher layer set it.
func Resolve(args []string, getenv func(string) string) (Config, error) {
	cfg := defaultConfig()

	// Layer 1: optional config file. The file path itself may come from a flag or
	// env; do a pre-pass to find it so file values sit below env/flags.
	filePath := getenv("CRONOMICON_RUNNER_CONFIG")
	for i, a := range args {
		if a == "-config" || a == "--config" {
			if i+1 < len(args) {
				filePath = args[i+1]
			}
		} else if after, ok := strings.CutPrefix(a, "-config="); ok {
			filePath = after
		} else if after, ok := strings.CutPrefix(a, "--config="); ok {
			filePath = after
		}
	}
	if filePath != "" {
		fileCfg, err := loadConfigFile(filePath)
		if err != nil {
			return Config{}, fmt.Errorf("config file %q: %w", filePath, err)
		}
		applyFileConfig(&cfg, fileCfg)
	}

	// Layer 2: environment (CRONOMICON_RUNNER_*).
	applyEnv(&cfg, getenv)

	// Layer 3: flags (highest precedence). We define flags whose defaults are the
	// already-resolved cfg values, so an unset flag leaves the lower layer intact.
	fs := flag.NewFlagSet("amadeus-runner", flag.ContinueOnError)
	var (
		serverURL         = fs.String("server", cfg.ServerURL, "Cronomicon server base URL")
		caCert            = fs.String("ca-cert", cfg.CACertPath, "path to a PEM CA bundle to trust for TLS")
		regToken          = fs.String("registration-token", cfg.RegistrationToken, "registration bearer token (single-use, minted per install)")
		name              = fs.String("name", cfg.Name, "runner display name (stable across restarts)")
		osFlag            = fs.String("os", cfg.OS, "runner OS: Linux or Windows")
		capsFlag          = fs.String("capabilities", strings.Join(cfg.Capabilities, ","), "comma-separated run-type capabilities (empty = auto-detect from the host's toolchains; set to narrow)")
		maxConc           = fs.Int("max-concurrent", cfg.MaxConcurrent, "max simultaneously-executing runs")
		inventory         = fs.String("inventory", cfg.Inventory, "inventory mode: amadeus or local")
		identityFile      = fs.String("identity-file", cfg.IdentityFile, "path to persist {id, apiKey}")
		pollInterval      = fs.Duration("poll-interval", cfg.PollInterval, "poll/heartbeat cadence")
		keyDir            = fs.String("key-dir", cfg.KeyDir, "directory to search for private keys named by authKeyEnvVar")
		keyMap            = fs.String("key-map", joinKeyMap(cfg.KeyMap), "authKeyEnvVar→keyfile map, NAME=path,NAME2=path2")
		knownHosts        = fs.String("known-hosts", cfg.KnownHostsFile, "OpenSSH known_hosts file for host-key verification")
		ansibleSSHArgs    = fs.String("ansible-ssh-common-args", cfg.AnsibleSSHCommonArgs, "value injected as ANSIBLE_SSH_COMMON_ARGS for ansible runs INSTEAD of the bridge default (ProxyJump/cipher estates)")
		noAuthBridge      = fs.Bool("no-auth-bridge", cfg.NoAuthBridge, "disable the auth bridge (key-name→file for ansible + known_hosts injection)")
		localInv          = fs.String("local-inventory", cfg.LocalInventoryFile, "local inventory file for 'local' mode scope resolution")
		stateDir          = fs.String("state-dir", cfg.StateDir, "writable dir for transient run artifacts (ansible -i temp file); empty ⇒ os.TempDir()")
		logRetry          = fs.Int("log-retry-budget", cfg.LogRetryBudget, "max log-POST resume attempts before giving up")
		fanOut            = fs.Int("fan-out", cfg.FanOut, "max parallel SSH targets within a run")
		envBaseExtra      = fs.String("env-base-extra", strings.Join(cfg.EnvBaseExtra, ","), "comma-separated extra env-var NAMES forwarded into a scoped local-toolchain child env")
		noSSHSock         = fs.Bool("exclude-ssh-auth-sock", cfg.ExcludeSSHAuthSock, "drop SSH_AUTH_SOCK from the scoped child env base safe-list")
		allowCheckout     = fs.Bool("allow-checkout", cfg.AllowCheckout, "opt into runner-side pinned checkout of playbook projects")
		checkoutRepos     = fs.String("checkout-repos", strings.Join(cfg.CheckoutRepos, ","), "comma-separated allowlist of repo clone URLs this runner may check out")
		allowWatch        = fs.Bool("allow-watch", cfg.AllowWatch, "opt into file-arrival watching (ET-D)")
		watchPaths        = fs.String("watch-paths", strings.Join(cfg.WatchPaths, ","), "comma-separated directory roots this runner may watch; empty permits nothing")
		mirrorDir         = fs.String("mirror-dir", cfg.MirrorDir, "directory for persistent bare git mirrors (empty ⇒ <state-dir>/mirrors)")
		checkoutToken     = fs.String("checkout-token", cfg.CheckoutToken, "read-only deploy token for fetching the playbooks repo (prefer -checkout-token-file)")
		checkoutTokenFile = fs.String("checkout-token-file", cfg.CheckoutTokenFile, "file holding the read-only deploy token")
		galaxyServer      = fs.String("galaxy-server", cfg.GalaxyServer, "ansible-galaxy source URL for per-run requirements installs (empty ⇒ public galaxy.ansible.com)")
		vaultPasswordFile = fs.String("vault-password-file", cfg.VaultPasswordFile, "runner-local Ansible Vault password file (enables vault-encrypted checkout runs)")
		noSandbox         = fs.Bool("no-sandbox", cfg.NoSandbox, "disable the per-run systemd-run scope wrapper even when available")
		sandboxMemoryMax  = fs.String("sandbox-memory-max", cfg.SandboxMemoryMax, "per-run cgroup MemoryMax (e.g. 2G); empty ⇒ uncapped")
		sandboxCPUQuota   = fs.String("sandbox-cpu-quota", cfg.SandboxCPUQuota, "per-run cgroup CPUQuota (e.g. 150%); empty ⇒ uncapped")
		sandboxTasksMax   = fs.String("sandbox-tasks-max", cfg.SandboxTasksMax, "per-run cgroup TasksMax (e.g. 512); empty ⇒ uncapped")
		_                 = fs.String("config", filePath, "path to an optional config file") // consumed in pre-pass
	)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	cfg.ServerURL = normalizeServerURL(*serverURL)
	cfg.CACertPath = *caCert
	cfg.RegistrationToken = *regToken
	cfg.Name = *name
	cfg.OS = *osFlag
	cfg.Capabilities = splitCSV(*capsFlag)
	cfg.MaxConcurrent = *maxConc
	cfg.Inventory = *inventory
	cfg.IdentityFile = *identityFile
	cfg.PollInterval = *pollInterval
	cfg.KeyDir = *keyDir
	if km, err := parseKeyMap(*keyMap); err != nil {
		return Config{}, err
	} else if len(km) > 0 || *keyMap != "" {
		cfg.KeyMap = km
	}
	cfg.KnownHostsFile = *knownHosts
	cfg.AnsibleSSHCommonArgs = *ansibleSSHArgs
	cfg.NoAuthBridge = *noAuthBridge
	cfg.LocalInventoryFile = *localInv
	cfg.StateDir = *stateDir
	cfg.LogRetryBudget = *logRetry
	cfg.FanOut = *fanOut
	cfg.EnvBaseExtra = splitCSV(*envBaseExtra)
	cfg.ExcludeSSHAuthSock = *noSSHSock
	cfg.AllowCheckout = *allowCheckout
	cfg.AllowWatch = *allowWatch
	cfg.WatchPaths = splitCSV(*watchPaths)
	cfg.CheckoutRepos = splitCSV(*checkoutRepos)
	cfg.MirrorDir = *mirrorDir
	cfg.CheckoutToken = *checkoutToken
	cfg.CheckoutTokenFile = *checkoutTokenFile
	cfg.GalaxyServer = *galaxyServer
	cfg.VaultPasswordFile = *vaultPasswordFile
	cfg.NoSandbox = *noSandbox
	cfg.SandboxMemoryMax = *sandboxMemoryMax
	cfg.SandboxCPUQuota = *sandboxCPUQuota
	cfg.SandboxTasksMax = *sandboxTasksMax

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalizeServerURL trims a trailing slash and, when the value carries no
// scheme (e.g. "amadeus.example.com" set via env or a non-bash installer that
// skipped the prepend guard), defaults it to https://. A bare host otherwise
// reaches net/http as a schemeless URL and fails at request time with the
// opaque `unsupported protocol scheme ""`; self-healing here turns that into a
// working config. An explicit http:// (or any other scheme) is left intact so
// validate() can accept http for local/dev and reject anything unusable.
func normalizeServerURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return ""
	}
	// url.Parse treats "host:port" as scheme="host" opaque=..., so key off the
	// literal "://" separator to decide whether a real scheme is present.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	return s
}

// validate checks the required fields and closed-set values.
func (c Config) validate() error {
	if c.ServerURL == "" {
		return fmt.Errorf("server URL is required (-server / CRONOMICON_RUNNER_SERVER)")
	}
	if u, err := url.Parse(c.ServerURL); err != nil {
		return fmt.Errorf("server URL %q is not a valid URL: %w", c.ServerURL, err)
	} else if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("server URL %q must start with http:// or https:// (got scheme %q)", c.ServerURL, u.Scheme)
	} else if u.Host == "" {
		return fmt.Errorf("server URL %q is missing a host", c.ServerURL)
	}
	if c.Name == "" {
		return fmt.Errorf("runner name is required (-name / CRONOMICON_RUNNER_NAME)")
	}
	// Empty Capabilities is NOT an error: unset = auto-detect the run-types
	// from the host's toolchains at startup (D1: 1B); set explicitly to narrow.
	// detectCapabilities errors when the probe finds nothing.
	if c.OS != "Linux" && c.OS != "Windows" {
		return fmt.Errorf("os must be 'Linux' or 'Windows', got %q", c.OS)
	}
	if c.Inventory != "amadeus" && c.Inventory != "local" {
		return fmt.Errorf("inventory must be 'amadeus' or 'local', got %q", c.Inventory)
	}
	if c.MaxConcurrent <= 0 {
		return fmt.Errorf("max-concurrent must be > 0, got %d", c.MaxConcurrent)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll-interval must be > 0")
	}
	if c.IdentityFile == "" {
		return fmt.Errorf("identity-file path is required")
	}
	return nil
}

// applyEnv overlays CRONOMICON_RUNNER_* environment variables onto cfg. Only set
// (non-empty) env vars override; everything else is left at its lower-layer
// value.
func applyEnv(cfg *Config, getenv func(string) string) {
	setStr := func(env string, dst *string) {
		if v := getenv(env); v != "" {
			*dst = v
		}
	}
	setStr("CRONOMICON_RUNNER_SERVER", &cfg.ServerURL)
	setStr("CRONOMICON_RUNNER_CA_CERT", &cfg.CACertPath)
	setStr("CRONOMICON_RUNNER_REGISTRATION_TOKEN", &cfg.RegistrationToken)
	setStr("CRONOMICON_RUNNER_NAME", &cfg.Name)
	setStr("CRONOMICON_RUNNER_OS", &cfg.OS)
	if v := getenv("CRONOMICON_RUNNER_CAPABILITIES"); v != "" {
		cfg.Capabilities = splitCSV(v)
	}
	if v := getenv("CRONOMICON_RUNNER_MAX_CONCURRENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrent = n
		}
	}
	setStr("CRONOMICON_RUNNER_INVENTORY", &cfg.Inventory)
	setStr("CRONOMICON_RUNNER_IDENTITY_FILE", &cfg.IdentityFile)
	if v := getenv("CRONOMICON_RUNNER_POLL_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.PollInterval = d
		}
	}
	setStr("CRONOMICON_RUNNER_KEY_DIR", &cfg.KeyDir)
	if v := getenv("CRONOMICON_RUNNER_KEY_MAP"); v != "" {
		if km, err := parseKeyMap(v); err == nil {
			cfg.KeyMap = km
		}
	}
	setStr("CRONOMICON_RUNNER_KNOWN_HOSTS", &cfg.KnownHostsFile)
	setStr("CRONOMICON_RUNNER_ANSIBLE_SSH_COMMON_ARGS", &cfg.AnsibleSSHCommonArgs)
	if v := getenv("CRONOMICON_RUNNER_NO_AUTH_BRIDGE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoAuthBridge = b
		}
	}
	setStr("CRONOMICON_RUNNER_LOCAL_INVENTORY", &cfg.LocalInventoryFile)
	setStr("CRONOMICON_RUNNER_STATE_DIR", &cfg.StateDir)
	if cfg.StateDir == "" {
		// systemd sets STATE_DIRECTORY to the unit's writable StateDirectory.
		cfg.StateDir = getenv("STATE_DIRECTORY")
	}
	if v := getenv("CRONOMICON_RUNNER_LOG_RETRY_BUDGET"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LogRetryBudget = n
		}
	}
	if v := getenv("CRONOMICON_RUNNER_FAN_OUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.FanOut = n
		}
	}
	if v := getenv("CRONOMICON_RUNNER_ENV_BASE_EXTRA"); v != "" {
		cfg.EnvBaseExtra = splitCSV(v)
	}
	if v := getenv("CRONOMICON_RUNNER_EXCLUDE_SSH_AUTH_SOCK"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.ExcludeSSHAuthSock = b
		}
	}
	if v := getenv("CRONOMICON_RUNNER_ALLOW_CHECKOUT"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AllowCheckout = b
		}
	}
	if v := getenv("CRONOMICON_RUNNER_CHECKOUT_REPOS"); v != "" {
		cfg.CheckoutRepos = splitCSV(v)
	}
	if v := getenv("CRONOMICON_RUNNER_ALLOW_WATCH"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.AllowWatch = b
		}
	}
	if v := getenv("CRONOMICON_RUNNER_WATCH_PATHS"); v != "" {
		cfg.WatchPaths = splitCSV(v)
	}
	setStr("CRONOMICON_RUNNER_MIRROR_DIR", &cfg.MirrorDir)
	setStr("CRONOMICON_RUNNER_CHECKOUT_TOKEN", &cfg.CheckoutToken)
	setStr("CRONOMICON_RUNNER_CHECKOUT_TOKEN_FILE", &cfg.CheckoutTokenFile)
	setStr("CRONOMICON_RUNNER_GALAXY_SERVER", &cfg.GalaxyServer)
	setStr("CRONOMICON_RUNNER_VAULT_PASSWORD_FILE", &cfg.VaultPasswordFile)
	if v := getenv("CRONOMICON_RUNNER_NO_SANDBOX"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoSandbox = b
		}
	}
	setStr("CRONOMICON_RUNNER_SANDBOX_MEMORY_MAX", &cfg.SandboxMemoryMax)
	setStr("CRONOMICON_RUNNER_SANDBOX_CPU_QUOTA", &cfg.SandboxCPUQuota)
	setStr("CRONOMICON_RUNNER_SANDBOX_TASKS_MAX", &cfg.SandboxTasksMax)
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// parseKeyMap parses "NAME=path,NAME2=path2" into a map. An empty string yields
// an empty map.
func parseKeyMap(s string) (map[string]string, error) {
	m := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return m, nil
	}
	for pair := range strings.SplitSeq(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("invalid key-map entry %q (want NAME=path)", pair)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, nil
}

// joinKeyMap renders a key map back to "NAME=path,..." form (for flag defaults).
func joinKeyMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

// fileConfig is the on-disk config-file shape (JSON). All fields optional.
type fileConfig struct {
	ServerURL            *string           `json:"serverUrl"`
	CACertPath           *string           `json:"caCertPath"`
	RegistrationToken    *string           `json:"registrationToken"`
	Name                 *string           `json:"name"`
	OS                   *string           `json:"os"`
	Capabilities         []string          `json:"capabilities"`
	MaxConcurrent        *int              `json:"maxConcurrent"`
	Inventory            *string           `json:"inventory"`
	IdentityFile         *string           `json:"identityFile"`
	PollInterval         *string           `json:"pollInterval"`
	KeyDir               *string           `json:"keyDir"`
	KeyMap               map[string]string `json:"keyMap"`
	KnownHostsFile       *string           `json:"knownHostsFile"`
	AnsibleSSHCommonArgs *string           `json:"ansibleSshCommonArgs"`
	NoAuthBridge         *bool             `json:"noAuthBridge"`
	LocalInventoryFile   *string           `json:"localInventoryFile"`
	StateDir             *string           `json:"stateDir"`
	LogRetryBudget       *int              `json:"logRetryBudget"`
	FanOut               *int              `json:"fanOut"`
	EnvBaseExtra         []string          `json:"envBaseExtra"`
	ExcludeSSHAuthSock   *bool             `json:"excludeSshAuthSock"`
	AllowCheckout        *bool             `json:"allowCheckout"`
	AllowWatch           *bool             `json:"allowWatch"`
	WatchPaths           []string          `json:"watchPaths"`
	CheckoutRepos        []string          `json:"checkoutRepos"`
	MirrorDir            *string           `json:"mirrorDir"`
	CheckoutToken        *string           `json:"checkoutToken"`
	CheckoutTokenFile    *string           `json:"checkoutTokenFile"`
	GalaxyServer         *string           `json:"galaxyServer"`
	VaultPasswordFile    *string           `json:"vaultPasswordFile"`
	NoSandbox            *bool             `json:"noSandbox"`
	SandboxMemoryMax     *string           `json:"sandboxMemoryMax"`
	SandboxCPUQuota      *string           `json:"sandboxCpuQuota"`
	SandboxTasksMax      *string           `json:"sandboxTasksMax"`
}

func loadConfigFile(path string) (fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fileConfig{}, err
	}
	var fc fileConfig
	if err := jsonUnmarshalStrict(data, &fc); err != nil {
		return fileConfig{}, err
	}
	return fc, nil
}

// applyFileConfig overlays the file values onto cfg (only present fields).
func applyFileConfig(cfg *Config, fc fileConfig) {
	if fc.ServerURL != nil {
		cfg.ServerURL = *fc.ServerURL
	}
	if fc.CACertPath != nil {
		cfg.CACertPath = *fc.CACertPath
	}
	if fc.RegistrationToken != nil {
		cfg.RegistrationToken = *fc.RegistrationToken
	}
	if fc.Name != nil {
		cfg.Name = *fc.Name
	}
	if fc.OS != nil {
		cfg.OS = *fc.OS
	}
	if fc.Capabilities != nil {
		cfg.Capabilities = fc.Capabilities
	}
	if fc.MaxConcurrent != nil {
		cfg.MaxConcurrent = *fc.MaxConcurrent
	}
	if fc.Inventory != nil {
		cfg.Inventory = *fc.Inventory
	}
	if fc.IdentityFile != nil {
		cfg.IdentityFile = *fc.IdentityFile
	}
	if fc.PollInterval != nil {
		if d, err := time.ParseDuration(*fc.PollInterval); err == nil {
			cfg.PollInterval = d
		}
	}
	if fc.KeyDir != nil {
		cfg.KeyDir = *fc.KeyDir
	}
	if fc.KeyMap != nil {
		cfg.KeyMap = fc.KeyMap
	}
	if fc.KnownHostsFile != nil {
		cfg.KnownHostsFile = *fc.KnownHostsFile
	}
	if fc.AnsibleSSHCommonArgs != nil {
		cfg.AnsibleSSHCommonArgs = *fc.AnsibleSSHCommonArgs
	}
	if fc.NoAuthBridge != nil {
		cfg.NoAuthBridge = *fc.NoAuthBridge
	}
	if fc.LocalInventoryFile != nil {
		cfg.LocalInventoryFile = *fc.LocalInventoryFile
	}
	if fc.StateDir != nil {
		cfg.StateDir = *fc.StateDir
	}
	if fc.LogRetryBudget != nil {
		cfg.LogRetryBudget = *fc.LogRetryBudget
	}
	if fc.FanOut != nil {
		cfg.FanOut = *fc.FanOut
	}
	if fc.EnvBaseExtra != nil {
		cfg.EnvBaseExtra = fc.EnvBaseExtra
	}
	if fc.ExcludeSSHAuthSock != nil {
		cfg.ExcludeSSHAuthSock = *fc.ExcludeSSHAuthSock
	}
	if fc.AllowCheckout != nil {
		cfg.AllowCheckout = *fc.AllowCheckout
	}
	if fc.CheckoutRepos != nil {
		cfg.CheckoutRepos = fc.CheckoutRepos
	}
	if fc.AllowWatch != nil {
		cfg.AllowWatch = *fc.AllowWatch
	}
	if fc.WatchPaths != nil {
		cfg.WatchPaths = fc.WatchPaths
	}
	if fc.MirrorDir != nil {
		cfg.MirrorDir = *fc.MirrorDir
	}
	if fc.CheckoutToken != nil {
		cfg.CheckoutToken = *fc.CheckoutToken
	}
	if fc.CheckoutTokenFile != nil {
		cfg.CheckoutTokenFile = *fc.CheckoutTokenFile
	}
	if fc.GalaxyServer != nil {
		cfg.GalaxyServer = *fc.GalaxyServer
	}
	if fc.VaultPasswordFile != nil {
		cfg.VaultPasswordFile = *fc.VaultPasswordFile
	}
	if fc.NoSandbox != nil {
		cfg.NoSandbox = *fc.NoSandbox
	}
	if fc.SandboxMemoryMax != nil {
		cfg.SandboxMemoryMax = *fc.SandboxMemoryMax
	}
	if fc.SandboxCPUQuota != nil {
		cfg.SandboxCPUQuota = *fc.SandboxCPUQuota
	}
	if fc.SandboxTasksMax != nil {
		cfg.SandboxTasksMax = *fc.SandboxTasksMax
	}
}
