package settings

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
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
	ErrGlobalUsesInstallationVault = fmt.Errorf("%w: Global's Vault is the installation's (Settings → Vault)", ErrValidation)
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
	AgencyID       string `json:"agencyId"`
	Addr           string `json:"addr"`
	Namespace      string `json:"namespace"`
	CAPresent      bool   `json:"caPresent"`
	RoleIDSet      bool   `json:"roleIdSet"`
	SecretIDSet    bool   `json:"secretIdSet"`
	LastModifiedBy string `json:"lastModifiedBy"`
	LastModifiedAt string `json:"lastModifiedAt"`
}

// AgencyVaultInput is a write. A blank RoleID or SecretID keeps the stored one
// (a new connection needs both); CAPEM nil keeps the stored bundle, "" clears it.
type AgencyVaultInput struct {
	Addr      string  `json:"addr"`
	Namespace string  `json:"namespace"`
	RoleID    string  `json:"roleId"`
	SecretID  string  `json:"secretId"`
	CAPEM     *string `json:"caPem"`
}

// AgencyVaultRuntime is what a client is built from.
type AgencyVaultRuntime struct {
	// Agency is whose connection this is: the cache keeps one client per agency.
	Agency                            string
	Addr, Namespace, RoleID, SecretID string
	CAPEM                             []byte
}

// GetAgencyVault reads an agency's connection; nil when it has none.
func GetAgencyVault(ctx context.Context, database *sql.DB, agency string) (*AgencyVault, error) {
	var v AgencyVault
	var roleEnc, secretEnc, ca string
	err := database.QueryRowContext(ctx, `
		SELECT agency_id, addr, namespace, role_id_enc, secret_id_enc, ca_pem, last_modified_by, last_modified_at
		  FROM agency_vaults WHERE agency_id = ?`, agency).
		Scan(&v.AgencyID, &v.Addr, &v.Namespace, &roleEnc, &secretEnc, &ca, &v.LastModifiedBy, &v.LastModifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agency vault: %w", err)
	}
	v.CAPresent, v.RoleIDSet, v.SecretIDSet = ca != "", roleEnc != "", secretEnc != ""
	return &v, nil
}

// ReadAgencyVaultRuntime opens an agency's connection: found false when it
// has none (it uses the installation's); an error when it has one that cannot
// be read, which a caller must not answer with the installation's Vault.
func ReadAgencyVaultRuntime(ctx context.Context, database *sql.DB, cfg *config.Config, agency string) (AgencyVaultRuntime, bool, error) {
	rt := AgencyVaultRuntime{Agency: agency}
	var roleEnc, secretEnc, ca string
	err := database.QueryRowContext(ctx, `
		SELECT addr, namespace, role_id_enc, secret_id_enc, ca_pem FROM agency_vaults WHERE agency_id = ?`, agency).
		Scan(&rt.Addr, &rt.Namespace, &roleEnc, &secretEnc, &ca)
	if errors.Is(err, sql.ErrNoRows) {
		return rt, false, nil
	}
	if err != nil {
		return rt, true, fmt.Errorf("read agency vault: %w", err)
	}
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
	rt := AgencyVaultRuntime{Agency: agency, Addr: strings.TrimRight(strings.TrimSpace(in.Addr), "/"), Namespace: strings.TrimSpace(in.Namespace)}
	if err := CheckVaultURL(ctx, database, rt.Addr); err != nil {
		return rt, err
	}
	cur, found, err := ReadAgencyVaultRuntime(ctx, database, cfg, agency)
	if err != nil && (in.RoleID == "" || in.SecretID == "") {
		return rt, err
	}
	rt.RoleID, rt.SecretID, rt.CAPEM = strings.TrimSpace(in.RoleID), strings.TrimSpace(in.SecretID), cur.CAPEM
	if rt.RoleID == "" && found {
		rt.RoleID = cur.RoleID
	}
	if rt.SecretID == "" && found {
		rt.SecretID = cur.SecretID
	}
	if rt.RoleID == "" || rt.SecretID == "" {
		return rt, fmt.Errorf("%w: an agency's Vault is reached by AppRole: give its role id and secret id", ErrValidation)
	}
	if in.CAPEM != nil {
		pem := strings.TrimSpace(*in.CAPEM)
		if len(pem) > maxCAPEMBytes {
			return rt, fmt.Errorf("%w: the CA bundle is larger than 64 KiB", ErrValidation)
		}
		if pem != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(pem)) {
			return rt, fmt.Errorf("%w: the CA bundle holds no PEM certificate", ErrValidation)
		}
		rt.CAPEM = []byte(pem)
	}
	return rt, nil
}

// PutAgencyVault writes an agency's connection (prepared by PrepareAgencyVault).
func PutAgencyVault(ctx context.Context, database *sql.DB, cfg *config.Config, agency string, rt AgencyVaultRuntime, actor string) (*AgencyVault, error) {
	roleTok, err := secrets.EncryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultRoleID, agency), rt.RoleID)
	if err != nil {
		return nil, fmt.Errorf("seal the role id (is a KEK configured?): %w", err)
	}
	secretTok, err := secrets.EncryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultSecretID, agency), rt.SecretID)
	if err != nil {
		return nil, fmt.Errorf("seal the secret id (is a KEK configured?): %w", err)
	}
	prev, err := GetAgencyVault(ctx, database, agency)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := database.ExecContext(ctx, `
		INSERT INTO agency_vaults (agency_id, addr, namespace, role_id_enc, secret_id_enc, ca_pem, created_by, created_at, last_modified_by, last_modified_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agency_id) DO UPDATE SET addr = excluded.addr, namespace = excluded.namespace,
		  role_id_enc = excluded.role_id_enc, secret_id_enc = excluded.secret_id_enc, ca_pem = excluded.ca_pem,
		  last_modified_by = excluded.last_modified_by, last_modified_at = excluded.last_modified_at`,
		agency, rt.Addr, rt.Namespace, roleTok, secretTok, string(rt.CAPEM), actor, now, actor, now); err != nil {
		return nil, fmt.Errorf("write agency vault: %w", err)
	}
	secrets.RedactionSourceChanged()
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
