// Package envref is the single source of truth for the CRONOMICON_* env-var
// namespace contract (the namespace-update plan, Option B).
//
// Rule: if an CRONOMICON_* name starts with one of the reserved reference prefixes
// (VAR_, SECRET_, KEY_, RUN_) it is a REFERENCE the app resolves and injects into
// runs; every other CRONOMICON_* name is server/runner configuration.
//
// The prefix is reference syntax, not stored data: rows keep bare names and the
// reference is DERIVED at use time (reference = CRONOMICON_<SECTION>_<bare name>).
// Resolution strips the known prefix verbatim (case-preserved) to locate the
// row/file.
//
// This package imports only the standard library so any subsystem — server or
// runner — can use it without risking an import cycle.
package envref

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Namespace is the umbrella prefix for every Cronomicon-owned env-var name (config
// AND references).
const Namespace = "CRONOMICON_"

// Reserved reference prefixes (the closed allowlist). Exact-prefix matching:
// CRONOMICON_SECRET_ never matches CRONOMICON_SECRETS_.
const (
	PrefixVar    = "CRONOMICON_VAR_"
	PrefixSecret = "CRONOMICON_SECRET_" //nolint:gosec // G101: an env-var name prefix, not a credential
	PrefixKey    = "CRONOMICON_KEY_"
	PrefixRun    = "CRONOMICON_RUN_"
)

// PrefixRunnerConfig is the runner agent's own configuration namespace
// (CRONOMICON_RUNNER_SERVER, _REGISTRATION_TOKEN, _CHECKOUT_TOKEN, _VAULT_PASSWORD_FILE,
// …). It is NOT a reference prefix: nothing under it resolves to a store row, and
// a job may never read it — see IsAgentConfig.
//
// Deliberately distinct from PrefixRun ("CRONOMICON_RUN_", the run-context names a
// job MAY read). The two differ only by the letters "NER" and are easy to misread,
// but they are separate namespaces with OPPOSITE access rules: CRONOMICON_RUN_* is
// injected into every run on purpose; CRONOMICON_RUNNER_* is refused outright.
const PrefixRunnerConfig = "CRONOMICON_RUNNER_"

// IsAgentConfig reports whether name belongs to the runner agent's own
// configuration namespace, and is therefore never a legitimate job env
// passthrough (DR-1).
//
// The agent's environment holds this runner's credentials: the registration or
// bootstrap token, the checkout deploy token, the vault password path. A job that
// could name one of these in env_passthrough would have it resolved out of the
// agent's environment and handed to the child process — and because a passthrough
// value is not a declared secret binding, nothing registers it with the per-run
// log redactor. Under a multi-use bootstrap token that is a permanent enrollment
// credential printed in plaintext, so the namespace is unreachable from a child
// by ANY route.
func IsAgentConfig(name string) bool {
	return strings.HasPrefix(name, PrefixRunnerConfig)
}

// Error is a namespace-contract validation failure (bad row name, or a reserved
// key in operator-authored env). Callers map it to HTTP 422 via errors.As.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, a ...any) error { return &Error{Msg: fmt.Sprintf(format, a...)} }

// Reserved CRONOMICON_RUN_* run-context names — the fixed, dispatcher-owned set
// (namespace plan N-D4, vault-integration.md D7). The executor injects these into
// every run; they are NOT bindable (KindForSection rejects SectionRun) and never
// resolve from a store row. Extend deliberately: each is a stable contract a run
// author may read. Values are always log-safe (a run's own metadata).
const (
	RunID          = PrefixRun + "ID"           // the run / trace id (runs.id)
	RunJob         = PrefixRun + "JOB"          // job name
	RunJobSource   = PrefixRun + "JOB_SOURCE"   // git | cronomicon
	RunScope       = PrefixRun + "SCOPE"        // run scope ("" = global)
	RunType        = PrefixRun + "TYPE"         // bash | ansible | terraform | ...
	RunTriggeredBy = PrefixRun + "TRIGGERED_BY" // the actor that triggered the run
	RunExecutor    = PrefixRun + "EXECUTOR"     // ssh | runner
)

// Section identifies which Env Vars section a derived reference routes to.
type Section int

const (
	SectionNone Section = iota
	SectionVariable
	SectionSecret
	SectionKey
	SectionRun
)

// HasCronomiconPrefix reports whether name is in the CRONOMICON_ namespace at all. This
// is the test the run-env guard uses: operator-authored env may never define ANY
// CRONOMICON_* key (N-D1, absolute, no carve-outs).
func HasCronomiconPrefix(name string) bool {
	return strings.HasPrefix(name, Namespace)
}

// Split routes a derived reference to its section and bare row name. ok is false
// (Section SectionNone) when name is not a reserved reference. Case-preserved.
func Split(name string) (section Section, bare string, ok bool) {
	switch {
	case strings.HasPrefix(name, PrefixVar):
		return SectionVariable, name[len(PrefixVar):], true
	case strings.HasPrefix(name, PrefixSecret):
		return SectionSecret, name[len(PrefixSecret):], true
	case strings.HasPrefix(name, PrefixKey):
		return SectionKey, name[len(PrefixKey):], true
	case strings.HasPrefix(name, PrefixRun):
		return SectionRun, name[len(PrefixRun):], true
	}
	return SectionNone, "", false
}

// StripKey returns the bare name behind an CRONOMICON_KEY_ reference; ok is false
// (and the input is returned unchanged) when name has no key prefix.
func StripKey(name string) (bare string, ok bool) {
	if strings.HasPrefix(name, PrefixKey) {
		return name[len(PrefixKey):], true
	}
	return name, false
}

// StripValueReference returns the bare name behind an CRONOMICON_SECRET_ or
// CRONOMICON_VAR_ reference (both resolve to a value); ok is false when name is
// neither. Used by the agent env bridge's prefixed→bare fallback.
func StripValueReference(name string) (bare string, ok bool) {
	if strings.HasPrefix(name, PrefixSecret) {
		return name[len(PrefixSecret):], true
	}
	if strings.HasPrefix(name, PrefixVar) {
		return name[len(PrefixVar):], true
	}
	return name, false
}

// VarReference / SecretReference / KeyReference build the derived reference an
// operator copies for a bare row name.
func VarReference(bare string) string    { return PrefixVar + bare }
func SecretReference(bare string) string { return PrefixSecret + bare }
func KeyReference(bare string) string    { return PrefixKey + bare }

// rowNameRe is the POSIX-identifier charset a row name must match so its derived
// reference is a legal env-var name everywhere Cronomicon executes (POSIX shells,
// systemd, PowerShell). Lowercase is legal — existing names are preserved
// verbatim — though upper-snake is recommended for new rows.
var rowNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// kekRe matches the reserved KEK config names (KEK, KEK_FILE, KEK_VERSION, and
// the KEK_<N> / KEK_<N>_FILE rotation forms).
//
// The bar originally existed because a Secrets row named KEK derived to
// CRONOMICON_SECRET_KEK, which WAS a live config alias for the key-encryption key
// until v1.5.41 removed the alias — so the collision it was written to prevent
// no longer exists. It stays anyway, as naming hygiene rather than as a
// correctness guard: the KEK is app config and explicitly NOT a stored secret
// (that distinction is the whole point of evicting it from the CRONOMICON_SECRET_*
// prefix), so a Secrets row called KEK invites exactly the confusion the
// eviction was meant to end. Removing it would only widen what is accepted, and
// nobody has asked for the name.
var kekRe = regexp.MustCompile(`^KEK(_FILE|_VERSION|_[0-9]+(_FILE)?)?$`)

// ValidateRowName enforces the derived-reference charset and the no-self-prefix
// rule for a row in any Env Vars section. Applies to WRITES only — existing rows
// are never re-validated.
func ValidateRowName(name string) error {
	if name == "" {
		return errf("name is required")
	}
	if HasCronomiconPrefix(name) {
		return errf("invalid name %q: a row name may not start with CRONOMICON_ — the prefix is reference syntax, derived automatically", name)
	}
	if !rowNameRe.MatchString(name) {
		return errf("invalid name %q: must match %s (a POSIX identifier) so its CRONOMICON_ reference is a legal env-var name", name, rowNameRe.String())
	}
	return nil
}

// ValidateSecretRowName is ValidateRowName plus the reserved-KEK bar: a Secrets
// row named KEK/KEK_FILE/… is rejected. See kekRe for why the bar outlived the
// collision it was written for.
func ValidateSecretRowName(name string) error {
	if err := ValidateRowName(name); err != nil {
		return err
	}
	if kekRe.MatchString(name) {
		return errf("invalid secret name %q: reserved — its %s reference would collide with the KEK config name CRONOMICON_%s", name, SecretReference(name), name)
	}
	return nil
}

// ValidateOperatorEnv rejects any CRONOMICON_-namespaced key in operator-authored
// run env (job env, schedule env, per-run override, workflow step inputs).
// Absolute — no carve-outs (N-D1): these names are injector-owned, so allowing
// an operator to define one would let them spoof/shadow a reference or run
// context. Returns an error naming the first (sorted) offending key, or nil.
func ValidateOperatorEnv(env map[string]string) error {
	var bad []string
	for k := range env {
		if HasCronomiconPrefix(k) {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return errf("env key %q is reserved: operator-authored env may not define any CRONOMICON_* name — these are references Cronomicon injects, not settings. To use a stored value, reference it (e.g. lookup('env','%s…') in an inventory) rather than defining the key here", bad[0], Namespace)
}
