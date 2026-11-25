package settings

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
	"github.com/ResetSmith/cronomicon/internal/secretstore/hashicorp"
)

// A Vault per agency (2.5.1 SG3, SG-20 to SG-27; migration 1630).
//
// An agency may connect a Vault of its own, by AppRole (question 129). One
// that does not uses the installation's (vault_config), which is also
// Global's. Its rows (Vault-sourced secrets and SSH keys) resolve in its own
// Vault from then on, and only there (secrets.VaultForOwner): Vault, not only
// Cronomicon, then keeps the agencies' secrets apart, the agency can revoke
// Cronomicon's access, and every read is in its own Vault's audit log.
//
// The address must be on the installation's allowed Vault hosts (SG-22): an
// agency's administrator would otherwise point the server at any address.
// The check is made on write, on test and every time a client is built.

const vaultAllowedHostsKey = "vaultAllowedHosts"

var (
	// ErrVaultURLNotAllowed refuses an agency Vault address (422).
	ErrVaultURLNotAllowed = fmt.Errorf("%w: Vault address not allowed", ErrValidation)
	// ErrAgencyVaultInUse refuses disconnecting an agency's Vault while it
	// owns Vault-sourced secrets or keys (409 vault_in_use, SG-27): they would
	// resolve in the installation's Vault, where their paths could name
	// another agency's secrets.
	ErrAgencyVaultInUse = errors.New("the agency's Vault-sourced secrets and SSH keys still name paths in its Vault")
	// ErrGlobalUsesInstallationVault refuses an own Vault for Global (422).
	ErrGlobalUsesInstallationVault = fmt.Errorf("%w: the built-in administration agency's Vault is the installation's (Settings → Vault)", ErrValidation)
)

const maxCAPEMBytes = 64 << 10

// VaultAllowedHosts reads the list, normalized. A stored value that cannot be
// read is an empty list: nothing is allowed that the list does not say.
func VaultAllowedHosts(ctx context.Context, database *sql.DB) ([]string, error) {
	var raw sql.NullString
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, vaultAllowedHostsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", vaultAllowedHostsKey, err)
	}
	return parseGitAllowedHosts(raw.String), nil // the same entry rules
}

func writeVaultAllowedHosts(ctx context.Context, database *sql.DB, hosts []string, actor, now string) error {
	b, err := json.Marshal(hosts)
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx,
		`INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value,
		  last_modified_by=excluded.last_modified_by, last_modified_at=excluded.last_modified_at`,
		vaultAllowedHostsKey, string(b), actor, now)
	if err != nil {
		return fmt.Errorf("upsert setting %q: %w", vaultAllowedHostsKey, err)
	}
	return nil
}

// CheckVaultURL holds an agency's Vault address to https, no user, and a host
// on the allowed list (an empty list allows none).
func CheckVaultURL(ctx context.Context, database *sql.DB, rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: an agency's Vault is named by an https:// URL", ErrVaultURLNotAllowed)
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: an agency's Vault is reached over https; %s:// is not accepted", ErrVaultURLNotAllowed, u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("%w: the URL must not carry a user or a password", ErrVaultURLNotAllowed)
	}
	allowed, err := VaultAllowedHosts(ctx, database)
	if err != nil {
		return err
	}
	host, port := strings.ToLower(u.Hostname()), u.Port()
	for _, e := range allowed {
		eh, ep, serr := splitHostEntry(e)
		if serr != nil || eh != host {
			continue
		}
		if ep == "" || ep == port || (port == "" && ep == "443") {
			return nil
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: no host is allowed for an agency's Vault yet. A global administrator lists the allowed "+
			"hosts in Settings → Vault", ErrVaultURLNotAllowed)
	}
	return fmt.Errorf("%w: the host %s is not on the list of hosts an agency's Vault may be on. A global administrator "+
		"maintains it in Settings → Vault", ErrVaultURLNotAllowed, u.Host)
}

// AgencyVault is an agency's own Vault connection as its card shows it. The
// credentials are never returned.
type AgencyVault struct {
	AgencyID string `json:"agencyId"`
	// Provider is the kind of store (2.5.5 SM-3); Settings its plain settings
	// (a CA bundle is reported by CAPresent, not returned); CredentialSet
	// which of its credential's fields are stored. The HashiCorp fields below
	// stay for the screens that read them.
	Provider       string            `json:"provider"`
	ProviderLabel  string            `json:"providerLabel"`
	Settings       map[string]string `json:"settings"`
	CredentialSet  map[string]bool   `json:"credentialSet"`
	Addr           string            `json:"addr"`
	Namespace      string            `json:"namespace"`
	CAPresent      bool              `json:"caPresent"`
	RoleIDSet      bool              `json:"roleIdSet"`
	SecretIDSet    bool              `json:"secretIdSet"`
	LastModifiedBy string            `json:"lastModifiedBy"`
	LastModifiedAt string            `json:"lastModifiedAt"`
}

// AgencyVaultInput is a write. A blank RoleID or SecretID keeps the stored one
// (a new connection needs both); CAPEM nil keeps the stored bundle, "" clears it.
//
// 2.5.5 SM-4: Provider ("" is the connection's own, else hashicorp),
// Settings and Credential carry any provider's fields; for HashiCorp the flat
// fields below still work and the maps win where both are given. A blank
// credential field keeps the stored one when the provider is unchanged. A
// settings key absent keeps the stored value; "" clears it.
type AgencyVaultInput struct {
	Provider   string            `json:"provider"`
	Settings   map[string]string `json:"settings"`
	Credential map[string]string `json:"credential"`
	Addr       string            `json:"addr"`
	Namespace  string            `json:"namespace"`
	RoleID     string            `json:"roleId"`
	SecretID   string            `json:"secretId"`
	CAPEM      *string           `json:"caPem"`
}

// AgencyVaultRuntime is what a client is built from.
//
// 2.5.5 SM-3: a connection names its provider. Until the API carries a
// provider's own fields (SM4) the runtime keeps HashiCorp's as fields;
// Connection turns it into what a provider reads.
type AgencyVaultRuntime struct {
	// Agency is whose connection this is: the cache keeps one client per agency.
	Agency                            string
	Provider                          string
	Addr, Namespace, RoleID, SecretID string
	CAPEM                             []byte
	// Settings and Credential are another provider's (not HashiCorp's) as
	// stored; HashiCorp's are the fields above.
	Settings, Credential map[string]string
}

// Connection is the runtime as its provider reads it.
func (rt AgencyVaultRuntime) Connection() secretstore.Connection {
	if rt.provider() != hashicorp.ID {
		return secretstore.Connection{Agency: rt.Agency, Provider: rt.provider(), Settings: rt.Settings, Credential: rt.Credential}
	}
	return secretstore.Connection{
		Agency:   rt.Agency,
		Provider: rt.provider(),
		Settings: map[string]string{
			hashicorp.SettingAddr: rt.Addr, hashicorp.SettingNamespace: rt.Namespace, hashicorp.SettingCAPEM: string(rt.CAPEM),
		},
		Credential: map[string]string{hashicorp.CredRoleID: rt.RoleID, hashicorp.CredSecretID: rt.SecretID},
	}
}

func (rt AgencyVaultRuntime) provider() string {
	if rt.Provider == "" {
		return secretstore.Default
	}
	return rt.Provider
}

// storedConnection is a connection's sealed credential and plain settings as
// agency_vaults holds them (migration 1720).
func sealStoreCredential(cfg *config.Config, agency string, cred map[string]string) (string, error) {
	b, err := json.Marshal(cred)
	if err != nil {
		return "", err
	}
	return secrets.EncryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyStoreCredential, agency), string(b))
}

func openStoreCredential(cfg *config.Config, agency, tok string) (map[string]string, error) {
	plain, err := secrets.DecryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyStoreCredential, agency), tok)
	if err != nil {
		return nil, err
	}
	var cred map[string]string
	if err := json.Unmarshal([]byte(plain), &cred); err != nil {
		return nil, fmt.Errorf("the stored credential is not a JSON object: %w", err)
	}
	return cred, nil
}

// GetAgencyVault reads an agency's connection; nil when it has none.
func GetAgencyVault(ctx context.Context, database *sql.DB, agency string) (*AgencyVault, error) {
	var v AgencyVault
	var roleEnc, secretEnc, ca, credEnc, settingsJSON string
	err := database.QueryRowContext(ctx, `
		SELECT agency_id, provider, addr, namespace, role_id_enc, secret_id_enc, ca_pem, COALESCE(credential_enc, ''), settings_json,
		       last_modified_by, last_modified_at
		  FROM agency_vaults WHERE agency_id = ?`, agency).
		Scan(&v.AgencyID, &v.Provider, &v.Addr, &v.Namespace, &roleEnc, &secretEnc, &ca, &credEnc, &settingsJSON, &v.LastModifiedBy, &v.LastModifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agency vault: %w", err)
	}
	v.CAPresent, v.RoleIDSet, v.SecretIDSet = ca != "", roleEnc != "" || credEnc != "", secretEnc != "" || credEnc != ""
	var set map[string]string
	_ = json.Unmarshal([]byte(settingsJSON), &set)
	v.Settings = map[string]string{}
	for k, val := range set {
		if k == "caPem" {
			v.CAPresent = v.CAPresent || val != ""
			continue // public, but large: its presence is what the card shows
		}
		v.Settings[k] = val
	}
	if p, err := secretstore.Lookup(v.Provider); err == nil {
		v.ProviderLabel = p.Label()
	}
	// Which credential fields are stored is known without opening it: a
	// connection is saved only when its provider's Validate passed, so every
	// field the provider requires is set. The optional ones are not reported.
	v.CredentialSet = map[string]bool{}
	if credEnc != "" || roleEnc != "" {
		if p, err := secretstore.Lookup(v.Provider); err == nil {
			for _, k := range p.RequiredCredential() {
				v.CredentialSet[k] = true
			}
		}
	}
	return &v, nil
}

// ReadAgencyVaultRuntime opens an agency's connection: found false when it
// has none (it uses the installation's); an error when it has one that cannot
// be read, which a caller must not answer with the installation's Vault.
func ReadAgencyVaultRuntime(ctx context.Context, database *sql.DB, cfg *config.Config, agency string) (AgencyVaultRuntime, bool, error) {
	rt := AgencyVaultRuntime{Agency: agency}
	var roleEnc, secretEnc, ca, credEnc, settingsJSON string
	err := database.QueryRowContext(ctx, `
		SELECT provider, addr, namespace, role_id_enc, secret_id_enc, ca_pem, COALESCE(credential_enc, ''), settings_json
		  FROM agency_vaults WHERE agency_id = ?`, agency).
		Scan(&rt.Provider, &rt.Addr, &rt.Namespace, &roleEnc, &secretEnc, &ca, &credEnc, &settingsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return rt, false, nil
	}
	if err != nil {
		return rt, true, fmt.Errorf("read agency vault: %w", err)
	}
	if credEnc != "" {
		// 2.5.5 SM-3: the provider's credential and settings.
		cred, err := openStoreCredential(cfg, agency, credEnc)
		if err != nil {
			return rt, true, fmt.Errorf("open the agency's secret store credential: %w", err)
		}
		var set map[string]string
		if err := json.Unmarshal([]byte(settingsJSON), &set); err != nil {
			return rt, true, fmt.Errorf("read the agency's secret store settings: %w", err)
		}
		rt.Settings, rt.Credential = set, cred
		if rt.provider() == hashicorp.ID {
			rt.Addr, rt.Namespace, rt.CAPEM = set[hashicorp.SettingAddr], set[hashicorp.SettingNamespace], []byte(set[hashicorp.SettingCAPEM])
			rt.RoleID, rt.SecretID = cred[hashicorp.CredRoleID], cred[hashicorp.CredSecretID]
		}
		return rt, true, nil
	}
	// A row the boot pass has not converted yet (it runs before any request;
	// this is for the pass itself and a pass that could not finish).
	if rt.RoleID, err = secrets.DecryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultRoleID, agency), roleEnc); err != nil {
		return rt, true, fmt.Errorf("open the agency's Vault role id: %w", err)
	}
	if rt.SecretID, err = secrets.DecryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultSecretID, agency), secretEnc); err != nil {
		return rt, true, fmt.Errorf("open the agency's Vault secret id: %w", err)
	}
	rt.CAPEM = []byte(ca)
	return rt, true, nil
}

// PrepareAgencyVault validates a write against what is stored and returns the
// runtime it would give (for the preview and the test, before anything is
// written).
func PrepareAgencyVault(ctx context.Context, database *sql.DB, cfg *config.Config, agency string, in AgencyVaultInput) (AgencyVaultRuntime, error) {
	if agency == agencyid.Global || agency == "" {
		return AgencyVaultRuntime{}, ErrGlobalUsesInstallationVault
	}
	cur, found, curErr := ReadAgencyVaultRuntime(ctx, database, cfg, agency)
	if found && curErr != nil {
		// Unreadable: nothing of it is kept, so the input must be whole.
		found = false
	} else if curErr != nil {
		return AgencyVaultRuntime{}, curErr
	}
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = secretstore.Default
		if found {
			provider = cur.provider()
		}
	}
	p, err := secretstore.Lookup(provider)
	if err != nil {
		return AgencyVaultRuntime{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	// SM-9: another kind of store while rows name this one is refused; their
	// references are written in this provider's grammar.
	prev, exists, err := agencyVaultProvider(ctx, database, agency)
	if err != nil {
		return AgencyVaultRuntime{}, err
	}
	if exists && prev != provider {
		n, err := countExternalRows(ctx, database, agency)
		if err != nil {
			return AgencyVaultRuntime{}, err
		}
		if n > 0 {
			return AgencyVaultRuntime{}, fmt.Errorf("%w (%d)", ErrStoreProviderInUse, n)
		}
	}
	same := found && cur.provider() == provider
	conn := secretstore.Connection{Agency: agency, Provider: provider, Settings: map[string]string{}, Credential: map[string]string{}}
	if same {
		c := cur.Connection()
		maps.Copy(conn.Settings, c.Settings)
		maps.Copy(conn.Credential, c.Credential)
	}
	if provider == hashicorp.ID {
		// The flat fields (the 2.5.1 body): the address and namespace are
		// always the body's; a blank credential keeps the stored one.
		conn.Settings[hashicorp.SettingAddr] = strings.TrimRight(strings.TrimSpace(in.Addr), "/")
		conn.Settings[hashicorp.SettingNamespace] = strings.TrimSpace(in.Namespace)
		if v := strings.TrimSpace(in.RoleID); v != "" {
			conn.Credential[hashicorp.CredRoleID] = v
		}
		if v := strings.TrimSpace(in.SecretID); v != "" {
			conn.Credential[hashicorp.CredSecretID] = v
		}
		if in.CAPEM != nil {
			conn.Settings[hashicorp.SettingCAPEM] = *in.CAPEM
		}
	}
	for k, v := range in.Settings {
		v = strings.TrimSpace(v)
		if k == hashicorp.SettingAddr || k == "endpoint" {
			v = strings.TrimRight(v, "/")
		}
		conn.Settings[k] = v
	}
	for k, v := range in.Credential {
		if v = strings.TrimSpace(v); v != "" {
			conn.Credential[k] = v
		}
	}
	if pem := strings.TrimSpace(conn.Settings["caPem"]); pem != "" {
		if len(pem) > maxCAPEMBytes {
			return AgencyVaultRuntime{}, fmt.Errorf("%w: the CA bundle is larger than 64 KiB", ErrValidation)
		}
		if !x509.NewCertPool().AppendCertsFromPEM([]byte(pem)) {
			return AgencyVaultRuntime{}, fmt.Errorf("%w: the CA bundle holds no PEM certificate", ErrValidation)
		}
		conn.Settings["caPem"] = pem
	} else {
		delete(conn.Settings, "caPem")
	}
	for k, v := range conn.Settings {
		if v == "" {
			delete(conn.Settings, k)
		}
	}
	if err := p.Validate(conn); err != nil {
		return AgencyVaultRuntime{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	rt := runtimeOf(conn)
	if addr := p.Address(conn); addr != "" {
		if err := CheckVaultURL(ctx, database, addr); err != nil {
			return rt, err
		}
	}
	return rt, nil
}

// runtimeOf is the runtime for a connection.
func runtimeOf(c secretstore.Connection) AgencyVaultRuntime {
	rt := AgencyVaultRuntime{Agency: c.Agency, Provider: c.Provider, Settings: c.Settings, Credential: c.Credential}
	if c.Provider == hashicorp.ID {
		rt.Addr, rt.Namespace, rt.CAPEM = c.Settings[hashicorp.SettingAddr], c.Settings[hashicorp.SettingNamespace], []byte(c.Settings[hashicorp.SettingCAPEM])
		rt.RoleID, rt.SecretID = c.Credential[hashicorp.CredRoleID], c.Credential[hashicorp.CredSecretID]
	}
	return rt
}

// ErrStoreProviderInUse refuses changing an agency's provider while it owns
// external secrets or keys (2.5.5 SM-9, decision 5): 409 vault_in_use.
var ErrStoreProviderInUse = errors.New("the agency's external secrets and SSH keys name references in its current kind of store")

func agencyVaultProvider(ctx context.Context, database *sql.DB, agency string) (string, bool, error) {
	var p string
	err := database.QueryRowContext(ctx, `SELECT provider FROM agency_vaults WHERE agency_id = ?`, agency).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return p, err == nil, err
}

// CountExternalRows is countExternalRows for the API's answer (SM5: the
// screen locks the provider picker and says why).
func CountExternalRows(ctx context.Context, database *sql.DB, agency string) (int, error) {
	return countExternalRows(ctx, database, agency)
}

// countExternalRows counts an agency's external secrets and SSH keys, by each
// store's own question (a secret: a source that is not exactly "stored"; a
// key: exactly "vault"; either: a ref), as the disconnect does.
func countExternalRows(ctx context.Context, q rowQuerier, agency string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM secrets WHERE owner_agency = ?1 AND (source <> 'stored' OR COALESCE(vault_ref, '') <> ''))
		     + (SELECT COUNT(*) FROM ssh_credentials WHERE owner_agency = ?1 AND (source = 'vault' OR COALESCE(vault_ref, '') <> ''))`, agency).Scan(&n)
	return n, err
}

// PutAgencyVault writes an agency's connection (prepared by PrepareAgencyVault).
func PutAgencyVault(ctx context.Context, database *sql.DB, cfg *config.Config, agency string, rt AgencyVaultRuntime, actor string) (*AgencyVault, error) {
	conn := rt.Connection()
	credTok, err := sealStoreCredential(cfg, agency, conn.Credential)
	if err != nil {
		return nil, fmt.Errorf("seal the credential (is a KEK configured?): %w", err)
	}
	settingsJSON, err := json.Marshal(conn.Settings)
	if err != nil {
		return nil, err
	}
	prev, err := GetAgencyVault(ctx, database, agency)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO agency_vaults (agency_id, provider, credential_enc, settings_json, addr, namespace, role_id_enc, secret_id_enc, ca_pem,
		                           created_by, created_at, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, ?)
		ON CONFLICT(agency_id) DO UPDATE SET provider = excluded.provider, credential_enc = excluded.credential_enc,
		  settings_json = excluded.settings_json, addr = excluded.addr, namespace = excluded.namespace,
		  role_id_enc = '', secret_id_enc = '', ca_pem = excluded.ca_pem,
		  last_modified_by = excluded.last_modified_by, last_modified_at = excluded.last_modified_at`,
		agency, rt.provider(), credTok, string(settingsJSON), rt.Addr, rt.Namespace, string(rt.CAPEM), actor, now, actor, now); err != nil {
		return nil, fmt.Errorf("write agency vault: %w", err)
	}
	secrets.RedactionSourceChanged()
	forgetStoreReadFailure(agency) // a new credential is judged afresh (SM-6)
	action := "vault-connected"
	if prev != nil {
		action = "vault-updated"
	}
	audit(ctx, database, agency, actor, "Settings", action, "Vault", "addr="+rt.Addr)
	return GetAgencyVault(ctx, database, agency)
}

// DeleteAgencyVault disconnects an agency's Vault, refused while it owns
// Vault-sourced rows (SG-27). Reports whether there was one.
//
// The agency's Vault path prefixes go with it (2.5.1 SG6.2, the review's A4).
// In its own Vault they were a limit it set itself (SG-26); left standing,
// they would become its assignment in the installation's Vault, where they
// are a global administrator's to give (LR-80). With none it names no path
// there until one does. The count, the connection and the prefixes are one
// transaction; an agency with no connection keeps the prefixes it was given.
func DeleteAgencyVault(ctx context.Context, database *sql.DB, agency, actor string) (bool, error) {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM secrets WHERE owner_agency = ?1 AND source = 'vault')
		     + (SELECT COUNT(*) FROM ssh_credentials WHERE owner_agency = ?1 AND source = 'vault')`, agency).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, fmt.Errorf("%w (%d)", ErrAgencyVaultInUse, n)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM agency_vaults WHERE agency_id = ?`, agency)
	if err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	if k, _ := res.RowsAffected(); k == 0 {
		return false, nil
	}
	var cleared []string
	rows, err := tx.QueryContext(ctx, `SELECT prefix FROM agency_vault_prefixes WHERE agency_id = ? ORDER BY prefix`, agency)
	if err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return false, fmt.Errorf("delete agency vault: %w", err)
		}
		cleared = append(cleared, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM agency_vault_prefixes WHERE agency_id = ?`, agency); err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("delete agency vault: %w", err)
	}
	secrets.RedactionSourceChanged()
	detail := ""
	if len(cleared) > 0 {
		detail = "prefixes cleared: " + strings.Join(cleared, ", ")
	}
	forgetStoreReadFailure(agency)
	audit(ctx, database, agency, actor, "Settings", "vault-disconnected", "Vault", detail)
	return true, nil
}

// AgencyHasOwnVault reports whether an agency has connected a Vault of its own.
func AgencyHasOwnVault(ctx context.Context, database *sql.DB, agency string) (bool, error) {
	var n int
	err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM agency_vaults WHERE agency_id = ?`, agency).Scan(&n)
	return n > 0, err
}

// AgencyVaultRefs lists an agency's Vault-sourced secrets and keys, for the
// preview: kind, id, name and the path each names.
type AgencyVaultRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// ListAgencyVaultRefs returns them.
func ListAgencyVaultRefs(ctx context.Context, database *sql.DB, agency string) ([]AgencyVaultRef, error) {
	rows, err := database.QueryContext(ctx, `
		SELECT 'secret', id, key, COALESCE(vault_ref, '') FROM secrets WHERE owner_agency = ?1 AND source = 'vault'
		UNION ALL
		SELECT 'key', id, label, COALESCE(vault_ref, '') FROM ssh_credentials WHERE owner_agency = ?1 AND source = 'vault'
		ORDER BY 1, 3`, agency)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AgencyVaultRef{}
	for rows.Next() {
		var r AgencyVaultRef
		if err := rows.Scan(&r.Kind, &r.ID, &r.Name, &r.Path); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
