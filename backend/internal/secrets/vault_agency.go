package secrets

import "github.com/ResetSmith/cronomicon/internal/agencyid"

// A Vault per agency (2.5.1 SG3, SG-20, SG-21). An agency may connect a Vault
// of its own; one that does not uses the installation's, which is also
// Global's. A Vault-sourced secret or SSH key resolves in its OWNER agency's
// Vault: since 2.4.0 a row is used by its owner's runs only, so owner and run
// agree. The VaultClient interface stays as it was; the client settings wires
// in also answers ForAgency, and VaultForOwner is the one place a caller asks
// for the owner's client.

// AgencyVault is a VaultClient that can answer for one agency's rows.
type AgencyVault interface {
	ForAgency(owner string) VaultClient
}

// VaultLogin is a VaultClient that can authenticate without reading anything
// (the connection test).
type VaultLogin interface {
	Login() error
}

// VaultForOwner is the client for a row owned by owner (an agency id): the
// agency's own Vault when it has connected one, else the installation's. An
// agency whose own connection cannot be read or built gets a client that
// fails; it is never given the installation's instead, where its paths could
// name another agency's secrets (SG-21). Global, and an owner that names no
// agency, are the installation's.
func VaultForOwner(vc VaultClient, owner string) VaultClient {
	if vc == nil || owner == "" || owner == agencyid.Global {
		return vc
	}
	if av, ok := vc.(AgencyVault); ok {
		return av.ForAgency(owner)
	}
	return vc
}
