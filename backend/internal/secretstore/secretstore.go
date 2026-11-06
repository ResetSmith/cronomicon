// Package secretstore is the one place a kind of secret store is described
// (2.5.5 SM-1). A store is a secret manager Cronomicon reads values from at
// dispatch; a provider is one kind of store (HashiCorp Vault and OpenBao, AWS
// Secrets Manager, …); a connection is one agency's row naming its provider.
//
// A provider says what a reference to one of its secrets looks like and what
// "inside a prefix" means for it (LR-80, per provider since SM-3 of the
// owner's decisions). Callers that judge a reference, a prefix or a move ask
// the provider of the row's owner; they never parse a reference themselves.
//
// The client a provider builds is secrets.VaultClient: one interface for
// every provider, read-only since SM-8.
package secretstore

import (
	"errors"
	"fmt"
	"slices"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// ErrInvalid is matched (errors.Is) by every refusal of a malformed reference
// or prefix, whichever provider made it.
var ErrInvalid = errors.New("invalid secret store reference")

// ErrUnknownProvider is returned by Lookup for an id no provider registered.
var ErrUnknownProvider = errors.New("unknown secret store provider")

// Default is the provider of the installation's store and of every
// connection made before 2.5.5.
const Default = "hashicorp"

// Provider describes one kind of store.
type Provider interface {
	// ID is the stored value ("hashicorp", "aws").
	ID() string
	// Label is what the screens call it.
	Label() string
	// ValidateRef refuses a malformed reference, as written: a reference is
	// stored and sent exactly as written, so nothing is repaired here.
	ValidateRef(ref string) error
	// NormalizePrefix returns a prefix in its one stored spelling, or refuses it.
	NormalizePrefix(prefix string) (string, error)
	// UnderPrefix reports whether ref lies inside prefix. Either side being
	// malformed is an error, not "no".
	UnderPrefix(ref, prefix string) (bool, error)
	// Location is the part of a reference that names where the secret is,
	// without any key inside it: what a sentence quotes.
	Location(ref string) string
	// Address is the URL a connection makes the server dial, which the
	// installation's allowed hosts must list (vaultAllowedHosts, SG-22), or ""
	// when it reaches only a fixed public endpoint no list has to name (SM-7).
	Address(c Connection) string
	// Validate refuses a connection missing what its provider needs. It
	// never dials.
	Validate(c Connection) error
	// NewClient builds the client for a connection. It reads nothing but c:
	// no environment, no instance role, no metadata service (SM-4). The
	// caller has already checked Address against the allowed hosts.
	NewClient(c Connection, egress httpx.EgressPolicy) (secrets.VaultClient, error)
}

// Connection is one agency's connection as a provider reads it: plain
// settings and the opened credential, both string maps (their JSON is what
// agency_vaults stores, migration 1720).
type Connection struct {
	Agency     string
	Provider   string
	Settings   map[string]string
	Credential map[string]string
}

// Allowed reports whether ref lies inside any of prefixes under p's rule. A
// malformed reference is an error; a malformed stored prefix is skipped (it
// can match nothing, and must not make every reference of its agency
// unusable).
func Allowed(p Provider, ref string, prefixes []string) (bool, error) {
	if err := p.ValidateRef(ref); err != nil {
		return false, err
	}
	for _, x := range prefixes {
		if ok, err := p.UnderPrefix(ref, x); err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}

var registry = map[string]Provider{}

// Register adds a provider. It panics on a duplicate id: providers register
// from init, and two with one id is a build mistake.
func Register(p Provider) {
	if _, dup := registry[p.ID()]; dup {
		panic(fmt.Sprintf("secretstore: provider %q registered twice", p.ID()))
	}
	registry[p.ID()] = p
}

// Lookup returns the provider with id.
func Lookup(id string) (Provider, error) {
	if p, ok := registry[id]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, id)
}

// MustLookup is Lookup for an id the code itself names (Default).
func MustLookup(id string) Provider {
	p, err := Lookup(id)
	if err != nil {
		panic(err)
	}
	return p
}

// All returns every registered provider, ordered by id.
func All() []Provider {
	out := make([]Provider, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Provider) int {
		switch {
		case a.ID() < b.ID():
			return -1
		case a.ID() > b.ID():
			return 1
		}
		return 0
	})
	return out
}
