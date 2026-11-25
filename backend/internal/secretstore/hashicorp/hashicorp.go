// Package hashicorp is the secret store provider for HashiCorp Vault and
// OpenBao, which speaks the same API (2.5.5 SM-2). A reference is a KV v2 path
// and a field, "secret/data/app#PASSWORD"; its prefix rule is internal/vaultpath
// (whole segments, nothing a URL could decode into "..").
//
// Importing the package registers the provider; internal/secretstore/all
// imports every provider so a caller needs one import.
package hashicorp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
	"github.com/ResetSmith/cronomicon/internal/vaultpath"
)

// ID is the provider's stored value.
const ID = "hashicorp"

func init() { secretstore.Register(Provider{}) }

// Provider is HashiCorp Vault / OpenBao.
type Provider struct{}

func (Provider) ID() string    { return ID }
func (Provider) Label() string { return "HashiCorp Vault / OpenBao" }

// ValidateRef refuses a reference whose path is malformed by vaultpath's
// rules, including a slash at either end (the path is sent as written). The
// "#field" is optional: without one the client reads the field "value".
func (Provider) ValidateRef(ref string) error {
	path, _ := vaultpath.Split(ref)
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return invalid(fmt.Errorf("%w: %q must not begin or end with a slash", vaultpath.ErrInvalid, path))
	}
	if _, err := vaultpath.Segments(path); err != nil {
		return invalid(err)
	}
	return nil
}

func (Provider) NormalizePrefix(prefix string) (string, error) {
	n, err := vaultpath.Normalize(prefix)
	if err != nil {
		return "", invalid(err)
	}
	return n, nil
}

func (Provider) UnderPrefix(ref, prefix string) (bool, error) {
	ok, err := vaultpath.Under(ref, prefix)
	if err != nil {
		return false, invalid(err)
	}
	return ok, nil
}

func (Provider) Location(ref string) string {
	path, _ := vaultpath.Split(ref)
	return path
}

// The connection's keys. Settings: addr, namespace, caPem. Credential: roleId,
// secretId (AppRole only, question 129).
const (
	SettingAddr      = "addr"
	SettingNamespace = "namespace"
	SettingCAPEM     = "caPem"
	CredRoleID       = "roleId"
	CredSecretID     = "secretId"
)

func (Provider) RequiredCredential() []string { return []string{CredRoleID, CredSecretID} }

func (Provider) RefHint() secretstore.RefHint {
	return secretstore.RefHint{Label: "Vault path", Placeholder: "secret/data/myapp#KEY_NAME",
		Format: "A KV v2 path and a field: engine/data/path#FIELD (without #FIELD the field \"value\" is read)."}
}

func (Provider) Address(c secretstore.Connection) string { return c.Settings[SettingAddr] }

func (Provider) Validate(c secretstore.Connection) error {
	if c.Settings[SettingAddr] == "" {
		return fmt.Errorf("a HashiCorp Vault / OpenBao connection needs its address")
	}
	if c.Credential[CredRoleID] == "" || c.Credential[CredSecretID] == "" {
		return fmt.Errorf("an agency's Vault is reached by AppRole: give its role id and secret id")
	}
	return nil
}

func (p Provider) NewClient(c secretstore.Connection, egress httpx.EgressPolicy) (secrets.VaultClient, error) {
	if err := p.Validate(c); err != nil {
		return nil, err
	}
	return secrets.NewVaultClientWithOptions(c.Settings[SettingAddr], c.Credential[CredRoleID], c.Credential[CredSecretID], secrets.VaultOptions{
		AuthMethod: "approle",
		Namespace:  c.Settings[SettingNamespace],
		CAPEM:      []byte(c.Settings[SettingCAPEM]),
		Egress:     egress,
	})
}

// invalidErr carries vaultpath's message and matches both vaultpath.ErrInvalid
// and secretstore.ErrInvalid.
type invalidErr struct{ err error }

func (e invalidErr) Error() string { return e.err.Error() }
func (e invalidErr) Unwrap() error { return e.err }
func (e invalidErr) Is(target error) bool {
	return target == secretstore.ErrInvalid
}

func invalid(err error) error {
	if err == nil || errors.Is(err, secretstore.ErrInvalid) {
		return err
	}
	return invalidErr{err}
}
