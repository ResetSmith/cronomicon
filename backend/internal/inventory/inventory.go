// Package inventory holds the shared, dependency-free logic for handling Ansible
// inventory files. It is imported by BOTH the git-sync ingest (internal/gitlab)
// and the in-app upload handler (internal/settings) so the rules live in exactly
// one place and the two ingest paths cannot drift (the ansible-inventory plan
// §4.0). It imports only the standard library — no project packages — so neither
// caller risks an import cycle.
//
// M1 implements ValidateSecrets (Path A secret rejection). The best-effort parser
// (ParseProjection: groups/host_vars/group_vars) lands in M2.
package inventory

import (
	"bufio"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// secretBearingVars is the explicit, named reject-set for inline secret VALUES
// (Path A / D1; the ansible-inventory plan §4.3, OD-5 → narrow explicit list).
//
// DECISION (OD-5, option b): an explicit list, NOT a *_pass/*_password prefix
// match. Accepted tradeoff — it can miss a future connection plugin's password
// var (e.g. some WinRM aliases). When a new plugin is adopted, add its pass var
// here; this map is the single source of truth (the matcher regex is derived
// from it at init).
var secretBearingVars = map[string]bool{
	"ansible_ssh_pass":        true,
	"ansible_password":        true,
	"ansible_become_pass":     true,
	"ansible_become_password": true,
	"ansible_sudo_pass":       true,
	"ansible_sudo_password":   true,
	"ansible_su_pass":         true,
	"ansible_su_password":     true,
	"ansible_paramiko_pass":   true,
}

// secretKeyRe matches `key = value` for any reject-set key, capturing the value
// whether double-quoted, single-quoted, or a bareword. Built from the map above
// so the key list has one home. The value alternation keeps a quoted value with
// embedded whitespace (the allowed env-lookup form) as a SINGLE capture, which a
// naive whitespace split would shatter.
var secretKeyRe = func() *regexp.Regexp {
	keys := make([]string, 0, len(secretBearingVars))
	for k := range secretBearingVars {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Strings(keys)
	return regexp.MustCompile(`(?i)\b(` + strings.Join(keys, "|") +
		`)\s*=\s*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s]+)`)
}()

// envLookupRe is the ONLY allowed VALUE form for a secret-bearing var: the entire
// (quote-stripped, trimmed) value must be an Ansible env lookup naming the secret
// the runner resolves locally, e.g. {{ lookup('env','NAME') }}. Anchored (^...$)
// so a mixed literal+lookup value like "hunter2 {{ lookup('env','X') }}" cannot
// smuggle a literal past the check.
var envLookupRe = regexp.MustCompile(
	`^\{\{\s*lookup\(\s*['"](?:ansible\.builtin\.)?env['"]\s*,\s*['"][A-Za-z_][A-Za-z0-9_]*['"]\s*\)\s*\}\}$`)

// reservedAssignRe matches a LITERAL assignment (`=`) to an CRONOMICON_-namespaced
// var anywhere on a line — standalone or an inline host-var. It deliberately
// matches the KEY position (a name followed by `=`), so an CRONOMICON_ name that
// appears only INSIDE a lookup value (e.g. lookup('env','CRONOMICON_SECRET_X'), the
// intended reference form) is NOT matched — there is no `=` after the name there.
// The run-env reserved guard (W4 / namespace plan N-D1) is absolute: operator
// content may never DEFINE an CRONOMICON_* key, only reference one.
var reservedAssignRe = regexp.MustCompile(`\bCRONOMICON_[A-Za-z0-9_]*\s*=`)

// SecretError is one secret-rejection finding (one offending line). It implements
// error so callers can append it to an []error error list directly; Error()
// carries the actionable file:line:message the operator sees.
type SecretError struct {
	File    string
	Line    int
	Var     string
	Message string
}

func (e SecretError) Error() string {
	return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Message)
}

// ValidateSecrets scans raw inventory content for inline secret VALUES (Path A /
// D1). It returns one SecretError per offending line; an empty result means the
// content is safe to persist and ship to a runner. file is the repo-relative
// path used in the error (e.g. "inventory/prod.ini").
//
// It scans RAW lines (not a parsed projection) on purpose: the projection is
// best-effort and is suppressed on exactly the constructs where a secret could
// hide, so a projection-only scan would have a blind spot. The scan is the SINGLE
// load-bearing guard keeping decrypted secrets off the runner path — fail-closed.
//
// SCOPE: this matcher targets the INI `key=value` form (the only inventory format
// ingested today — sync hard-codes Format="ini"). The allowed env-lookup VALUE
// must be QUOTED so its inner whitespace stays in one token (the documented
// remediation always quotes; an unquoted lookup with spaces fails closed, i.e. is
// rejected). Before ANY YAML ingest path (`key: value`) is enabled, this matcher
// MUST be extended to cover it (or YAML must be refused at ingest) — otherwise a
// YAML-form secret would slip past.
func ValidateSecrets(content, file string) []SecretError {
	var errs []SecretError
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		// Skip blank lines and full-line comments (Ansible INI: # or ;).
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		// Inline Ansible Vault content is always rejected — it is opaque cipher
		// text the runner can't (and shouldn't, under model b) decrypt server-side.
		if strings.Contains(line, "$ANSIBLE_VAULT") || strings.Contains(line, "!vault") {
			errs = append(errs, SecretError{File: file, Line: lineNo, Var: "vault",
				Message: "inline Ansible Vault content is not allowed in an Cronomicon-managed inventory; " +
					"Cronomicon never carries secret values on the runner path — provision the secret on the runner and reference it by env-var NAME"})
			continue
		}
		for _, m := range secretKeyRe.FindAllStringSubmatch(line, -1) {
			key := strings.ToLower(m[1])
			if isEnvLookup(m[2]) {
				continue // allowed: value is a bare {{ lookup('env','NAME') }}
			}
			errs = append(errs, SecretError{File: file, Line: lineNo, Var: key,
				Message: fmt.Sprintf("secret-bearing var %q is not allowed; Cronomicon never carries secret values on the "+
					"runner path. Use env-var-NAME indirection resolved by the runner, e.g. %s=\"{{ lookup('env','NAME') }}\"", key, key)})
		}
		// Reserved-namespace guard (W4 / N-D1): reject a literal assignment of an
		// CRONOMICON_* var. The prefix is injector-owned — an inventory may REFERENCE a
		// runner env var by name (lookup('env','CRONOMICON_SECRET_X')) but may never
		// DEFINE an CRONOMICON_* key, which would shadow/spoof an injected reference.
		if loc := reservedAssignRe.FindString(line); loc != "" {
			name := strings.TrimRight(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(loc), "=")), " ")
			errs = append(errs, SecretError{File: file, Line: lineNo, Var: name,
				Message: fmt.Sprintf("reserved var %q may not be defined in an inventory; CRONOMICON_* names are references Cronomicon injects, not variables you set. Reference a runner-provisioned value instead, e.g. lookup('env','%s')", name, name)})
		}
	}
	// Fail CLOSED if the scan could not complete — e.g. a line exceeds the 1 MiB
	// buffer (bufio.ErrTooLong). A silently truncated scan must NEVER be treated
	// as "no secrets found": that would let a secret-bearing line past the only
	// guard keeping decrypted secrets off the runner path. Reject the whole scope.
	if err := sc.Err(); err != nil {
		errs = append(errs, SecretError{File: file, Line: lineNo + 1, Var: "scan",
			Message: "inventory could not be fully scanned for secrets (oversized or unparseable line); " +
				"refusing it fail-closed — split lines longer than 1 MiB"})
	}
	return errs
}

// secretAssignRe matches a secret-bearing var assigned to a value via either
// `=` (INI/ansible.cfg) or `:` (YAML vars/host_vars), capturing the value the
// same quote-aware way secretKeyRe does. Used by the project tree secret-scan
// (§6.2), which must catch YAML-form secrets the INI-only ValidateSecrets never
// sees. Built from the same reject-set so the key list has one home.
var secretAssignRe = func() *regexp.Regexp {
	keys := make([]string, 0, len(secretBearingVars))
	for k := range secretBearingVars {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	sort.Strings(keys)
	return regexp.MustCompile(`(?i)\b(` + strings.Join(keys, "|") +
		`)\s*[:=]\s*("(?:[^"\\]|\\.)*"|'[^']*'|[^\s]+)`)
}()

// SecretHit is one inline plaintext-secret finding from a project-tree scan
// (§6.2). Var is the offending variable name; Message is operator-facing.
type SecretHit struct {
	Line    int
	Var     string
	Message string
}

// ScanSecretAssignments scans arbitrary text (a project YAML/INI/template file)
// for a secret-bearing var assigned a LITERAL value — the thing the value-based
// log redactor can never mask because Cronomicon never saw it. The allowed
// env-lookup indirection form ({{ lookup('env','NAME') }}) is not flagged.
// Full-line comments (# or ;) are skipped. Callers exempt vault-encrypted files
// before calling (a $ANSIBLE_VAULT payload is opaque ciphertext).
func ScanSecretAssignments(content string) []SecretHit {
	var hits []SecretHit
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		for _, m := range secretAssignRe.FindAllStringSubmatch(line, -1) {
			if isEnvLookup(m[2]) {
				continue // allowed: env-var-NAME indirection
			}
			key := strings.ToLower(m[1])
			hits = append(hits, SecretHit{Line: lineNo, Var: key,
				Message: fmt.Sprintf("secret-bearing var %q assigned a literal value — Cronomicon never carries secret values on the runner path and cannot redact this; use env-var indirection (lookup('env','NAME')) or Ansible Vault", key)})
		}
	}
	return hits
}

// envLookupScanRe finds every {{ lookup('env','NAME') }} reference anywhere in
// inventory content (unanchored, unlike envLookupRe which validates that a
// secret-bearing VALUE is exactly one lookup). Capture group 1 is the env-var
// NAME.
var envLookupScanRe = regexp.MustCompile(
	`\{\{\s*lookup\(\s*['"](?:ansible\.builtin\.)?env['"]\s*,\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\)`)

// EnvLookupNames extracts the deduplicated, sorted set of env-var NAMES the
// content references via {{ lookup('env', NAME) }} (both bare and
// ansible.builtin.env forms). Used to compute the manifest env-passthrough
// allowlist (ansible-update.md RX.9): a scoped-env runner only forwards its
// local env vars to a playbook when their NAMES appear in this set (or the
// other allowlist sources) — names only, never values (D1).
func EnvLookupNames(content string) []string {
	seen := map[string]bool{}
	for _, m := range envLookupScanRe.FindAllStringSubmatch(content, -1) {
		seen[m[1]] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// isEnvLookup reports whether a captured value (possibly surrounded by one layer
// of matching quotes) is exactly the allowed env-lookup form.
func isEnvLookup(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = strings.TrimSpace(v[1 : len(v)-1])
		}
	}
	return envLookupRe.MatchString(v)
}
