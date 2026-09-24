// Package runref resolves a run's declared Env Vars references (the "reference
// bindings") into values injected into the run at dispatch time.
//
// It is the core of the injection workflow (the vault-integration plan,
// Phase 1). A job or script declares — via the reference_bindings table
// (migration 590) — which Secrets, Variables, and SSH Keys it consumes, by BARE
// row name. At dispatch the Resolver (resolve.go) turns those bindings into
//
//   - Secrets → sensitive VALUES (via secrets.Service.Reveal, which is
//     source-transparent: stored-envelope OR Vault), keyed by CRONOMICON_SECRET_<name>
//   - Variables → log-safe VALUES (from env_vars), keyed by CRONOMICON_VAR_<name>
//   - SSH Keys → key MATERIAL (decrypted PEM), surfaced as CRONOMICON_KEY_<name>
//
// The derived reference form (CRONOMICON_<SECTION>_<name>) is owned by envref; this
// package never stores a prefix. Because Reveal hides the storage backend, the
// whole workflow is backend-agnostic — Vault is simply one secret source (Phase 2
// wires it in behind the same seam with no change here).
//
// This package imports secrets/sshkeys/settings-free stdlib + those leaf stores;
// nothing imports it, so it is safe to call from both executor seams (P1.3 SSH,
// P1.4 runner) without an import cycle.
package runref

import (
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// Kind is the Env Vars section a binding/reference targets.
type Kind string

const (
	KindSecret Kind = "secret"
	KindVar    Kind = "var"
	KindKey    Kind = "key"
)

// ValidKind reports whether k is one of the three reference sections.
func ValidKind(k Kind) bool {
	switch k {
	case KindSecret, KindVar, KindKey:
		return true
	}
	return false
}

// Reference derives the CRONOMICON_* reference for a (kind, bare name); "" for an
// invalid kind.
func (k Kind) Reference(name string) string {
	switch k {
	case KindSecret:
		return envref.SecretReference(name)
	case KindVar:
		return envref.VarReference(name)
	case KindKey:
		return envref.KeyReference(name)
	}
	return ""
}

// KindForSection is the inverse the scanner uses (envref.Split → Kind). ok is
// false for SectionRun/SectionNone: CRONOMICON_RUN_* is dispatcher-owned context,
// not a binding.
func KindForSection(s envref.Section) (Kind, bool) {
	switch s {
	case envref.SectionSecret:
		return KindSecret, true
	case envref.SectionVariable:
		return KindVar, true
	case envref.SectionKey:
		return KindKey, true
	}
	return "", false
}

// Binding is a single declared reference a job/script consumes. Name is the BARE
// row name; Reference is the derived CRONOMICON_<SECTION>_<name> (populated on read,
// ignored on write).
//
// As is the optional ALIAS (RA-1): the bare DESTINATION name the resolved value is
// injected under. Empty ⇒ the value is injected under the row's own name, which is
// the pre-Phase-A behaviour verbatim.
//
// ⚠️ The alias is a destination, NEVER a selector (plan §2.2). Name alone
// identifies the row, and the row is resolved by the one visibility predicate
// (lookupScoped) under the run's scope and frozen agency snapshot. The alias is
// applied at the LAST step, where the injected env key is built. Any code path
// that resolves BY alias — or lets an alias widen what a binding can reach —
// inverts the least-privilege model the whole package exists to enforce.
type Binding struct {
	Kind      Kind   `json:"kind"`
	Name      string `json:"name"`
	As        string `json:"as,omitempty"`
	Reference string `json:"reference"`
	// File requests FILE delivery instead of an environment VALUE (RA-12, Phase B):
	// the resolved secret is written to a 0600 file off the run tree and the
	// binding's reference holds the PATH. Secrets only — variables are log-safe and
	// gain nothing, and keys already work this way.
	//
	// This is the generalised form RA-Q9 chose over a become-specific channel: the
	// mechanism is "deliver this secret as a file", and `--become-password-file` is
	// simply its first consumer. It mirrors CRONOMICON_KEY_* exactly, where the
	// reference has always resolved to a path rather than to material.
	//
	// Why it matters for a become password specifically: an environment variable is
	// visible in /proc on the target for anything that can read the process env, and
	// an operator who echoes it into `sudo -S` puts it in the process table. A file
	// the agent wipes at run end is a materially smaller window.
	File bool `json:"file,omitempty"`
}

// InjectName is the bare name this binding's value is injected under: the alias
// when one is declared, the row's own name otherwise.
func (b Binding) InjectName() string {
	if b.As != "" {
		return b.As
	}
	return b.Name
}

// InjectReference is the derived CRONOMICON_<SECTION>_<name> KEY the value lands on —
// the alias-aware counterpart of Kind.Reference(Name). This is what both executor
// seams must key their env maps by; Reference (the row's own derived form) stays
// the binding's identity for display and audit.
func (b Binding) InjectReference() string { return b.Kind.Reference(b.InjectName()) }

// ValidateName applies the bare-name rules for a reference of this kind: a secret
// carries the stricter reserved-KEK bar (a row named KEK/… would derive to the
// evicted CRONOMICON_KEK config name), var and key carry the base POSIX-identifier
// charset. Returns an *envref.Error, which every caller maps to 422.
//
// It is exported because FOUR surfaces must agree on the rule — ReplaceBindings,
// the per-run additions on the trigger route, the authoring validator, and alias
// validation below. An inlined copy is exactly how one of them comes to accept a
// name the others reject.
func ValidateName(kind Kind, name string) error {
	if kind == KindSecret {
		return envref.ValidateSecretRowName(name)
	}
	return envref.ValidateRowName(name)
}

// ValidateBinding checks a binding's kind, its row name, and — when aliased — its
// destination name. The alias runs through the SAME charset rules as a row name of
// that kind (RA-1): an alias mints an env-var key exactly as a row name does, so a
// weaker bar here would let a binding produce a malformed or reserved key that no
// row could ever have been named.
func ValidateBinding(b Binding) error {
	if !ValidKind(b.Kind) {
		return &envref.Error{Msg: fmt.Sprintf("invalid reference kind %q: must be secret, var, or key", b.Kind)}
	}
	if err := ValidateName(b.Kind, b.Name); err != nil {
		return err
	}
	if b.As != "" {
		if err := ValidateName(b.Kind, b.As); err != nil {
			return &envref.Error{Msg: "invalid alias: " + err.Error()}
		}
	}
	return nil
}

// DedupeKey is the identity of a binding for de-duplication: kind + row name +
// alias (RA-4). The same row aliased twice is a DIFFERENT binding — it lands two
// values in the run env — so alias belongs in the key; an exact triple repeated
// across a job, its script and the per-run additions is one binding declared
// three times and collapses to one.
func DedupeKey(b Binding) string {
	// Delivery mode is part of the identity: the same secret delivered as a VALUE
	// and as a FILE are two different things landing on two different keys, and
	// collapsing them would silently drop one.
	mode := ""
	if b.File {
		mode = "\x00file"
	}
	return string(b.Kind) + "\x00" + b.Name + "\x00" + b.As + mode
}

// CheckAliasCollisions rejects a binding set in which two DISTINCT bindings would
// inject under the same key (RA-Q2). Aliasing makes this reachable in a way it
// never was before: {secret X} and {secret Y as X} both target CRONOMICON_SECRET_X,
// as do {secret Y as X} and {secret Z as X}.
//
// There is no defensible silent winner. Map-write order decides which value lands,
// which for secret material means the run escalates with whichever credential the
// iteration happened to reach last — and the audit would record both as injected.
// So this fails, and it fails everywhere the set is assembled: the authoring write,
// the trigger boundary (422), and dispatch itself over the UNION of declared and
// per-run bindings, which is the only place the full set exists.
//
// Bindings must already be deduped by DedupeKey; an exact repeat is one binding.
func CheckAliasCollisions(bindings []Binding) error {
	seen := make(map[string]Binding, len(bindings))
	for _, b := range bindings {
		key := string(b.Kind) + "\x00" + b.InjectName()
		prev, clash := seen[key]
		if clash && prev.Name != b.Name {
			return &envref.Error{Msg: fmt.Sprintf(
				"reference collision: %s and %s both inject as %s — give one of them a different alias",
				prev.Kind.Reference(prev.Name), b.Kind.Reference(b.Name), b.InjectReference())}
		}
		if clash && prev.File != b.File {
			// RA-12: the SAME row wanted both as a value and as a file, on one key.
			// This is not harmless the way a repeated spelling is — the two land on the
			// same env key with different CONTENTS (the secret vs. a path to it), and
			// whichever the agent writes last wins. A job body reading it would get a
			// path where it expected a password, or the reverse, with nothing saying so.
			return &envref.Error{Msg: fmt.Sprintf(
				"reference collision: %s is bound both as a value and as a file on the same key %s — "+
					"alias one of them so each has its own name",
				b.Kind.Reference(b.Name), b.InjectReference())}
		}
		if clash {
			// Same row, same destination, same delivery (X bound bare and again `as X`).
			// Harmless — one value, one key — so collapse rather than refuse a set that
			// means exactly what it says.
			continue
		}
		seen[key] = b
	}
	return nil
}

// Owner identifies the job or script a set of bindings belongs to. Source is the
// job source ('amadeus'|'git'); scripts (a single-namespace catalog) use "".
//
// R2F-1: UID is the owner's permanent identity, and once two departments may own
// a job of the same name (R2-5) it is the ONLY thing that tells the twins apart.
// Set it for job owners whenever the caller resolved a real row — every caller
// that fetched a job, or that holds a run (runs.job_uid since R2-1), does. Leave
// it empty for script owners: scripts keep PRIMARY KEY (name) and stayed outside
// AF-4b, so their identity is permanently their name.
//
// An empty UID on a job owner is not an error — it means the caller cannot say
// WHICH twin it means, and gets the legacy shared name-keyed namespace it always
// had. The store deliberately does not resolve one by name on its behalf: under
// duplicates that would be a guess, and a guess that silently deletes the other
// twin's bindings is exactly the defect R2F-1 exists to remove.
type Owner struct {
	Kind   string
	Source string
	Name   string
	UID    string
}

// UIDKey is the identity the binding store keys this owner by: the job uid when
// the caller resolved one, "" otherwise. A uid set on a SCRIPT owner is ignored
// rather than written — scripts are name-identified permanently, and honouring a
// stray uid would file a script's bindings under a job's identity where nothing
// would ever read them back.
func (o Owner) UIDKey() string {
	if o.Kind != "job" {
		return ""
	}
	return o.UID
}
