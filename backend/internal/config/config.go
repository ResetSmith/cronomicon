// Package config loads typed runtime configuration from environment variables.
//
// Per T3 (single-container, single-process) all configuration is supplied via
// the environment; there is no config file. Every value has a safe default for
// local development except the security-sensitive ones, which fail closed.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/logsink"
)

// Auth modes (Phase A). trusted-header is the primary production mode (a forward-auth proxy such as Authelia
// forward-auth via the reverse proxy injects Remote-* headers); oidc is the
// retained optional relying-party flow.
const (
	AuthModeTrustedHeader = "trusted-header"
	AuthModeOIDC          = "oidc"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// HTTP server
	Addr string // listen address, e.g. ":8080"

	// Logging
	LogLevel  string // debug|info|warn|error
	LogFormat string // json|text

	// Process log file (LU-4). stdout is always written; these control the
	// additional on-disk copy, which exists so a crash is diagnosable on a host
	// where nothing is collecting stdout.
	//
	// LogFilePath empty means "{run-log dir}/amadeus.log", resolved from the
	// log-storage setting after the DB opens and re-pointed live when that
	// setting changes (LU-5). Set it to pin the process log somewhere else —
	// worth doing if the run-log tree ever moves to slower or shared storage,
	// since a continuously-appended file maps badly onto object storage.
	LogFileEnabled bool
	LogFilePath    string
	LogFileMaxMB   int // rotate at this size
	// LogFileKeep is the number of rotated generations retained; disk is bounded
	// by (Keep+1)*MaxMB. This is the only layer that can tell "unset" from
	// "explicitly 0", so the default is applied here and logsink takes the value
	// literally — 0 really does mean keep no generations.
	LogFileKeep int

	// Audit stream (LU-10). audit.log is the compliance export of the audit
	// tables, written as JSON Lines and rotated daily by UTC date. Its retention
	// is a settings knob rather than an env var (auditCompliance.retentionDays),
	// because it is a compliance window and belongs with the others.
	//
	// AuditLogPath empty means "{run-log dir}/audit.log", resolved after the DB
	// opens and re-pointed live when the log directory changes — same shape as
	// the process log.
	AuditLogEnabled bool
	AuditLogPath    string

	// Database (T1/T4) — single SQLite file on a mounted volume (architecture §5).
	DBPath string

	// Auth mode (Phase A): trusted-header (default) | oidc.
	AuthMode string

	// Trusted Header SSO (Phase A). Honor Remote-* identity headers only from a
	// peer in TrustedProxies; the header names are overridable but default to
	// the common Remote-* forward-auth convention. BootstrapAdminGroup grants admin to any user in that group
	// regardless of ad_group_mappings (first-run seeding; remove after).
	TrustedProxies      []string // CIDRs/IPs allowed to set Remote-* (default-deny if empty)
	TrustedHeaderUser   string
	TrustedHeaderEmail  string
	TrustedHeaderName   string
	TrustedHeaderGroups string
	BootstrapAdminGroup string
	LogoutRedirectURL   string // identity-provider logout endpoint the SPA navigates to on sign-out

	// Identity — OIDC relying party (A3.1/T8). Optional mode.
	OIDC OIDCConfig

	// Operator session + CSRF (T8).
	SessionHashKey  string // base64, 32+ bytes, signs the session cookie
	SessionBlockKey string // base64, 32 bytes, encrypts the session cookie
	CookieSecure    bool   // Secure attribute; true in production (HTTPS)

	// Developer mode (local preview only — NEVER enable in production).
	// DevAuth exposes a one-click login that mints a synthetic operator session,
	// bypassing SSO so the UI can be browsed before OIDC is wired up.
	// DevSeed loads representative demo data into an empty DB on boot so the
	// views render with realistic content. Both default OFF and fail closed.
	DevAuth bool
	DevSeed bool

	// Runner auth (T9) + registration (A6.1).
	RunnerBootstrapToken string // shared registration token, 24h rotation

	// Runner-agent binary distribution (runner provisioning D1). The server
	// image bakes cross-compiled amadeus-runner binaries + SHA256SUMS into this
	// directory; GET /agents/{filename} serves them so a target host can
	// install a runner with nothing but curl + the Cronomicon server. Deployments
	// without the files (bare-metal `go build`) get a clean 404 with a
	// build-it-yourself hint.
	AgentDir string

	// Runner reaper (V1.1-8.4 / R3, D4+D7). The background sweep marks a runner
	// offline after RunnerOfflineAfter without a heartbeat (last_seen_at is
	// written on each long-poll; agents poll once per minute per D7, so 5m ≈ 5
	// missed polls). A runner that stays offline beyond RunnerDeregisterAfter is
	// fully deregistered (tokens revoked, row deleted) — D4.
	RunnerOfflineAfter    time.Duration
	RunnerDeregisterAfter time.Duration

	// Stored-secret encryption (S14) — AES-256-GCM envelope encryption.
	// KEK source precedence: file (if set & readable) → env. See internal/auth/kek (B6).
	SecretKEKFile    string // mounted secret file holding the base64 KEK (preferred)
	SecretKEKEnv     string // fallback: base64 KEK supplied directly
	SecretKEKVersion int    // active KEK version written on new/updated secrets (default 1)

	// Notifications (Phase C.1) — Apprise gateway base URL (the Phase B sidecar).
	AppriseURL string

	// Vault (Phase C.3) — AppRole auth, KV v2. The client is built but stays
	// disabled (stub) unless VaultAddr + role/secret IDs are present. Role/secret
	// IDs may be supplied directly or via mounted *_FILE paths (precedence: file).
	VaultAddr         string
	VaultRoleID       string
	VaultRoleIDFile   string
	VaultSecretID     string
	VaultSecretIDFile string

	// Vault client hardening (Phase 2, D4) — every knob is dormant until its own
	// config is present; unset preserves the pre-hardening behavior exactly.
	//   VaultNamespace  — sent as X-Vault-Namespace on every request when set
	//                     (Vault Enterprise / HCP namespaces). Env overrides the
	//                     DB-backed vault_config.namespace.
	//   VaultCAFile     — a PEM bundle for a private CA; when set the client uses a
	//                     transport pinned to it instead of the system roots.
	//   VaultSecretIDWrapped — when true, VaultSecretID[File] holds a response-
	//                     wrapping token, not the secret_id itself; the client
	//                     unwraps it once via sys/wrapping/unwrap and caches the
	//                     real secret_id in memory. Static long-lived secret_id
	//                     (unwrapped=false) remains the default.
	VaultNamespace       string
	VaultCAFile          string
	VaultSecretIDWrapped bool

	// OutboundAllowPrivate governs the SSRF egress guard on operator-configured
	// outbound targets (Vault, GitLab, S3/MinIO, Apprise, OIDC). SU-7: cloud-metadata,
	// loopback, and link-local are ALWAYS blocked. When true (default, SU-Q4(a)) the
	// RFC-1918 / unique-local ranges are ALLOWED — required here, since Vault/GitLab are
	// internal hosts. Set false for the stricter posture that also blocks private ranges
	// (only for deployments whose outbound targets are all public). The S3 IAM-role
	// credential provider is exempt so EC2/ECS IMDS still works.
	OutboundAllowPrivate bool
	// OutboundAllowLoopback re-permits loopback outbound (SU-7). Default false —
	// loopback is a prime SSRF target. Set true ONLY for a Vault-agent loopback
	// sidecar (Vault addr on 127.0.0.1); it weakens the guard for every outbound
	// client, so prefer a non-loopback Vault address where possible.
	OutboundAllowLoopback bool

	// GitLab integration (B3).
	GitLabBaseURL string
	WebhookSecret string
	// GitLabWriteBranch is the GitOps branch used for BOTH sync-read and
	// publish-write (V1.1-10). Empty ⇒ fall back to the DB-backed
	// gitlab_config.write_branch, then "main". See gitlab.Service.writeBranch.
	GitLabWriteBranch string

	// SSH executor (execution-update.md EX.6). Opt-in: when enabled the app
	// holds SSH private keys and opens outbound SSH to job targets. Default off.
	SSHExecutorEnabled     bool
	SSHExecutorConcurrency int

	// SecretsInjectionEnabled is the one-release kill-switch for dispatch-time
	// reference injection (vault-integration.md D5). Default ON: when a run's
	// reference bindings resolve, their values are injected into the run env. The
	// name is spelled PLURAL (SECRETS_) so it sits OUTSIDE the reserved
	// CRONOMICON_SECRET_* reference prefix — exact-prefix matching (envref) keeps
	// config and references disjoint. Set CRONOMICON_SECRETS_INJECTION_ENABLED=false
	// to hard-disable injection for a release.
	SecretsInjectionEnabled bool
	// SSHExecutorStaleAfter bounds the periodic SSH-orphan reaper (PP-H2): an
	// executor='ssh' run still 'running' longer than this is reconciled to
	// failure (executor_lost). Must stay safely above the longest plausible job
	// (its A12 timeout) so it never kills a live run — the synchronous startup
	// sweep, not this window, provides fast crash recovery. Default 24h.
	SSHExecutorStaleAfter time.Duration

	// MaxRunLogBytes caps the cumulative size of a single run's ingested log
	// (SU-9). The log-ingest endpoint is intentionally exempt from the global 2 MiB
	// body cap (a run streams many chunks), which removed the only ceiling on
	// per-run log growth — a rogue/compromised runner with a valid token could
	// otherwise fill the disk. Once a run's persisted log reaches this size the
	// ingest handler stops appending and returns 413. Default 512 MiB; 0 disables
	// the cap.
	MaxRunLogBytes int

	// Retention (A4) — per-table day knobs. Since LU-2 these are the *bootstrap
	// defaults* only: on first run they seed the auditCompliance settings blob,
	// which is authoritative thereafter (LU-Q3(a)). Editing the env on an
	// already-seeded deployment changes nothing; edit the Settings panel.
	RetentionRunsDays      int
	RetentionChangeLogDays int
	// RetentionLogFilesDays bootstraps the on-disk run-log window (LU-1). It has
	// no pre-LU-2 behaviour to preserve, so the default matches the settings blob.
	RetentionLogFilesDays int

	// Backups (A4) — S3 target for nightly VACUUM INTO upload (B8). Credentials
	// come from the environment (not the DB) so the backup path needs no secret
	// decryption and stays out of the settings table.
	BackupS3Bucket    string
	BackupS3Endpoint  string // host:port; empty ⇒ AWS S3 (s3.<region>.amazonaws.com)
	BackupS3Region    string
	BackupS3AccessKey string
	BackupS3SecretKey string
	BackupS3UseSSL    bool
	// BackupS3CAFile is an optional PEM bundle for a private S3 node signed by an
	// internal CA (the backup twin of the log archive's pasted CA bundle).
	BackupS3CAFile string
	// BackupAt is the daily wall-clock UTC time ("HH:MM") for the retention/backup
	// sweep (PP-H6). Default 02:00.
	BackupAt string
}

// OIDCConfig holds the OIDC relying-party settings.
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// Enabled reports whether enough OIDC config is present to wire the login flow.
// When false the server still boots (degraded) but operator endpoints that
// require a session will reject — the identity provider is the one hard-required dependency
// for real use (T12), but we don't want a missing issuer to crash local dev.
func (o OIDCConfig) Enabled() bool {
	return o.Issuer != "" && o.ClientID != "" && o.ClientSecret != "" && o.RedirectURL != ""
}

// Load reads configuration from the environment, applying defaults.
func Load() (*Config, error) {
	// The KEK is app config, not a store secret, so it lives under CRONOMICON_KEK*,
	// outside the reserved CRONOMICON_SECRET_* reference prefix (namespace plan
	// 2026-07-20, D6).
	kekFile := env("CRONOMICON_KEK_FILE", "")
	kekEnv := env("CRONOMICON_KEK", "")
	kekVersion := envInt("CRONOMICON_KEK_VERSION", 1)

	c := &Config{
		Addr:                env("CRONOMICON_ADDR", ":8080"),
		LogLevel:            env("CRONOMICON_LOG_LEVEL", "info"),
		LogFormat:           env("CRONOMICON_LOG_FORMAT", "json"),
		LogFileEnabled:      envBool("CRONOMICON_LOG_FILE_ENABLED", true),
		LogFilePath:         env("CRONOMICON_LOG_FILE", ""),
		LogFileMaxMB:        envInt("CRONOMICON_LOG_FILE_MAX_MB", 64),
		LogFileKeep:         envInt("CRONOMICON_LOG_FILE_KEEP", logsink.DefaultKeep),
		AuditLogEnabled:     envBool("CRONOMICON_AUDIT_LOG_ENABLED", true),
		AuditLogPath:        env("CRONOMICON_AUDIT_LOG", ""),
		DBPath:              env("CRONOMICON_DB_PATH", "/var/lib/amadeus/amadeus.db"),
		AuthMode:            env("CRONOMICON_AUTH_MODE", AuthModeTrustedHeader),
		TrustedProxies:      envList("CRONOMICON_TRUSTED_PROXIES"),
		TrustedHeaderUser:   env("CRONOMICON_TRUSTED_HEADER_USER", "Remote-User"),
		TrustedHeaderEmail:  env("CRONOMICON_TRUSTED_HEADER_EMAIL", "Remote-Email"),
		TrustedHeaderName:   env("CRONOMICON_TRUSTED_HEADER_NAME", "Remote-Name"),
		TrustedHeaderGroups: env("CRONOMICON_TRUSTED_HEADER_GROUPS", "Remote-Groups"),
		BootstrapAdminGroup: env("CRONOMICON_BOOTSTRAP_ADMIN_GROUP", ""),
		LogoutRedirectURL:   env("CRONOMICON_LOGOUT_REDIRECT_URL", ""),
		OIDC: OIDCConfig{
			Issuer:       env("CRONOMICON_OIDC_ISSUER", ""),
			ClientID:     env("CRONOMICON_OIDC_CLIENT_ID", ""),
			ClientSecret: env("CRONOMICON_OIDC_CLIENT_SECRET", ""),
			RedirectURL:  env("CRONOMICON_OIDC_REDIRECT_URL", ""),
		},
		SessionHashKey:          env("CRONOMICON_SESSION_HASH_KEY", ""),
		SessionBlockKey:         env("CRONOMICON_SESSION_BLOCK_KEY", ""),
		CookieSecure:            envBool("CRONOMICON_COOKIE_SECURE", true),
		DevAuth:                 envBool("CRONOMICON_DEV_AUTH", false),
		DevSeed:                 envBool("CRONOMICON_DEV_SEED", false),
		RunnerBootstrapToken:    env("CRONOMICON_RUNNER_BOOTSTRAP_TOKEN", ""),
		AgentDir:                env("CRONOMICON_AGENT_DIR", "/usr/share/amadeus/agents"),
		RunnerOfflineAfter:      envDuration("CRONOMICON_RUNNER_OFFLINE_AFTER", 5*time.Minute),
		RunnerDeregisterAfter:   envDuration("CRONOMICON_RUNNER_DEREGISTER_AFTER", 336*time.Hour), // 14d (D4)
		SecretKEKFile:           kekFile,
		SecretKEKEnv:            kekEnv,
		SecretKEKVersion:        kekVersion,
		AppriseURL:              env("CRONOMICON_APPRISE_URL", ""),
		VaultAddr:               env("CRONOMICON_VAULT_ADDR", ""),
		VaultRoleID:             env("CRONOMICON_VAULT_ROLE_ID", ""),
		VaultRoleIDFile:         env("CRONOMICON_VAULT_ROLE_ID_FILE", ""),
		VaultSecretID:           env("CRONOMICON_VAULT_SECRET_ID", ""),
		VaultSecretIDFile:       env("CRONOMICON_VAULT_SECRET_ID_FILE", ""),
		VaultNamespace:          env("CRONOMICON_VAULT_NAMESPACE", ""),
		VaultCAFile:             env("CRONOMICON_VAULT_CA_FILE", ""),
		OutboundAllowPrivate:    envBool("CRONOMICON_OUTBOUND_ALLOW_PRIVATE", true),
		OutboundAllowLoopback:   envBool("CRONOMICON_OUTBOUND_ALLOW_LOOPBACK", false),
		VaultSecretIDWrapped:    envBool("CRONOMICON_VAULT_SECRET_ID_WRAPPED", false),
		GitLabBaseURL:           env("CRONOMICON_GITLAB_BASE_URL", ""),
		WebhookSecret:           env("CRONOMICON_GITLAB_WEBHOOK_SECRET", ""),
		GitLabWriteBranch:       env("CRONOMICON_GITLAB_WRITE_BRANCH", ""),
		SSHExecutorEnabled:      envBool("CRONOMICON_SSH_EXECUTOR_ENABLED", false),
		SSHExecutorConcurrency:  envInt("CRONOMICON_SSH_EXECUTOR_CONCURRENCY", 4),
		SecretsInjectionEnabled: envBool("CRONOMICON_SECRETS_INJECTION_ENABLED", true),
		SSHExecutorStaleAfter:   envDuration("CRONOMICON_SSH_EXECUTOR_STALE_AFTER", 24*time.Hour),
		MaxRunLogBytes:          envInt("CRONOMICON_MAX_RUN_LOG_BYTES", 512<<20),
		RetentionRunsDays:       envInt("CRONOMICON_RETENTION_RUNS_DAYS", 90),
		RetentionChangeLogDays:  envInt("CRONOMICON_RETENTION_CHANGELOG_DAYS", 365),
		RetentionLogFilesDays:   envInt("CRONOMICON_RETENTION_LOG_FILES_DAYS", 90),
		BackupS3Bucket:          env("CRONOMICON_BACKUP_S3_BUCKET", ""),
		BackupS3Endpoint:        env("CRONOMICON_BACKUP_S3_ENDPOINT", ""),
		BackupS3Region:          env("CRONOMICON_BACKUP_S3_REGION", "us-east-1"),
		BackupS3AccessKey:       env("CRONOMICON_BACKUP_S3_ACCESS_KEY", ""),
		BackupS3SecretKey:       env("CRONOMICON_BACKUP_S3_SECRET_KEY", ""),
		BackupS3UseSSL:          envBool("CRONOMICON_BACKUP_S3_USE_SSL", true),
		BackupS3CAFile:          env("CRONOMICON_BACKUP_S3_CA_FILE", ""),
		BackupAt:                env("CRONOMICON_BACKUP_AT", "02:00"),
	}

	if c.SecretKEKVersion < 1 {
		return nil, fmt.Errorf("invalid CRONOMICON_KEK_VERSION %d (must be >= 1)", c.SecretKEKVersion)
	}
	if !validLogLevel(c.LogLevel) {
		return nil, fmt.Errorf("invalid CRONOMICON_LOG_LEVEL %q (want debug|info|warn|error)", c.LogLevel)
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		return nil, fmt.Errorf("invalid CRONOMICON_LOG_FORMAT %q (want json|text)", c.LogFormat)
	}
	// A relative process-log path would resolve against the process's working
	// directory, which differs between a systemd unit and a container — the one
	// place an operator must not have to guess where their logs went.
	if c.LogFilePath != "" && !filepath.IsAbs(c.LogFilePath) {
		return nil, fmt.Errorf("invalid CRONOMICON_LOG_FILE %q (must be an absolute path)", c.LogFilePath)
	}
	if c.LogFileMaxMB < 1 {
		return nil, fmt.Errorf("invalid CRONOMICON_LOG_FILE_MAX_MB %d (must be >= 1)", c.LogFileMaxMB)
	}
	if c.LogFileKeep < 0 {
		return nil, fmt.Errorf("invalid CRONOMICON_LOG_FILE_KEEP %d (must be >= 0)", c.LogFileKeep)
	}
	if c.AuditLogPath != "" && !filepath.IsAbs(c.AuditLogPath) {
		return nil, fmt.Errorf("invalid CRONOMICON_AUDIT_LOG %q (must be an absolute path)", c.AuditLogPath)
	}

	switch c.AuthMode {
	case AuthModeTrustedHeader, AuthModeOIDC:
	default:
		return nil, fmt.Errorf("invalid CRONOMICON_AUTH_MODE %q (want %s|%s)", c.AuthMode, AuthModeTrustedHeader, AuthModeOIDC)
	}
	for _, p := range c.TrustedProxies {
		if _, err := parseCIDRorIP(p); err != nil {
			return nil, fmt.Errorf("invalid CRONOMICON_TRUSTED_PROXIES entry %q: %w", p, err)
		}
	}
	// Default-deny: in trusted-header mode the allowlist is the load-bearing
	// control (anyone who can reach the app port could otherwise spoof
	// Remote-Groups). Refuse to boot wide-open. The local dev bypass
	// (CRONOMICON_DEV_AUTH) is the documented no-proxy escape hatch and is exempt.
	if c.AuthMode == AuthModeTrustedHeader && !c.DevAuth && len(c.TrustedProxies) == 0 {
		return nil, fmt.Errorf("CRONOMICON_AUTH_MODE=%s requires CRONOMICON_TRUSTED_PROXIES "+
			"(comma-separated CIDR/IP allowlist of the reverse proxy); refusing to boot wide-open",
			AuthModeTrustedHeader)
	}
	return c, nil
}

// VaultAppRole resolves the AppRole credentials, preferring mounted *_FILE
// paths over the inline env values (the file form keeps secrets out of the
// process environment). ok is false when Vault is not fully configured, in
// which case the caller keeps the stub client (no behavior change — decision #8).
func (c *Config) VaultAppRole() (roleID, secretID string, ok bool) {
	if c.VaultAddr == "" {
		return "", "", false
	}
	roleID = fileOrValue(c.VaultRoleIDFile, c.VaultRoleID)
	secretID = fileOrValue(c.VaultSecretIDFile, c.VaultSecretID)
	if roleID == "" || secretID == "" {
		return "", "", false
	}
	return roleID, secretID, true
}

// fileOrValue returns the trimmed contents of path when set and readable,
// otherwise the inline value.
func fileOrValue(path, value string) string {
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return value
}

// parseCIDRorIP accepts either a CIDR ("10.0.0.0/8") or a bare IP ("10.0.0.4",
// normalized to a /32 or /128) and returns the network it denotes.
func parseCIDRorIP(s string) (*net.IPNet, error) {
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not a valid CIDR or IP address")
	}
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

// TrustedProxyNets parses TrustedProxies into networks. The values are validated
// in Load, so this never errors in practice; callers can ignore the error.
func (c *Config) TrustedProxyNets() []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(c.TrustedProxies))
	for _, p := range c.TrustedProxies {
		if n, err := parseCIDRorIP(p); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

func validLogLevel(s string) bool {
	switch strings.ToLower(s) {
	case "debug", "info", "warn", "error":
		return true
	}
	return false
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// envList reads a comma-separated env var into a trimmed, non-empty slice.
// Returns nil when unset or blank.
func envList(key string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// envDuration reads a Go duration string (e.g. "5m", "336h") from the env,
// falling back to def when unset or unparseable.
func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
