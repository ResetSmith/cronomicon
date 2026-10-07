// Package vaultpath is the one rule for whether a Vault path lies inside a
// prefix (LR-80).
//
// The installation has ONE Vault connection, with one credential, and a path on
// it is not divided by agency: whatever that credential can read, any secret or
// SSH key that names the path can have delivered to a run. So an agency may
// name Vault paths only inside prefixes a global administrator has assigned to
// it, and "inside" has to mean what Vault will make of the path, not what a
// string comparison makes of it:
//
//   - whole SEGMENTS. "secret/data/tax" does not contain
//     "secret/data/tax-audit/db", which shares its characters and not its
//     position in the tree.
//   - no "..", no ".", no empty segment. "secret/data/tax/../fin/db" starts with
//     the prefix as a string and names another agency's secret.
//   - nothing that a URL would decode into one of those. The path is sent to
//     Vault inside a URL, so '%' is refused outright rather than decoded and
//     second-guessed, with '\', '?', '#' and control characters.
//
// A reference may carry a "#field" suffix (the key inside the secret); it is
// not part of the path.
package vaultpath

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalid is wrapped by every refusal of a malformed path or prefix.
var ErrInvalid = errors.New("invalid Vault path")

// Split separates a reference into its path and its "#field" suffix, as the
// Vault client does: at the LAST '#'.
func Split(ref string) (path, field string) {
	if i := strings.LastIndex(ref, "#"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, ""
}

// Segments normalises a path — the part of a reference before "#field", or a
// prefix — into its segments. Leading and trailing slashes are ignored; any
// other irregularity is refused, never repaired: a path that needs repairing is
// one whose meaning to Vault is in doubt. That includes surrounding white
// space: what is judged here must be, byte for byte, what is later sent, so
// nothing is trimmed on the caller's behalf.
func Segments(path string) ([]string, error) {
	p := strings.Trim(path, "/")
	if p == "" {
		return nil, fmt.Errorf("%w: it is empty", ErrInvalid)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		switch {
		case s == "":
			return nil, fmt.Errorf("%w: %q has an empty segment (a doubled slash)", ErrInvalid, path)
		case s == "." || s == "..":
			return nil, fmt.Errorf("%w: %q has a %q segment", ErrInvalid, path, s)
		}
		for _, r := range s {
			if r == '%' || r == '\\' || r == '?' || r == '#' || r < 0x20 || r == 0x7f {
				return nil, fmt.Errorf("%w: %q contains %q, which is not allowed in a path", ErrInvalid, path, r)
			}
		}
		if s != strings.TrimSpace(s) {
			return nil, fmt.Errorf("%w: %q has a segment with surrounding spaces", ErrInvalid, path)
		}
	}
	return segs, nil
}

// Normalize returns a path or prefix in its one canonical spelling: its
// segments joined by single slashes, with none at either end.
func Normalize(path string) (string, error) {
	segs, err := Segments(path)
	if err != nil {
		return "", err
	}
	return strings.Join(segs, "/"), nil
}

// Under reports whether the path of a reference lies inside a prefix: the
// prefix's segments are the path's first segments. A path equal to the prefix
// is inside it. Either side being malformed is an error, not "no".
//
// The reference's path must be in its canonical spelling already — no slash at
// either end. A prefix is normalised when it is stored; a reference is stored
// and sent to Vault exactly as it was written, so one that only becomes
// acceptable after trimming is one whose stored form was never judged
// ("/secret/data/tax/db" is sent as ".../v1//secret/data/tax/db").
func Under(ref, prefix string) (bool, error) {
	path, _ := Split(ref)
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false, fmt.Errorf("%w: %q must not begin or end with a slash", ErrInvalid, path)
	}
	ps, err := Segments(path)
	if err != nil {
		return false, err
	}
	xs, err := Segments(prefix)
	if err != nil {
		return false, fmt.Errorf("prefix: %w", err)
	}
	if len(xs) > len(ps) {
		return false, nil
	}
	for i := range xs {
		if ps[i] != xs[i] { // Vault paths are case-sensitive
			return false, nil
		}
	}
	return true, nil
}

// Allowed reports whether a reference lies inside ANY of the prefixes. A
// malformed reference is an error. A malformed stored prefix is skipped: it
// can match nothing, and must not make every path of its agency unusable.
func Allowed(ref string, prefixes []string) (bool, error) {
	path, _ := Split(ref)
	if strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false, fmt.Errorf("%w: %q must not begin or end with a slash", ErrInvalid, path)
	}
	if _, err := Segments(path); err != nil {
		return false, err
	}
	for _, p := range prefixes {
		if ok, err := Under(ref, p); err == nil && ok {
			return true, nil
		}
	}
	return false, nil
}
