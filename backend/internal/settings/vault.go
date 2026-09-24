package settings

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

type VaultConfig struct {
	Addr       string `json:"addr"`
	AuthMethod string `json:"authMethod"`       // approle | token
	RoleId     string `json:"roleId,omitempty"` // AppRole only; unused under token auth
	RoleIdSet  bool   `json:"roleIdSet"`
	// SecretId is write-only and carries whichever credential the auth method
	// needs: the AppRole secret_id, or the Vault token under token auth.
	SecretId       string  `json:"secretId,omitempty"`
	SecretIdSet    bool    `json:"secretIdSet"`
	Namespace      *string `json:"namespace"`
	Status         string  `json:"status"` // ok | degraded | unconfigured
	LastModifiedBy string  `json:"lastModifiedBy,omitempty"`
	LastModifiedAt string  `json:"lastModifiedAt,omitempty"`
}

// GetVaultConfig reads the vault_config singleton.
func GetVaultConfig(ctx context.Context, database *sql.DB, appCfg *config.Config) (*VaultConfig, error) {
	row := database.QueryRowContext(ctx, `
		SELECT addr, auth_method, role_id, secret_id_enc, namespace,
		       last_modified_by, last_modified_at
		FROM vault_config WHERE id=1`)
	var addr, authMethod, roleId, secretIdEnc, namespace, lastModBy, lastModAt sql.NullString

	var dbExists = true
	if err := row.Scan(&addr, &authMethod, &roleId, &secretIdEnc, &namespace, &lastModBy, &lastModAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			dbExists = false
		} else {
			return nil, fmt.Errorf("read vault_config: %w", err)
		}
	}

	cfg := &VaultConfig{
		AuthMethod: "approle",
		Status:     "unconfigured",
	}

	if dbExists {
		cfg.Addr = addr.String
		cfg.AuthMethod = kvStrOr(authMethod.String, "approle")
		cfg.LastModifiedBy = lastModBy.String
		cfg.LastModifiedAt = lastModAt.String
		if namespace.Valid {
			val := namespace.String
			cfg.Namespace = &val
		}

		if roleId.Valid && roleId.String != "" {
			decrypted, err := secrets.DecryptString(appCfg, roleId.String)
			if err == nil {
				cfg.RoleId = decrypted
				cfg.RoleIdSet = true
			}
		}
		if secretIdEnc.Valid && secretIdEnc.String != "" {
			cfg.SecretIdSet = true
		}
	}

	// Apply environment overrides if set (E.3)
	var finalAddr = cfg.Addr
	if appCfg.VaultAddr != "" {
		finalAddr = appCfg.VaultAddr
		cfg.Addr = finalAddr
	}

	var finalRoleID = cfg.RoleId
	var finalSecretID = ""
	envRoleID, envSecretID, envOk := appCfg.VaultAppRole()
	if envOk {
		finalRoleID = envRoleID
		finalSecretID = envSecretID
		cfg.RoleId = finalRoleID
		cfg.RoleIdSet = true
		cfg.SecretIdSet = true
		// Env creds are AppRole by construction, and ResolveVaultRuntime gives them
		// precedence — so report the method actually in force, not the stored one.
		cfg.AuthMethod = "approle"
	} else {
		// Decrypt secret ID from DB if not overridden by env
		if secretIdEnc.Valid && secretIdEnc.String != "" {
			decrypted, err := secrets.DecryptString(appCfg, secretIdEnc.String)
			if err == nil {
				finalSecretID = decrypted
			}
		}
	}

	// Calculate Vault Status
	cfg.Status = checkVaultStatus(finalAddr, cfg.AuthMethod, finalRoleID, finalSecretID, appCfg.VaultCAFile,
		httpx.EgressPolicy{AllowPrivate: appCfg.OutboundAllowPrivate, AllowLoopback: appCfg.OutboundAllowLoopback})

	return cfg, nil
}

// UpdateVaultConfig updates the vault_config singleton.
func UpdateVaultConfig(ctx context.Context, database *sql.DB, appCfg *config.Config, inp VaultConfig, actor string) (*VaultConfig, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	// Validate before the SQL CHECK constraint can turn a bad enum into a 500.
	if inp.AuthMethod != "" && inp.AuthMethod != "approle" && inp.AuthMethod != "token" {
		return nil, &ValidationError{Field: "authMethod", Message: "authMethod must be 'approle' or 'token'"}
	}
	if inp.AuthMethod == "" {
		inp.AuthMethod = "approle"
	}

	// Read the current row once: both credentials are "leave blank to keep the
	// stored value", and the stored auth method decides whether keeping it is even
	// meaningful (see below).
	var prevAuth, prevRole, prevSecret sql.NullString
	err := database.QueryRowContext(ctx,
		`SELECT auth_method, role_id, secret_id_enc FROM vault_config WHERE id=1`).
		Scan(&prevAuth, &prevRole, &prevSecret)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("read existing vault_config: %w", err)
	}
	// A stored credential belongs to the auth method it was entered under: an
	// AppRole secret_id is not a Vault token. Carrying it across a method switch
	// would wire a client that 403s on every read with no hint why, so a switch
	// requires the operator to supply the new credential.
	methodChanged := prevAuth.Valid && prevAuth.String != "" && prevAuth.String != inp.AuthMethod

	// role_id is NOT NULL with a '' default (migration 060), so it must be bound as
	// a string and never as a nil *string: token auth sends no role ID, and binding
	// NULL failed the constraint on every first save under token auth (a column
	// DEFAULT does not apply when a value is supplied explicitly).
	var roleIdEnc string
	if inp.RoleId != "" {
		token, encErr := secrets.EncryptString(appCfg, inp.RoleId)
		if encErr != nil {
			return nil, fmt.Errorf("encrypt RoleId: %w", encErr)
		}
		roleIdEnc = token
	} else {
		roleIdEnc = prevRole.String
	}

	var secretIdEnc *string
	if inp.SecretId != "" {
		token, encErr := secrets.EncryptString(appCfg, inp.SecretId)
		if encErr != nil {
			return nil, fmt.Errorf("encrypt SecretId: %w", encErr)
		}
		secretIdEnc = &token
	} else if !methodChanged && prevSecret.Valid && prevSecret.String != "" {
		secretIdEnc = &prevSecret.String
	}

	var nsVal *string
	if inp.Namespace != nil && *inp.Namespace != "" {
		nsVal = inp.Namespace
	}

	_, err = database.ExecContext(ctx, `
		INSERT INTO vault_config
		  (id, addr, auth_method, role_id, secret_id_enc, namespace, last_modified_by, last_modified_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  addr=excluded.addr, auth_method=excluded.auth_method,
		  role_id=excluded.role_id, secret_id_enc=excluded.secret_id_enc,
		  namespace=excluded.namespace,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		inp.Addr, inp.AuthMethod, roleIdEnc, secretIdEnc, nsVal, actor, now,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert vault_config: %w", err)
	}

	secrets.RedactionSourceChanged() // AM-4b: before the audit row, which the dictionary must already cover
	_ = WriteChangeLog(ctx, database, actor, "Settings", "updated", "HashiCorp Vault Connection", "")
	return GetVaultConfig(ctx, database, appCfg)
}

// vaultCredsComplete reports whether the credentials for authMethod are fully
// present. Token auth needs only the token (carried in the secret_id slot, the
// same write-only field the UI collects it in); AppRole needs both halves. This
// is the single definition of "configured" shared by the settings status badge
// and ResolveVaultRuntime, so the badge cannot say ok while the client stays a
// stub, or vice versa.
func vaultCredsComplete(authMethod, roleID, secretID string) bool {
	if authMethod == "token" {
		return secretID != ""
	}
	return roleID != "" && secretID != ""
}

// checkVaultStatus probes Vault's unauthenticated sys/health endpoint. A real
// AppRole login here would mint a fresh Vault token on every settings-page
// view (token sprawl); sys/health answers reachability without authenticating.
// 200 = active, 429 = standby — both reachable and serviceable.
func checkVaultStatus(addr, authMethod, roleID, secretID, caFile string, egress httpx.EgressPolicy) string {
	if addr == "" || !vaultCredsComplete(authMethod, roleID, secretID) {
		return "unconfigured"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	// SU-7: guard egress (SSRF) — the probe dials the operator-configured Vault addr.
	// Also honor AMADEUS_VAULT_CA_FILE, which this probe previously ignored (it fell
	// back to system roots, inconsistent with the KV client) — a private-CA Vault
	// would then always read "degraded".
	if caFile != "" {
		if tr, err := secrets.CATransport(caFile); err == nil {
			client.Transport = httpx.SafeTransport(tr, egress)
		} else {
			client.Transport = httpx.SafeTransport(nil, egress)
		}
	} else {
		client.Transport = httpx.SafeTransport(nil, egress)
	}
	url := strings.TrimRight(addr, "/") + "/v1/sys/health"
	resp, err := client.Get(url)
	if err != nil {
		return "degraded"
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests {
		return "ok"
	}
	return "degraded"
}

// VaultRuntime is the effective Vault connection Cronomicon should use: the
// credentials plus the auth method that gives them meaning. Under AuthMethod
// "token", SecretID holds the Vault token and RoleID is empty.
type VaultRuntime struct {
	Addr       string
	AuthMethod string // approle | token
	RoleID     string
	SecretID   string
}

// ResolveVaultRuntime returns the effective Vault credentials for wiring the
// client: env (AMADEUS_VAULT_*) wins when fully set (E.3 precedence, and is
// always AppRole); otherwise the DB-backed vault_config is used with its stored
// auth method. ok is false when neither source yields an addr plus the
// credentials that method requires — Vault operations then fail "unavailable"
// (decision #8). The dynamic client re-runs this per operation, so a
// vault_config change takes effect on the next Vault operation, no restart.
func ResolveVaultRuntime(ctx context.Context, database *sql.DB, appCfg *config.Config) (rt VaultRuntime, ok bool) {
	if envRole, envSecret, envOk := appCfg.VaultAppRole(); envOk && appCfg.VaultAddr != "" {
		return VaultRuntime{Addr: appCfg.VaultAddr, AuthMethod: "approle", RoleID: envRole, SecretID: envSecret}, true
	}

	row := database.QueryRowContext(ctx, `
		SELECT addr, auth_method, role_id, secret_id_enc FROM vault_config WHERE id=1`)
	var dbAddr, authMethod, roleIDEnc, secretIDEnc sql.NullString
	if err := row.Scan(&dbAddr, &authMethod, &roleIDEnc, &secretIDEnc); err != nil {
		return VaultRuntime{}, false
	}
	method := kvStrOr(authMethod.String, "approle")
	if dbAddr.String == "" {
		return VaultRuntime{}, false
	}
	// Decrypt what the method needs. Token auth stores nothing in role_id, so a
	// blank/absent role there is normal, not a misconfiguration.
	var role string
	if method != "token" {
		if !roleIDEnc.Valid || roleIDEnc.String == "" {
			return VaultRuntime{}, false
		}
		decrypted, err := secrets.DecryptString(appCfg, roleIDEnc.String)
		if err != nil {
			return VaultRuntime{}, false
		}
		role = decrypted
	}
	if !secretIDEnc.Valid || secretIDEnc.String == "" {
		return VaultRuntime{}, false
	}
	secret, err := secrets.DecryptString(appCfg, secretIDEnc.String)
	if err != nil {
		return VaultRuntime{}, false
	}
	if !vaultCredsComplete(method, role, secret) {
		return VaultRuntime{}, false
	}
	return VaultRuntime{Addr: dbAddr.String, AuthMethod: method, RoleID: role, SecretID: secret}, true
}

// ResolveVaultNamespace returns the effective Vault namespace for the client's
// X-Vault-Namespace header (D4): env (AMADEUS_VAULT_NAMESPACE) wins when set,
// otherwise the DB-backed vault_config.namespace. Empty ⇒ no namespace header
// (single-namespace / OSS Vault) — the dormant-until-configured default.
func ResolveVaultNamespace(ctx context.Context, database *sql.DB, appCfg *config.Config) string {
	if appCfg.VaultNamespace != "" {
		return appCfg.VaultNamespace
	}
	var ns sql.NullString
	if err := database.QueryRowContext(ctx,
		`SELECT namespace FROM vault_config WHERE id=1`).Scan(&ns); err != nil {
		return ""
	}
	return strings.TrimSpace(ns.String)
}

// vaultClientCache is the process-wide, config-keyed cache of the wired Vault
// client (H4). WireVaultClient is called from every subsystem that resolves
// vault-source rows (API server, SSH executor, runner, SSH-key backfill, and the
// probe-only executor), and each previously built its OWN httpVaultClient. With
// AMADEUS_VAULT_SECRET_ID_WRAPPED=true the wrapping token is single-use: whichever
// client unwrapped it first won, and every other client 400'd forever on its own
// resolveSecretID — so the losing executor failed closed on every vault-source
// run. Caching by resolved config makes all callers with the same config (the
// only real case at runtime) share ONE client and ONE unwrap; distinct configs
// (e.g. per-test mock Vaults on distinct addresses) still get distinct clients, so
// there is no cross-test bleed.
var (
	vaultClientMu    sync.Mutex
	vaultClientCache = map[string]secrets.VaultClient{}
	// vaultClientErrKeys dedupes the invalid-hardening-config error log by config
	// key: the dynamic client re-resolves per operation, and a persistently bad
	// config (e.g. unreadable CA bundle) must not emit one error line per secret
	// resolution. The build is still retried every time — the CA FILE may be fixed
	// in place without the key (its path) changing — only the logging is once-per-key.
	vaultClientErrKeys = map[string]bool{}
)

// WireVaultClient attaches the app's Vault client to sec: a dynamic client that
// resolves the CURRENT env-or-DB credentials (ResolveVaultRuntime: AppRole, or a
// static token when the stored auth method says so) plus the D4 hardening knobs
// (namespace, private CA, wrapped secret_id) on every Vault operation. Shared by
// the API server, the SSH executor, AND the runner so all three resolve
// vault-source rows (secrets and, per P2.4, SSH credentials) through an
// identically-configured client rather than each building its own. The underlying
// client is a process singleton keyed on the resolved config (H4), so a
// response-wrapped secret_id is unwrapped exactly once no matter how many
// subsystems wire a client — while a vault_config CHANGE keys a fresh client, so
// editing Vault settings (first-time setup, a rotated token or secret_id) takes
// effect on the next Vault operation with no restart. When Vault is unconfigured
// or the hardening config is invalid, operations fail with the stub's
// "vault unavailable" error and VaultConfigured reports false; a bad CA bundle
// still fails loud rather than silently trusting system roots the operator did
// not intend. Returns sec for chaining.
func WireVaultClient(_ context.Context, database *sql.DB, appCfg *config.Config, sec *secrets.Service, log *slog.Logger) *secrets.Service {
	sec.WithVaultClient(&dynamicVaultClient{db: database, cfg: appCfg, log: log})
	return sec
}

// dynamicVaultClient is the VaultClient WireVaultClient installs: a thin
// indirection that resolves the current config through the process-wide cache on
// EVERY operation instead of freezing whatever config existed when the holding
// service was constructed. Before it, the runner service, SSH executor, and API
// secrets service wired a concrete client once at startup, so a vault_config
// edit (rotate a token, fix an address, configure Vault for the first time)
// reached the per-request paths (capabilities, run-detail redaction) but NOT
// dispatch until a restart — with the status badge meanwhile probing the new
// config and reading "ok". The per-operation cost is two SQLite point reads
// (runtime + namespace), noise next to the Vault HTTP round-trip that follows;
// an unchanged config hits the cache and keeps its client, token cache, and
// single-use unwrap.
type dynamicVaultClient struct {
	db  *sql.DB
	cfg *config.Config
	log *slog.Logger
}

// resolve returns the client for the config as of now, or nil when Vault is
// unconfigured/misconfigured. Operations arrive without a context (the
// VaultClient interface predates one), so the config reads get their own bounded
// background context — never a request context, since the resolved client
// outlives any request.
func (d *dynamicVaultClient) resolve() secrets.VaultClient {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return sharedVaultClient(ctx, d.db, d.cfg, d.log)
}

func (d *dynamicVaultClient) Fetch(vaultRef string) (string, error) {
	vc := d.resolve()
	if vc == nil {
		return "", secrets.ErrVaultUnavailable
	}
	return vc.Fetch(vaultRef)
}

func (d *dynamicVaultClient) Write(vaultPath, value string) error {
	vc := d.resolve()
	if vc == nil {
		return secrets.ErrVaultUnavailable
	}
	return vc.Write(vaultPath, value)
}

// Configured is the live L7 signal secrets.Service.VaultConfigured asks for:
// whether resolving the CURRENT config yields a usable client.
func (d *dynamicVaultClient) Configured() bool { return d.resolve() != nil }

// sharedVaultClient resolves the config and returns the process-singleton Vault
// client for it (building it at most once per distinct config), or nil when Vault
// is unconfigured or the hardening config is invalid — the dynamic client then
// fails the operation with ErrVaultUnavailable.
func sharedVaultClient(ctx context.Context, database *sql.DB, appCfg *config.Config, log *slog.Logger) secrets.VaultClient {
	rt, ok := ResolveVaultRuntime(ctx, database, appCfg)
	if !ok {
		return nil
	}
	opts := secrets.VaultOptions{
		AuthMethod: rt.AuthMethod,
		Namespace:  ResolveVaultNamespace(ctx, database, appCfg),
		CAFile:     appCfg.VaultCAFile,
		// Response wrapping wraps an AppRole secret_id; it has no meaning for a
		// static token, and the client rejects the pairing outright.
		SecretIDWrapped: appCfg.VaultSecretIDWrapped && rt.AuthMethod != "token",
		Egress:          httpx.EgressPolicy{AllowPrivate: appCfg.OutboundAllowPrivate, AllowLoopback: appCfg.OutboundAllowLoopback},
	}
	// Key on every field that changes the client's identity, including secretID —
	// with a wrapped secret_id that IS the single-use wrapping token, so all callers
	// sharing the config share the one client that consumes it.
	key := strings.Join([]string{rt.Addr, rt.AuthMethod, rt.RoleID, rt.SecretID, opts.Namespace, opts.CAFile,
		fmt.Sprintf("%t", opts.SecretIDWrapped), fmt.Sprintf("%t/%t", opts.Egress.AllowPrivate, opts.Egress.AllowLoopback)}, "\x00")

	vaultClientMu.Lock()
	defer vaultClientMu.Unlock()
	if vc, cached := vaultClientCache[key]; cached {
		return vc
	}
	vc, err := secrets.NewVaultClientWithOptions(rt.Addr, rt.RoleID, rt.SecretID, opts)
	if err != nil {
		if log != nil && !vaultClientErrKeys[key] {
			vaultClientErrKeys[key] = true
			log.Error("vault client disabled: invalid hardening config", "error", err)
		}
		return nil
	}
	vaultClientCache[key] = vc
	if log != nil {
		log.Info("vault client enabled", "authMethod", rt.AuthMethod, "addr", rt.Addr,
			"namespace", opts.Namespace != "", "customCA", opts.CAFile != "", "wrappedSecretID", opts.SecretIDWrapped)
	}
	return vc
}
