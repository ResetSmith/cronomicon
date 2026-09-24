package runref

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
)

// KeyMaterial is a resolved SSH-key reference: the decrypted private-key PEM plus
// its derived AMADEUS_KEY_<name> reference. The executor writes it to a
// memfd/tmpfs file (0600) at dispatch (P1.4/D8) and points AMADEUS_KEY_<name> at
// the path; the resolver only surfaces material and never touches the filesystem.
//
// RA-5: Reference carries the ALIAS when the binding declared one (it is the env
// key a job body reads), while Name stays the credential's own label — the agent
// keys its key-map by Name so a delivered key still resolves for the runner's own
// signer lookup. The two deliberately differ for an aliased key binding.
type KeyMaterial struct {
	Reference string
	Name      string
	Material  string
}

// FileMaterial is a resolved secret bound for FILE delivery (RA-12): its derived
// reference (alias-aware), its bare row name, and the value the executor writes to
// a 0600 file off the run tree. The resolver never touches the filesystem — it only
// surfaces the bytes, exactly as it does for key material.
type FileMaterial struct {
	Reference string
	Name      string
	Value     string
}

// Resolved is the output of a resolve pass: injectable values plus the redaction
// dictionary the run's log sinks must mask.
type Resolved struct {
	// Env maps a derived reference key (AMADEUS_SECRET_x / AMADEUS_VAR_x) to its
	// value, injected verbatim into the run child env at the executor seam.
	Env map[string]string
	// Keys are resolved SSH key materials (AMADEUS_KEY_* references).
	Keys []KeyMaterial
	// Files are resolved secrets the executor must deliver as FILES rather than as
	// environment values (RA-12): the reference resolves to a path. Same delivery
	// shape as Keys — 0600, off the run tree, wiped at run end.
	Files []FileMaterial
	// Redact lists every SENSITIVE value resolved (secret values + key material)
	// for the run's log-redaction dictionary. Variable values are log-safe (D7)
	// and are NOT included.
	Redact []string
	// Refs is audit metadata for every reference actually injected (P1.6): kind,
	// bare name, and backing source — NEVER values. The dispatch audit records
	// WHICH references a run received, not their contents.
	Refs []ResolvedRef
}

// ResolvedRef is dispatch-audit metadata for one injected reference: its kind, bare
// name, the alias it was injected under (RA-7; empty when injected under its own
// name), and backing source ("stored"|"vault"). It carries NO value.
//
// BOTH names are recorded, not just the destination: with aliasing, several
// departments' rows land on one key, so an audit that said only
// "BECOME_PASSWORD was injected" could not answer whose credential actually ran.
type ResolvedRef struct {
	Kind   Kind
	Name   string
	As     string
	Source string
}

// ErrOutOfScope is returned when a binding names a row that exists but is not
// visible from the run's scope (least-privilege violation). Dispatch fails closed.
var ErrOutOfScope = errors.New("reference is not in the run's scope")

// ErrMissingReference is returned when a binding names a row that does not exist.
var ErrMissingReference = errors.New("referenced Env Vars row not found")

// ErrAmbiguousReference is returned when a bare name resolves to MORE THAN ONE
// row the run is entitled to (RA-17, Phase E): two departments in the run's agency
// snapshot each own a row of that key in that scope.
//
// There is no defensible silent pick. A run's agency snapshot is a SET — a scope
// can belong to several agencies — so whichever row an ORDER BY happened to put
// first, the run would escalate with SOMEBODY's credential and the dispatch audit
// would record an intent that never existed. Fail closed; the escape hatch is a
// per-run reference addition naming the row explicitly, or an alias (Phase A).
var ErrAmbiguousReference = errors.New("reference resolves to more than one row")

// ReferenceError is a fail-closed resolution error for a single binding. It wraps
// ErrOutOfScope or ErrMissingReference (so errors.Is still classifies it) and
// carries the reference's display name. Its Error() is PRECISE (for server logs);
// OperatorMessage is deliberately GENERIC — identical for out-of-scope and missing
// — so an operator cannot use a run as a cross-scope existence oracle (M2): the
// precise "exists only outside run scope" vs "not found" distinction defeated
// P1.7's 404-not-403 design, letting an actor probe any name one run at a time.
type ReferenceError struct {
	Ref string // the AMADEUS_* display name (Kind.Reference(Name)), which the operator already declared
	err error  // wraps ErrOutOfScope or ErrMissingReference plus the precise detail
}

func (e *ReferenceError) Error() string { return e.err.Error() }
func (e *ReferenceError) Unwrap() error { return e.err }

// OperatorMessage returns the generic, oracle-safe message for a resolution error
// destined for an operator-visible surface (SSH run log, runner 409 body). For a
// ReferenceError it collapses out-of-scope and missing into one message naming
// only the reference the operator already declared; any other error passes through
// verbatim (those carry no cross-scope existence signal).
func OperatorMessage(err error) string {
	if re, ok := errors.AsType[*ReferenceError](err); ok {
		// RA-17: ambiguity gets its OWN operator sentence rather than collapsing into
		// "unavailable". Both are oracle-safe — neither names anything the caller did
		// not declare — but sending someone hunting for a MISSING row when the problem
		// is TWO rows is a diagnosis they cannot recover from. Which two departments
		// own it is deliberately withheld here and reported by the authoring-time
		// validator instead, where precision is already bounded by caller visibility.
		if errors.Is(re, ErrAmbiguousReference) {
			return fmt.Sprintf("reference %s is ambiguous for this run: more than one "+
				"department in this run's scope owns a row with that name — select one "+
				"explicitly as a per-run reference, or bind it under an alias", re.Ref)
		}
		return fmt.Sprintf("reference %s is unavailable for this run", re.Ref)
	}
	return err.Error()
}

// Resolver resolves a run's declared reference bindings into injectable values. It
// routes secret rows through secrets.Service.Reveal (stored OR vault, source-
// transparent), variable rows through the plaintext env_vars table, and key rows
// through sshkeys material resolution.
type Resolver struct {
	db  *sql.DB
	cfg *config.Config
	sec *secrets.Service
	log *slog.Logger
}

// NewResolver builds a Resolver. sec must be the app's secrets.Service (already
// wired with a real Vault client when Vault is configured) so vault-source rows
// resolve through the same seam.
func NewResolver(database *sql.DB, cfg *config.Config, sec *secrets.Service, log *slog.Logger) *Resolver {
	if log == nil {
		log = slog.Default()
	}
	return &Resolver{db: database, cfg: cfg, sec: sec, log: log}
}

// Resolve resolves `bindings` for a run in runScope owned by an actor whose
// resolved scope grants are actorScopes (empty ⇒ unrestricted — the admin/dev
// convention shared with the execution handlers). It fails closed on the first
// out-of-scope, missing, or un-revealable binding. When the injection kill-switch
// is off it returns an empty (non-nil) Resolved so callers need no special-casing.
func (r *Resolver) Resolve(ctx context.Context, actorScopes []string, runScope string, runAgencies []string, bindings []Binding) (*Resolved, error) {
	out := &Resolved{Env: map[string]string{}}
	if !r.cfg.SecretsInjectionEnabled {
		return out, nil
	}
	// Actor-scope gate (mirrors execution_mount's scope checks): a restricted actor
	// may not run — hence inject — into a scope outside their grants. Empty
	// actorScopes ⇒ unrestricted; a global run (runScope=="") is always allowed.
	if len(actorScopes) > 0 && runScope != "" && !slices.Contains(actorScopes, runScope) {
		return nil, fmt.Errorf("%w: run scope %q not in the actor's allowed scopes", ErrOutOfScope, runScope)
	}
	// RA-Q2, and THE authoritative check: this is the only place the run's full
	// binding set exists — the job's declared bindings, its script's, the operator's
	// per-run additions and (on the runner path) the implicit connect-as key, unioned.
	// The trigger route rejects a collision inside the per-run additions, but it
	// cannot see a job binding aliased to the same key. Fail before resolving
	// anything: a run must not escalate with whichever of two credentials the map
	// write happened to reach last.
	if err := CheckAliasCollisions(bindings); err != nil {
		return nil, err
	}
	for _, b := range bindings {
		var err error
		switch b.Kind {
		case KindSecret:
			err = r.resolveSecret(ctx, runScope, runAgencies, b, out)
		case KindVar:
			err = r.resolveVar(ctx, runScope, runAgencies, b, out)
		case KindKey:
			err = r.resolveKey(ctx, runAgencies, b, out)
		default:
			err = fmt.Errorf("invalid reference kind %q", b.Kind)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *Resolver) resolveSecret(ctx context.Context, runScope string, runAgencies []string, b Binding, out *Resolved) error {
	id, found, err := lookupScopedID(ctx, r.db, "secrets", b.Name, runScope, runAgencies)
	if err != nil {
		return ambiguityOrErr(b, err)
	}
	if !found {
		return scopeOrMissing(ctx, r.db, "secrets", b, runScope)
	}
	val, err := r.sec.Reveal(ctx, id)
	if err != nil {
		// L3: the row lookupScopedID just found was deleted before Reveal (TOCTOU) —
		// treat it as a missing reference (fail-closed, non-oracle operator message)
		// rather than surfacing a raw reveal error.
		if errors.Is(err, secrets.ErrSecretNotFound) {
			return &ReferenceError{Ref: b.Kind.Reference(b.Name),
				err: fmt.Errorf("%w: %s %q", ErrMissingReference, b.Kind, b.Name)}
		}
		return fmt.Errorf("reveal secret %q: %w", b.Name, err)
	}
	if val == "" {
		// A TOCTOU delete now fails closed above (L3: Reveal returns ErrSecretNotFound),
		// so an empty value here is a legitimately-empty stored secret. Inject it (a
		// bare "" is harmless and NOT added to Redact — an empty redaction token would
		// corrupt masking) but flag it: an empty secret is almost always a misconfig.
		r.log.Warn("resolved secret reference is empty", "reference", b.Kind.Reference(b.Name), "scope", runScope)
	}
	// RA-5: the alias is applied HERE and nowhere earlier — the row was selected by
	// b.Name under the unchanged predicate above, and all the alias does is choose
	// the key the value lands on. Redact is keyed by VALUE, so masking follows the
	// alias for free (RA-8 fences that with a test rather than trusting it).
	//
	// RA-12: a file-mode binding takes the SAME resolved value down a different
	// delivery path — the executor writes it to a 0600 file and the reference holds
	// the path. It is deliberately NOT also placed in Env: shipping both would leave
	// the value in the environment, which is the exposure file delivery exists to
	// avoid. Redaction is unchanged either way, because it is keyed by value.
	if b.File {
		out.Files = append(out.Files, FileMaterial{Reference: b.InjectReference(), Name: b.Name, Value: val})
	} else {
		out.Env[b.InjectReference()] = val
	}
	if val != "" {
		out.Redact = append(out.Redact, val)
	}
	// Audit metadata (P1.6): record the backing source (stored|vault) so the
	// dispatch audit shows provenance. Names/source only — never the value. A
	// deleted-between-queries row (ErrNoRows) correctly keeps the "stored" default;
	// any OTHER error is logged so a mislabel isn't fully silent (matters once
	// vault-source rows exist — a transient fault must not quietly record a vault
	// secret as stored). Resolution itself is unaffected by this read.
	source := "stored"
	if serr := r.db.QueryRowContext(ctx, `SELECT COALESCE(NULLIF(source,''),'stored') FROM secrets WHERE id = ?`, id).Scan(&source); serr != nil && !errors.Is(serr, sql.ErrNoRows) {
		r.log.Warn("audit: could not read secret source; recording as 'stored'", "reference", b.Kind.Reference(b.Name), "error", serr)
		source = "stored"
	}
	out.Refs = append(out.Refs, ResolvedRef{Kind: KindSecret, Name: b.Name, As: b.As, Source: source})
	return nil
}

func (r *Resolver) resolveVar(ctx context.Context, runScope string, runAgencies []string, b Binding, out *Resolved) error {
	// Variables are plaintext and log-safe; the value comes straight from the row.
	// Resolved through lookupScoped so the AG-Q1(b) predicate is applied from ONE
	// place — an inlined copy of the query here is exactly how the agency clause
	// would come to be enforced on secrets but silently not on variables.
	id, _, found, err := lookupScoped(ctx, r.db, "env_vars", b.Name, runScope, runAgencies)
	if err != nil {
		return ambiguityOrErr(b, err)
	}
	if !found {
		return scopeOrMissing(ctx, r.db, "env_vars", b, runScope)
	}
	var value string
	if err := r.db.QueryRowContext(ctx, `SELECT value FROM env_vars WHERE id = ?`, id).Scan(&value); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// TOCTOU: the row lookupScoped just found was deleted before this read.
			// Fail closed as a missing reference, like the secret path (L3).
			return &ReferenceError{Ref: b.Kind.Reference(b.Name),
				err: fmt.Errorf("%w: %s %q", ErrMissingReference, b.Kind, b.Name)}
		}
		return fmt.Errorf("resolve variable %q: %w", b.Name, err)
	}
	out.Env[b.InjectReference()] = value // RA-6 parity: same alias rule as a secret
	// Variables are always plaintext rows in env_vars (D7 log-safe); their source
	// is "stored" for the dispatch audit. Value is never recorded.
	out.Refs = append(out.Refs, ResolvedRef{Kind: KindVar, Name: b.Name, As: b.As, Source: "stored"})
	return nil
}

func (r *Resolver) resolveKey(ctx context.Context, runAgencies []string, b Binding, out *Resolved) error {
	// AG-Q5 (Phase 3) closes the §8 hole this comment used to describe. SSH keys had
	// NO isolation at all: any job could bind any key, which was documented and
	// accepted but is a real least-privilege gap — and it made the whole model less
	// legible, not more (three entity types, three different answers).
	//
	// Keys get AGENCY membership only, not scope (AG-Q5a): the FK-based access story
	// (host/bastion) already provides a narrowing signal, and a scope column on
	// ssh_credentials would be a bigger migration with a new UNIQUE question.
	//
	// **THIS IS A TIGHTENING.** A job binding an out-of-agency key that works today
	// stops working. The graded part — and what keeps it from being a cliff on
	// upgrade — is that an EMPTY membership set means "no agency restriction", the
	// same convention secrets and variables use. Migration 670 deliberately
	// backfills NO key membership, so nothing breaks until an operator assigns some,
	// and GET /agency-preflight (T2.12) reports exactly which bindings each
	// assignment would break, one release ahead.
	//
	// Scope is still NOT applied to keys: ssh_credentials has no scope column, so a
	// job in 'dev' binding a key its agency owns still gets it regardless of scope.
	// RA-19 (Phase E): labels are per-owner unique now, so the row must be SELECTED
	// through the owner-aware predicate — resolving by label with LIMIT 1 would pick
	// one of two departments' same-labelled keys at random, which for key material is
	// the same class of mistake as picking one of two become passwords. One predicate,
	// three tables, or the kinds drift.
	id, found, err := lookupKeyID(ctx, r.db, b.Name, runAgencies)
	if errors.Is(err, ErrAmbiguousReference) {
		return &ReferenceError{Ref: b.Kind.Reference(b.Name),
			err: fmt.Errorf("%w: key %q is owned by more than one of this run's agencies", ErrAmbiguousReference, b.Name)}
	}
	if err != nil {
		return err
	}
	if !found {
		// Same generic operator surface as an out-of-scope secret (M2): the run must
		// not become an existence oracle for key labels either. A label that exists but
		// belongs to another department and one that exists nowhere collapse here.
		return &ReferenceError{Ref: b.Kind.Reference(b.Name),
			err: fmt.Errorf("%w: key %q is not in this run's agencies", ErrOutOfScope, b.Name)}
	}
	// Pass the resolver's configured Vault client so a vault-source key credential
	// resolves through the same client as vault secrets (P2.4/D8).
	material, found, err := sshkeys.ResolveMaterialByID(ctx, r.db, r.cfg, r.sec.Vault(), id)
	if err != nil {
		return fmt.Errorf("resolve key %q: %w", b.Name, err)
	}
	if !found {
		return fmt.Errorf("%w: key %q", ErrMissingReference, b.Name)
	}
	// RA-5: Reference is the ENV KEY the delivered path is exposed under, so it
	// takes the alias. Name stays the row's own label — the agent keys its key-map
	// by it (materializeKeys → resolveKeyPath), so aliasing it would break the
	// runner's own credential lookup for a delivered key.
	out.Keys = append(out.Keys, KeyMaterial{Reference: b.InjectReference(), Name: b.Name, Material: material})
	if material != "" {
		out.Redact = append(out.Redact, material)
	}
	// D8: delivered key material IS now audited, in lockstep with delivery — a key
	// that ships to a runner must leave a dispatch-audit trail exactly like a secret.
	// Best-effort backing source (stored|vault) from ssh_credentials for provenance;
	// never the material. A legacy/fallback-resolved key with no credential row keeps
	// the "stored" default.
	source := "stored"
	if serr := r.db.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(source,''),'stored') FROM ssh_credentials WHERE id = ?`, id).Scan(&source); serr != nil && !errors.Is(serr, sql.ErrNoRows) {
		r.log.Warn("audit: could not read key source; recording as 'stored'", "reference", b.Kind.Reference(b.Name), "error", serr)
		source = "stored"
	}
	out.Refs = append(out.Refs, ResolvedRef{Kind: KindKey, Name: b.Name, As: b.As, Source: source})
	return nil
}

// lookupScopedID finds the best row id in `table` for a bare key visible from
// runScope: a row whose scope is global (NULL/”) or equals runScope, preferring
// the scope-exact row over the global fallback. found=false means no in-scope row
// (the caller distinguishes missing-vs-out-of-scope via scopeOrMissing). `table`
// is always a compile-time constant, never user input.
func lookupScopedID(ctx context.Context, database *sql.DB, table, key, runScope string, runAgencies []string) (id string, found bool, err error) {
	id, _, found, err = lookupScoped(ctx, database, table, key, runScope, runAgencies)
	return id, found, err
}

// lookupScoped is THE visibility predicate for a bare Env Vars name — the single
// query dispatch resolution and the authoring-time validator (validate.go) both
// run, so the two can never drift into disagreeing about what a run can see. It
// additionally returns the WINNING row's scope ("" = global), which the validator
// reports and dispatch ignores. `table` is a compile-time constant.
//
// AG-Q1(b), Phase 3: the predicate is now BOTH clauses, agency AND scope —
//
//	(row has no agency membership OR row.agencies ∩ run.agencies ≠ ∅)
//	AND (row.scope IS NULL/'' OR row.scope = run.scope)
//
// The scope half is unchanged, so nobody loses a control they had. The agency half
// narrows only rows an operator has explicitly given membership: an EMPTY
// membership set means "no agency restriction", which is what keeps every global
// row global and what made migration 670's backfill behavior-preserving. Option
// (a) — agency-only — was rejected because it is a LOOSENING (every job in an
// agency would reach every secret in it) and because it would force
// UNIQUE(key, scope) to be reworked, a migration that can fail on real data.
//
// runAgencies is the RUN'S SNAPSHOT, never live membership (AG-Q8): a run's
// injectable set is determined by the world as it was when the run was authorized,
// so re-homing a scope mid-flight cannot change what an in-flight run can reach.
// RA-17 (Phase E) adds ONE preference tier above the scope rule, and one refusal:
//
//  1. rows whose owner_agency ∈ run.agencies   — "your department's row"
//  2. rows with no owner_agency                — today's shared/global rows
//     (a row owned by an agency NOT in the run's snapshot is already excluded by
//     the visibility clause — owner ⇒ member ⇒ filtered — and is dropped here
//     again as defence in depth, in case ownership and membership ever drift)
//     within a tier: scope-exact over global, exactly as before
//     > 1 row surviving in tier 1 ⇒ ErrAmbiguousReference, never a silent pick
//
// The tiering is applied in Go rather than as a bigger ORDER BY because the query
// must be able to SEE the ambiguity: `LIMIT 1` cannot tell "one candidate" from
// "two candidates and I picked one", and that distinction is the entire safety
// property. Every row this returns is one the old query could also have returned.
func lookupScoped(ctx context.Context, database *sql.DB, table, key, runScope string, runAgencies []string) (id, scope string, found bool, err error) {
	// The membership table for this row type. Compile-time constant, never input.
	memberTable := "secret_agencies"
	memberCol := "secret_id"
	if table == "env_vars" {
		memberTable, memberCol = "env_var_agencies", "env_var_id"
	}
	agencyJSON := marshalNames(runAgencies)
	rows, qerr := database.QueryContext(ctx,
		`SELECT t.id, COALESCE(t.scope,''), COALESCE(ag.name,'') FROM `+table+` t
		 LEFT JOIN agencies ag ON ag.id = t.owner_agency
		 WHERE t.key = ?
		   AND (COALESCE(t.scope,'') = '' OR COALESCE(t.scope,'') = ?)
		   AND (
		     NOT EXISTS (SELECT 1 FROM `+memberTable+` m WHERE m.`+memberCol+` = t.id)
		     OR EXISTS (
		       SELECT 1 FROM `+memberTable+` m
		       JOIN agencies a ON a.id = m.agency_id
		       WHERE m.`+memberCol+` = t.id AND a.name IN (SELECT value FROM json_each(?)))
		   )
		 ORDER BY (COALESCE(t.scope,'') = ?) DESC, t.id`,
		key, runScope, agencyJSON, runScope)
	if qerr != nil {
		return "", "", false, fmt.Errorf("lookup %s %q: %w", table, key, qerr)
	}
	defer rows.Close()
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.scope, &c.owner); err != nil {
			return "", "", false, fmt.Errorf("lookup %s %q: %w", table, key, err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("lookup %s %q: %w", table, key, err)
	}
	win, ambiguous := pickOwned(cands, runAgencies, runScope)
	if ambiguous {
		return "", "", false, ErrAmbiguousReference
	}
	if win == nil {
		return "", "", false, nil
	}
	return win.id, win.scope, true, nil
}

// candidate is one row surviving the visibility predicate: its id, its scope ("" =
// global) and its owning agency's NAME ("" = unowned/shared). The name rather than
// the id because a run's frozen snapshot is a set of agency NAMES — and because
// agencies can be renamed, which is why the COLUMN stores the id and this join
// resolves it fresh.
type candidate struct{ id, scope, owner string }

// pickOwned applies RA-17's tiers to the visible candidates. Returns the winner, or
// ambiguous=true when the winning tier holds more than one row at the same scope
// precedence. A nil winner with ambiguous=false means nothing resolved.
func pickOwned(cands []candidate, runAgencies []string, runScope string) (win *candidate, ambiguous bool) {
	var owned, shared []candidate
	for _, c := range cands {
		switch {
		case c.owner == "":
			shared = append(shared, c)
		case slices.Contains(runAgencies, c.owner):
			owned = append(owned, c)
		default:
			// Owned by a department this run is not in. Unreachable while ownership
			// implies membership (the write path enrolls the owner), but dropping it
			// explicitly means a drifted row can never be injected into a run whose
			// department does not own it — the failure that would matter most.
		}
	}
	tier := owned
	if len(tier) == 0 {
		tier = shared
	}
	if len(tier) == 0 {
		return nil, false
	}
	// Scope-exact beats global WITHIN the tier — the pre-Phase-E rule, unchanged.
	var exact []candidate
	for _, c := range tier {
		if runScope != "" && c.scope == runScope {
			exact = append(exact, c)
		}
	}
	if len(exact) > 0 {
		tier = exact
	}
	if len(tier) > 1 {
		return nil, true
	}
	return &tier[0], false
}

// LookupEntityID resolves a bare Env Vars name to the row DISPATCH would inject,
// under the run's scope and agency snapshot. Exported so the API's per-run attach
// gate authorizes the SAME row the injector will pick.
//
// ⚠️ This exists to kill a duplicated predicate. The attach gate used to carry its
// own hand-copied SQL "mirroring runref.lookupScoped exactly" — which is a promise
// no comment can keep, and which Phase E would have broken silently: the copy had
// no owner tier, so with two departments' rows present the gate would authorize one
// row while dispatch injected another. One function, one answer.
//
// found=false means nothing resolved. An ambiguity is returned as
// ErrAmbiguousReference so the caller can fail closed rather than authorize a row
// dispatch will refuse.
func LookupEntityID(ctx context.Context, database *sql.DB, kind Kind, name, runScope string, runAgencies []string) (id string, found bool, err error) {
	switch kind {
	case KindSecret:
		return lookupScopedID(ctx, database, "secrets", name, runScope, runAgencies)
	case KindVar:
		return lookupScopedID(ctx, database, "env_vars", name, runScope, runAgencies)
	case KindKey:
		return lookupKeyID(ctx, database, name, runAgencies)
	}
	return "", false, nil
}

// marshalNames renders an agency set for the json_each parameter above. Always a
// well-formed array — an empty run set is "[]", which matches only rows with no
// membership, exactly as intended for a general-pool run.
func marshalNames(names []string) string {
	if len(names) == 0 {
		return "[]"
	}
	b, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// ambiguityOrErr wraps RA-17's ambiguity in a ReferenceError so it reaches the
// operator through OperatorMessage's own ambiguity sentence, and passes anything
// else (a real DB fault) through untouched — a query failure must not be dressed up
// as a resolution verdict.
func ambiguityOrErr(b Binding, err error) error {
	if errors.Is(err, ErrAmbiguousReference) {
		return &ReferenceError{Ref: b.Kind.Reference(b.Name),
			err: fmt.Errorf("%w: %s %q is owned by more than one of this run's agencies", ErrAmbiguousReference, b.Kind, b.Name)}
	}
	return err
}

// scopeOrMissing produces the precise fail-closed error when lookupScopedID found
// no in-scope row: ErrOutOfScope if a row with that key exists in some OTHER
// scope, ErrMissingReference if the key exists nowhere. `table` is a compile-time
// constant.
func scopeOrMissing(ctx context.Context, database *sql.DB, table string, b Binding, runScope string) error {
	var n int
	if err := database.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM `+table+` WHERE key = ?`, b.Name).Scan(&n); err != nil {
		// A failed count must not masquerade as "missing" — surface it so dispatch
		// fails on the real cause rather than a misleading not-found.
		return fmt.Errorf("classify %s %q: %w", table, b.Name, err)
	} else if n > 0 {
		// Precise for logs; ReferenceError.OperatorMessage collapses it with the
		// missing case so the operator surface is a non-oracle (M2).
		return &ReferenceError{Ref: b.Kind.Reference(b.Name),
			err: fmt.Errorf("%w: %s %q exists only outside run scope %q", ErrOutOfScope, b.Kind, b.Name, runScope)}
	}
	return &ReferenceError{Ref: b.Kind.Reference(b.Name),
		err: fmt.Errorf("%w: %s %q", ErrMissingReference, b.Kind, b.Name)}
}

// keyInAgencies applies the AG-Q5 clause to an SSH-key label: reachable when the
// credential has NO agency membership (unrestricted — the state migration 670
// leaves every existing key in) or when its membership intersects the run's
// snapshot. A label with no credential row at all returns true so the caller's
// existing not-found path produces the missing-reference error rather than this
// one — the two are different facts and collapse to the same operator message
// anyway.
// lookupKeyID is the KEY half of THE predicate (RA-19): the AG-Q5 membership clause,
// plus RA-17's owned-beats-shared tier and its fail-closed ambiguity, applied to an
// SSH-credential label.
//
// Labels carry no scope dimension, so per-owner uniqueness here is (label, owner) and
// there is no scope tiebreak — which makes the ambiguity slightly EASIER to hit than
// for secrets: two departments in a run's snapshot owning `deploy_key` collide
// outright, with no scope to separate them.
//
// found=false covers both "no such label" and "exists, but belongs to a department
// this run is not in": the two are different facts that must collapse to one operator
// message (M2), and the caller does exactly that.
func lookupKeyID(ctx context.Context, database *sql.DB, label string, runAgencies []string) (id string, found bool, err error) {
	rows, qerr := database.QueryContext(ctx, `
		SELECT c.id, COALESCE(ag.name,'')
		FROM ssh_credentials c
		LEFT JOIN agencies ag ON ag.id = c.owner_agency
		WHERE c.label = ?
		  AND (
		    NOT EXISTS (SELECT 1 FROM ssh_credential_agencies ca WHERE ca.credential_id = c.id)
		    OR EXISTS (
		      SELECT 1 FROM ssh_credential_agencies ca
		      JOIN agencies a ON a.id = ca.agency_id
		      WHERE ca.credential_id = c.id AND a.name IN (SELECT value FROM json_each(?)))
		  )
		ORDER BY c.id`, label, marshalNames(runAgencies))
	if qerr != nil {
		return "", false, fmt.Errorf("resolve key agencies %q: %w", label, qerr)
	}
	defer rows.Close()
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.owner); err != nil {
			return "", false, fmt.Errorf("resolve key agencies %q: %w", label, err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("resolve key agencies %q: %w", label, err)
	}
	// runScope "" — keys have no scope axis, so the scope tiebreak is a no-op here
	// and the tier decides alone.
	win, ambiguous := pickOwned(cands, runAgencies, "")
	if ambiguous {
		return "", false, ErrAmbiguousReference
	}
	if win == nil {
		return "", false, nil
	}
	return win.id, true, nil
}

func keyInAgencies(ctx context.Context, database *sql.DB, label string, runAgencies []string) (bool, error) {
	var members int
	err := database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ssh_credential_agencies ca
		JOIN ssh_credentials c ON c.id = ca.credential_id
		WHERE c.label = ?`, label).Scan(&members)
	if err != nil {
		return false, fmt.Errorf("resolve key agencies %q: %w", label, err)
	}
	if members == 0 {
		return true, nil // unrestricted
	}
	if len(runAgencies) == 0 {
		return false, nil // general-pool run intersects nothing
	}
	var hit int
	err = database.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM ssh_credential_agencies ca
		JOIN ssh_credentials c ON c.id = ca.credential_id
		JOIN agencies a        ON a.id = ca.agency_id
		WHERE c.label = ? AND a.name IN (SELECT value FROM json_each(?))`,
		label, marshalNames(runAgencies)).Scan(&hit)
	if err != nil {
		return false, fmt.Errorf("resolve key agencies %q: %w", label, err)
	}
	return hit > 0, nil
}
