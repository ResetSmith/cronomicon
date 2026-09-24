package secrets

import "errors"

// VaultClient is the interface for fetching secrets from HashiCorp Vault. The
// concrete implementation is httpVaultClient (vault_http.go — AppRole + KV v2 over
// the Vault HTTP API, no SDK dep); settings.WireVaultClient attaches it to a
// Service when Vault is configured (env or DB). The stubVaultClient below is the
// unconfigured fallback — vault-source rows then return an "unavailable" error,
// so an operator who never configures Vault sees no behavior change.
//
// A vault_ref is KV v2 "secret/data/app#FIELD"; the field after '#' selects one
// key from the secret's data map (defaults to "value"). The same interface backs
// vault-source secrets (Service.Reveal) and vault-source SSH credentials
// (sshkeys.decryptCredential, P2.4), so both resolve through one configured client.
type VaultClient interface {
	// Fetch retrieves the plaintext secret value at vaultRef.
	// Returns ErrVaultUnavailable if Vault is not reachable.
	Fetch(vaultRef string) (string, error)
	// Write stores a plaintext value at the given path. Used by migrate-to-vault.
	Write(vaultPath, value string) error
}

// ErrVaultUnavailable is returned when Vault is not configured or unreachable.
// Exported so settings' dynamic client can fail an operation with the exact
// error the stub uses — callers (and the settings_mount error mapping) see one
// consistent "vault unavailable" failure regardless of which layer refused.
var ErrVaultUnavailable = errors.New("vault unavailable: no Vault client configured")

// stubVaultClient is the no-op implementation used when Vault is unconfigured.
type stubVaultClient struct{}

func (stubVaultClient) Fetch(_ string) (string, error) { return "", ErrVaultUnavailable }
func (stubVaultClient) Write(_, _ string) error        { return ErrVaultUnavailable }
