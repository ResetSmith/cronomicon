package secretstore_test

import (
	"errors"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/secretstore"
	_ "github.com/ResetSmith/cronomicon/internal/secretstore/all"
)

// A case every provider must answer the same way (2.5.5 SM1): its own
// spelling of a reference inside a prefix, one that shares the prefix's
// characters but not its segments, and malformed ones.
type providerCases struct {
	prefix    string
	inside    []string // well formed, inside prefix
	sibling   []string // well formed, outside prefix though sharing its characters
	malformed []string
}

// cases names every registered provider's examples. A provider registered
// without an entry here fails TestEveryProviderIsTabled.
var cases = map[string]providerCases{
	"hashicorp": {
		prefix:    "secret/data/tax",
		inside:    []string{"secret/data/tax/db#password", "secret/data/tax#k", "secret/data/tax/a/b"},
		sibling:   []string{"secret/data/tax-audit/db#x", "secret/data/ta#x", "secret/data/fin/db#x"},
		malformed: []string{"secret/data/tax/../fin/db#x", "/secret/data/tax/db#x", "secret/data/tax/db/#x", "secret//data#x", "secret/data/tax/%2e%2e/fin#x", "secret/data/tax/./db#x", ""},
	},
}

func TestEveryProviderIsTabled(t *testing.T) {
	for _, p := range secretstore.All() {
		if _, ok := cases[p.ID()]; !ok {
			t.Errorf("provider %q has no cases in provider_table_test.go", p.ID())
		}
	}
	if _, err := secretstore.Lookup(secretstore.Default); err != nil {
		t.Fatalf("the default provider is not registered: %v", err)
	}
}

func TestProviderTable(t *testing.T) {
	for id, c := range cases {
		p, err := secretstore.Lookup(id)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		prefix, err := p.NormalizePrefix(c.prefix)
		if err != nil {
			t.Fatalf("%s: prefix %q refused: %v", id, c.prefix, err)
		}
		for _, ref := range c.inside {
			if err := p.ValidateRef(ref); err != nil {
				t.Errorf("%s: %q refused: %v", id, ref, err)
			}
			if ok, err := secretstore.Allowed(p, ref, []string{prefix}); err != nil || !ok {
				t.Errorf("%s: %q inside %q = %v, %v; want true", id, ref, prefix, ok, err)
			}
		}
		for _, ref := range c.sibling {
			if ok, err := secretstore.Allowed(p, ref, []string{prefix}); err != nil || ok {
				t.Errorf("%s: %q inside %q = %v, %v; want false, nil", id, ref, prefix, ok, err)
			}
		}
		for _, ref := range c.malformed {
			err := p.ValidateRef(ref)
			if err == nil {
				t.Errorf("%s: malformed %q accepted", id, ref)
				continue
			}
			if !errors.Is(err, secretstore.ErrInvalid) {
				t.Errorf("%s: %q refused with %v, which is not secretstore.ErrInvalid", id, ref, err)
			}
			if ok, aerr := secretstore.Allowed(p, ref, []string{prefix}); aerr == nil || ok {
				t.Errorf("%s: Allowed(%q) = %v, %v; want an error", id, ref, ok, aerr)
			}
		}
		// A malformed stored prefix matches nothing and stops nothing else.
		if ok, err := secretstore.Allowed(p, c.inside[0], []string{"../bad", prefix}); err != nil || !ok {
			t.Errorf("%s: a malformed prefix beside a good one = %v, %v; want true", id, ok, err)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, err := secretstore.Lookup("nope"); !errors.Is(err, secretstore.ErrUnknownProvider) {
		t.Fatalf("Lookup(nope) = %v, want ErrUnknownProvider", err)
	}
}
