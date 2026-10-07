package vaultpath

import (
	"errors"
	"testing"
)

func TestUnderMatchesWholeSegments(t *testing.T) {
	for _, c := range []struct {
		ref, prefix string
		want        bool
	}{
		{"secret/data/tax/db#password", "secret/data/tax", true},
		{"secret/data/tax#password", "secret/data/tax", true}, // the prefix itself
		{"secret/data/tax/db", "/secret/data/tax/", true},     // a PREFIX may be written with slashes at its ends
		{"secret/data/tax/a/b/c#k", "secret/data/tax", true},
		// Shares the characters, not the place in the tree.
		{"secret/data/tax-audit/db#password", "secret/data/tax", false},
		{"secret/data/taxes/db", "secret/data/tax", false},
		{"secret/data/ta", "secret/data/tax", false},
		{"secret/data", "secret/data/tax", false}, // the parent is not inside the child
		{"secret/data/fin/db", "secret/data/tax", false},
		{"secret/data/Tax/db", "secret/data/tax", false}, // case-sensitive, as Vault is
		{"other/data/tax/db", "secret/data/tax", false},
		// The field is not the path: a '#' later in the reference ends it.
		{"secret/data/tax/db#secret/data/fin", "secret/data/tax", true},
	} {
		got, err := Under(c.ref, c.prefix)
		if err != nil {
			t.Errorf("Under(%q, %q): %v", c.ref, c.prefix, err)
			continue
		}
		if got != c.want {
			t.Errorf("Under(%q, %q) = %v, want %v", c.ref, c.prefix, got, c.want)
		}
	}
}

// A path whose meaning to Vault is in doubt is refused, not repaired: each of
// these starts with the prefix as a STRING and names something outside it, or
// could once a URL has been decoded.
func TestMalformedPathsAreRefusedNotRepaired(t *testing.T) {
	for _, ref := range []string{
		"secret/data/tax/../fin/db#password",
		"secret/data/tax/..",
		"secret/data/tax/./db",
		"secret/data/tax//db",
		"secret/data/tax/%2e%2e/fin/db",
		"secret/data/tax/..%2ffin/db",
		`secret/data/tax\..\fin\db`,
		"secret/data/tax/db?version=1",
		// A reference is stored and sent as written, so its ends are not trimmed
		// for it: this would be sent as ".../v1//secret/data/tax/db".
		"/secret/data/tax/db",
		"secret/data/tax/db/",
		"//secret/data/tax/db",
		"secret/data/tax/ db",
		"secret/data/tax/db\n",
		"secret/data/tax/\x00db",
		"",
		"/",
		"#password",
	} {
		if ok, err := Under(ref, "secret/data/tax"); err == nil || !errors.Is(err, ErrInvalid) || ok {
			t.Errorf("Under(%q) = %v, %v; want an ErrInvalid refusal", ref, ok, err)
		}
		if ok, err := Allowed(ref, []string{"secret/data/tax"}); err == nil || ok {
			t.Errorf("Allowed(%q) = %v, %v; want a refusal", ref, ok, err)
		}
	}
	// A malformed PREFIX is an error for Under, and matches nothing for Allowed
	// without spoiling the agency's other prefixes.
	if _, err := Under("secret/data/tax/db", "secret/data/../data/tax"); err == nil {
		t.Error("a prefix with .. was accepted")
	}
	ok, err := Allowed("secret/data/tax/db", []string{"secret//broken", "secret/data/tax"})
	if err != nil || !ok {
		t.Errorf("a malformed stored prefix spoiled a good one: %v, %v", ok, err)
	}
	if ok, err := Allowed("secret/data/tax/db", []string{"secret//broken"}); err != nil || ok {
		t.Errorf("a malformed stored prefix matched: %v, %v", ok, err)
	}
	if ok, _ := Allowed("secret/data/tax/db", nil); ok {
		t.Error("a path was allowed with no prefixes at all")
	}
}

func TestNormalizeHasOneSpelling(t *testing.T) {
	for in, want := range map[string]string{
		"secret/data/tax":      "secret/data/tax",
		"/secret/data/tax/":    "secret/data/tax",
		"///secret/data/tax//": "secret/data/tax",
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// White space is not trimmed: what is judged must be what is sent.
	for _, in := range []string{"  secret/data/tax  ", "secret/data/tax ", " secret/data/tax", "secret/data/tax\n"} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q, want a refusal", in, got)
		}
	}
	if p, f := Split("secret/data/tax/db#password"); p != "secret/data/tax/db" || f != "password" {
		t.Errorf("Split = %q, %q", p, f)
	}
	if p, f := Split("secret/data/tax/db"); p != "secret/data/tax/db" || f != "" {
		t.Errorf("Split without a field = %q, %q", p, f)
	}
}
