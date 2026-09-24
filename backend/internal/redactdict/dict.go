// Package redactdict is the process-wide redaction dictionary (AM-4a,
// the audit-redaction plan): every value the server can know is
// secret, regardless of scope, so that a masker with no per-run context — the
// audit writers — can still scrub it.
//
// It is the second consumer of one set of rules. runner.NewRedactor builds a
// per-run dictionary (this set narrowed to a scope, plus the dispatch-time
// values only that run knows: Vault-sourced secrets, delivered SSH keys). Both
// use Skip/AddValue/Mask from here so they cannot drift on what counts as a
// secret.
//
// Imports secrets, config, metrics and the leaf auditlog (for the installer's
// self-audit row); imported by runner and main. It must never import settings
// or sshkeys — those are the writers whose changes invalidate it, and they
// reach it through secrets.RedactionSourceChanged.
package redactdict

import (
	"bytes"
	"sort"
	"strings"
)

// Mask is the replacement text.
const Mask = "[REDACTED]"

// minLen: values shorter than this are never added. Short values cause
// false-positive scrambling of normal text; the residual (a genuine secret
// under 5 chars is not masked) is intentional and accepted (V1.1-9).
const minLen = 5

// commonLiterals are non-sensitive runtime tokens that must never enter a
// dictionary even if a secret happens to equal one — they would otherwise
// scramble ordinary text (V1.1-9). Lowercased; matched case-insensitively.
var commonLiterals = map[string]struct{}{
	"true": {}, "false": {}, "null": {}, "none": {}, "nil": {},
	"trace": {}, "debug": {}, "info": {}, "warn": {}, "warning": {}, "error": {}, "fatal": {},
	"prod": {}, "production": {}, "dev": {}, "development": {}, "stage": {}, "staging": {}, "test": {}, "testing": {},
	"local": {}, "localhost": {}, "default": {}, "unknown": {},
	"enabled": {}, "disabled": {}, "active": {}, "inactive": {}, "online": {}, "offline": {},
}

// Skip reports whether a candidate value is too short or too common to safely
// add to a dictionary (V1.1-9).
func Skip(v string) bool {
	if len(v) < minLen {
		return true
	}
	_, common := commonLiterals[strings.ToLower(strings.TrimSpace(v))]
	return common
}

// AddValue appends v to vals (when not skipped) and — for a multi-line value
// such as a PEM private key — ALSO each of its lines (PP-B4a). Redaction runs
// per single log line, so a multi-line secret echoed by a job would never match
// as one blob; adding the discriminating lines (the long, unique base64 body
// lines) lets a line-by-line redactor mask it. Trivial fragments are dropped
// via Skip.
func AddValue(vals []string, v string) []string {
	if v == "" {
		return vals
	}
	if !Skip(v) {
		vals = append(vals, v)
	}
	if strings.ContainsAny(v, "\r\n") {
		for _, line := range strings.FieldsFunc(v, func(r rune) bool { return r == '\n' || r == '\r' }) {
			line = strings.TrimSpace(line)
			if line != "" && !Skip(line) {
				vals = append(vals, line)
			}
		}
	}
	return vals
}

// Dictionary is an immutable, longest-first ordered set of values to mask.
type Dictionary struct {
	values []string
}

// New builds a Dictionary from raw candidate values, applying AddValue to each
// and ordering longest-first so greedy replacement masks superstrings before
// substrings.
func New(raw ...string) *Dictionary {
	var vals []string
	for _, v := range raw {
		vals = AddValue(vals, v)
	}
	return FromValues(vals)
}

// FromValues wraps values that have ALREADY been through AddValue (a caller
// composing a base set with per-run extras). It sorts; it does not filter.
func FromValues(vals []string) *Dictionary {
	out := make([]string, len(vals))
	copy(out, vals)
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return &Dictionary{values: out}
}

// Len is the number of values, for the size gauge and tests.
func (d *Dictionary) Len() int {
	if d == nil {
		return 0
	}
	return len(d.values)
}

// Values returns a copy of the ordered values, for composing a per-run
// dictionary on top. Never log them (S7).
func (d *Dictionary) Values() []string {
	if d == nil {
		return nil
	}
	out := make([]string, len(d.values))
	copy(out, d.values)
	return out
}

// Redact replaces every known value in p with Mask. Returns the redacted bytes;
// never logs the original (S7). A nil Dictionary masks nothing.
func (d *Dictionary) Redact(p []byte) []byte {
	if d == nil || len(d.values) == 0 {
		return p
	}
	for _, v := range d.values {
		if bytes.Contains(p, []byte(v)) {
			p = bytes.ReplaceAll(p, []byte(v), []byte(Mask))
		}
	}
	return p
}

// RedactString is Redact over a string.
func (d *Dictionary) RedactString(s string) string {
	if d == nil || len(d.values) == 0 || s == "" {
		return s
	}
	return string(d.Redact([]byte(s)))
}
