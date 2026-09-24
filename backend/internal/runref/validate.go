package runref

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Authoring-time reference validation (agencies plan T1.1/T1.2, AG-Q4).
//
// Until now the ONLY feedback on a bad binding was a 409 at dispatch, and that
// message is deliberately vague: OperatorMessage collapses out-of-scope and
// missing into one sentence so a run cannot be used as a cross-scope existence
// oracle (M2). That rationale is about RUNS — "letting an actor probe any name one
// run at a time". It was never weighed for the AUTHORING surface, where the actor
// is looking at a job editor and can already LIST the rows in question.
//
// So this file is precise where dispatch is vague, bounded by exactly one rule:
//
//	the validator may reveal NOTHING the caller's own Env Vars / Secrets LIST
//	would not already show them.
//
// Both lists are scope-filtered per actor (auth.ScopeReadable on GET /env-vars;
// secrets.Service.List(grant.CanRead) on GET /env-secrets), so the caller supplies
// that same filter as `canRead` and precision degrades ROW BY ROW: a row the actor
// could not see in the list is never named here, and its existence collapses back
// into the generic not-found verdict. Do NOT "fix" this into unconditional
// precision — that is the back-door oracle M2 exists to prevent. Equally, do not
// "fix" it back into unconditional vagueness: the whole point of the authoring
// surface is that the cross-scope case is the one an operator cannot diagnose.
//
// The visibility predicate itself is lookupScoped — the SAME query dispatch runs
// (resolve.go) — so a ✓ here and a successful injection cannot disagree.

// Outcome classifies an authoring-time verdict for one declared reference.
type Outcome string

const (
	// OutcomeResolved: the reference resolves from the given scope — dispatch will
	// inject it (subject to the row still existing and being revealable).
	OutcomeResolved Outcome = "resolved"
	// OutcomeOutOfScope: a row with this name exists, is visible to the CALLER, but
	// not from the run's scope. The distinguishing fact the dispatch 409 withholds.
	OutcomeOutOfScope Outcome = "out_of_scope"
	// OutcomeNotFound: no row this caller can see carries this name — either it
	// exists nowhere, or it exists only in scopes the caller may not read (the
	// degraded case; the two are deliberately indistinguishable here, exactly as
	// they are in the caller's own list).
	OutcomeNotFound Outcome = "not_found"
	// OutcomeInvalid: the name is not a legal Env Vars row name, so no row could
	// ever carry it. A binding write would be refused with 422.
	OutcomeInvalid Outcome = "invalid"
)

// Validation is the verdict for a single declared reference, shaped for the
// authoring surfaces (job detail, script detail, Run-dialog preflight). It carries
// NO value, ever — only names, scopes and a reason.
type Validation struct {
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	// As echoes the alias the caller asked about (RA-1), so a dialog rendering a
	// set of chips can match verdicts back to bindings when one row is bound twice
	// under two destinations. It does NOT participate in resolution — the alias is
	// a destination, never a selector (§2.2) — but a MALFORMED alias is reported
	// here as OutcomeInvalid, because a binding write would refuse it with 422 and
	// discovering that only on save is the shape this validator exists to avoid.
	As string `json:"as,omitempty"`
	// InjectReference is the derived key the value would actually land on: the
	// alias's CRONOMICON_<SECTION>_<as> when aliased, otherwise Reference.
	InjectReference string  `json:"injectReference"`
	Reference       string  `json:"reference"`
	OK              bool    `json:"ok"`
	Outcome         Outcome `json:"outcome"`
	// Reason is an operator-facing sentence. It is precise only to the extent the
	// caller's own list already is (see the file comment).
	Reason string `json:"reason"`
	// ResolvedScope is the WINNING row's scope when OK: "" means the global row.
	// nil when nothing resolved, and always nil for keys (ssh_credentials carries
	// no scope column — the accepted §8 limitation, closed by AG-Q5 in Phase 3).
	ResolvedScope *string `json:"resolvedScope"`
	// OtherScopes lists the scopes where a CALLER-VISIBLE row of this name does
	// exist, when the reference did not resolve. Empty unless OutcomeOutOfScope.
	OtherScopes []string `json:"otherScopes,omitempty"`
}

// CanReadScope reports whether the calling actor may READ a row in `scope` ("" =
// global). Callers pass auth.Identity.Grant().CanRead — runref must not import
// auth (it is a leaf the executor seams both depend on), and a plain predicate
// keeps the scope model's single source of truth in the auth package.
type CanReadScope func(scope string) bool

// refTable maps a reference kind to the table and name column its rows live in.
// Keys are the odd one out: ssh_credentials is keyed by `label` and has no scope.
func refTable(k Kind) (table, nameCol string, scoped bool, ok bool) {
	switch k {
	case KindSecret:
		return "secrets", "key", true, true
	case KindVar:
		return "env_vars", "key", true, true
	case KindKey:
		return "ssh_credentials", "label", false, true
	}
	return "", "", false, false
}

// Validate answers, at AUTHORING time, the question dispatch answers vaguely:
// would this binding resolve for a run in runScope? canRead is the caller's own
// row-visibility filter (see CanReadScope); a nil canRead is treated as
// "GLOBAL only", the fail-closed reading of a zero grant.
func Validate(ctx context.Context, database *sql.DB, kind Kind, name, runScope string, canRead CanReadScope) (Validation, error) {
	v := Validation{Kind: kind, Name: name, Reference: kind.Reference(name), InjectReference: kind.Reference(name)}
	if canRead == nil {
		canRead = func(scope string) bool { return scope == "" }
	}
	if !ValidKind(kind) {
		v.Outcome = OutcomeInvalid
		v.Reason = fmt.Sprintf("%q is not a reference kind (expected secret, var or key)", string(kind))
		return v, nil
	}
	if name == "" {
		v.Outcome = OutcomeInvalid
		v.Reason = "reference name is empty"
		return v, nil
	}
	table, nameCol, scoped, _ := refTable(kind)

	// Keys have no scope axis at all (resolve.go's resolveKey documents why), so
	// the verdict is pure existence against the SAME predicate dispatch uses —
	// sshkeys.ResolveMaterialByName's `SELECT id FROM ssh_credentials WHERE label = ?`.
	// Existence only: validation must never decrypt material.
	if !scoped {
		found, err := rowExists(ctx, database, table, nameCol, name)
		if err != nil {
			return v, err
		}
		if !found {
			v.Outcome = OutcomeNotFound
			// G-2 (VF-6): kindNoun, not a retyped noun. This read "no SSH key
			// credential named %q exists" — `credential` is the ssh_credentials
			// table's name leaking into a sentence the operator reads verbatim in
			// the References editor and the Run dialog's preflight, where the UI's
			// word for the thing is SSH Key and nothing else.
			v.Reason = fmt.Sprintf("no %s named %q exists", kindNoun(kind), name)
			return v, nil
		}
		// AG-Q5 (Phase 3): keys now carry agency membership. A key with NO membership
		// stays reachable from everywhere — that is the state migration 670 leaves
		// every existing key in, and it is what keeps the tightening from being a
		// cliff on upgrade.
		runAgencies, err := scopeAgencyNames(ctx, database, runScope)
		if err != nil {
			return v, err
		}
		inAgency, err := keyInAgencies(ctx, database, name, runAgencies)
		if err != nil {
			return v, err
		}
		if !inAgency {
			owners, oerr := keyAgencyNames(ctx, database, name)
			if oerr != nil {
				return v, oerr
			}
			v.Outcome, v.OtherScopes = OutcomeOutOfScope, owners
			v.Reason = fmt.Sprintf("the key belongs to %s; this run's scope belongs to %s",
				joinAgencies(owners), joinAgencies(runAgencies))
			return v, nil
		}
		v.OK, v.Outcome = true, OutcomeResolved
		v.Reason = "resolves — SSH keys are not scope-filtered, only agency-filtered"
		return v, nil
	}

	runAgencies, err := scopeAgencyNames(ctx, database, runScope)
	if err != nil {
		return v, err
	}
	_, rowScope, found, err := lookupScoped(ctx, database, table, name, runScope, runAgencies)
	if err != nil {
		return v, err
	}
	if found {
		// A row can resolve and still be one the caller may not read (a global row
		// is readable by everyone, so in practice this only bites a restricted actor
		// previewing another scope's run). Report the ✓ — dispatch WILL inject it —
		// but do not name a scope the caller's list would hide.
		v.OK, v.Outcome = true, OutcomeResolved
		if canRead(rowScope) {
			s := rowScope
			v.ResolvedScope = &s
		}
		v.Reason = resolvedReason(kind, rowScope, runScope, v.ResolvedScope != nil)
		return v, nil
	}

	// Nothing in scope. Classify — but only over rows the CALLER can already see.
	others, err := visibleScopes(ctx, database, table, nameCol, name, canRead)
	if err != nil {
		return v, err
	}
	if len(others) > 0 {
		v.Outcome, v.OtherScopes = OutcomeOutOfScope, others
		v.Reason = fmt.Sprintf("exists, but only in %s — not visible from %s",
			joinScopes(others), scopeLabel(runScope))
		return v, nil
	}
	v.Outcome = OutcomeNotFound
	v.Reason = fmt.Sprintf("no %s named %q is visible from %s", kindNoun(kind), name, scopeLabel(runScope))
	return v, nil
}

// ValidateAll validates a whole binding set against one scope, preserving order.
// The authoring surfaces resolve every chip at once (and the Run dialog re-resolves
// the set whenever its scope override changes), so this is the shape callers want.
//
// RA-1: the alias rides through unchanged — it never affects WHICH row resolves —
// but a malformed one is reported as OutcomeInvalid here rather than surfacing as
// a 422 on save, and the collision check (RA-Q2) runs across the whole set so the
// dialog shows the clash before the trigger refuses it.
func ValidateAll(ctx context.Context, database *sql.DB, bindings []Binding, runScope string, canRead CanReadScope) ([]Validation, error) {
	out := make([]Validation, 0, len(bindings))
	for _, b := range bindings {
		v, err := Validate(ctx, database, b.Kind, b.Name, runScope, canRead)
		if err != nil {
			return nil, err
		}
		v.As = b.As
		if b.As != "" {
			if aerr := ValidateName(b.Kind, b.As); aerr != nil {
				v.OK, v.Outcome = false, OutcomeInvalid
				v.Reason = "invalid alias: " + aerr.Error()
			} else {
				v.InjectReference = b.InjectReference()
			}
		}
		out = append(out, v)
	}
	// A collision downgrades BOTH participants: neither is individually wrong, and
	// naming only the second would read as "the last one you added is the problem"
	// when the operator's fix may well be to re-alias the first.
	if cerr := CheckAliasCollisions(dedupeBindings(bindings)); cerr != nil {
		clashed := collisionKeys(bindings)
		for i := range out {
			key := string(out[i].Kind) + "\x00" + Binding{Kind: out[i].Kind, Name: out[i].Name, As: out[i].As}.InjectName()
			if clashed[key] {
				out[i].OK, out[i].Outcome = false, OutcomeInvalid
				out[i].Reason = cerr.Error()
			}
		}
	}
	return out, nil
}

// dedupeBindings collapses exact kind+name+alias repeats, the same collapse the
// store and both executor seams apply before resolving. Without it a set that
// merely repeats one binding would look like a collision with itself.
func dedupeBindings(bindings []Binding) []Binding {
	seen := make(map[string]bool, len(bindings))
	out := make([]Binding, 0, len(bindings))
	for _, b := range bindings {
		k := DedupeKey(b)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, b)
	}
	return out
}

// collisionKeys returns the injected keys that more than one DISTINCT row targets.
func collisionKeys(bindings []Binding) map[string]bool {
	rows := map[string]map[string]bool{}
	for _, b := range bindings {
		key := string(b.Kind) + "\x00" + b.InjectName()
		if rows[key] == nil {
			rows[key] = map[string]bool{}
		}
		rows[key][b.Name] = true
	}
	out := map[string]bool{}
	for key, names := range rows {
		if len(names) > 1 {
			out[key] = true
		}
	}
	return out
}

// rowExists reports whether `table` holds a row whose name column equals name.
// table/nameCol are compile-time constants from refTable, never user input.
func rowExists(ctx context.Context, database *sql.DB, table, nameCol, name string) (bool, error) {
	var one int
	err := database.QueryRowContext(ctx,
		`SELECT 1 FROM `+table+` WHERE `+nameCol+` = ? LIMIT 1`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lookup %s %q: %w", table, name, err)
	}
	return true, nil
}

// visibleScopes returns the scopes holding a row of this name that the CALLER may
// read, sorted, excluding the global scope (a global row is visible from every
// scope, so it would have resolved). This is the filtered classification that
// replaces scopeOrMissing's unfiltered COUNT(*) on the authoring surface.
func visibleScopes(ctx context.Context, database *sql.DB, table, nameCol, name string, canRead CanReadScope) ([]string, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT DISTINCT COALESCE(scope,'') FROM `+table+`
		 WHERE `+nameCol+` = ? AND COALESCE(scope,'') <> ''
		 ORDER BY 1`, name)
	if err != nil {
		return nil, fmt.Errorf("classify %s %q: %w", table, name, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("classify %s %q: %w", table, name, err)
		}
		if canRead(s) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

func kindNoun(k Kind) string {
	switch k {
	case KindSecret:
		return "secret"
	case KindVar:
		return "variable"
	case KindKey:
		return "SSH key"
	}
	return "reference"
}

// scopeLabel renders the "" global convention as a word, so a reason never reads
// "not visible from ”" (T1.8 makes the same substitution in the Env Vars list).
func scopeLabel(scope string) string {
	if scope == "" {
		return "the global scope"
	}
	return fmt.Sprintf("scope %q", scope)
}

func joinScopes(scopes []string) string {
	switch len(scopes) {
	case 0:
		return "another scope"
	case 1:
		return fmt.Sprintf("scope %q", scopes[0])
	}
	s := ""
	for i, sc := range scopes {
		switch {
		case i == 0:
			s = fmt.Sprintf("%q", sc)
		case i == len(scopes)-1:
			s += fmt.Sprintf(" and %q", sc)
		default:
			s += fmt.Sprintf(", %q", sc)
		}
	}
	return "scopes " + s
}

// resolvedReason names WHICH row won — the scope-exact row or the global fallback
// — because "it resolves" is only half the answer an operator editing scopes needs.
func resolvedReason(kind Kind, rowScope, runScope string, nameScope bool) string {
	if !nameScope {
		return "resolves for this run"
	}
	if rowScope != "" {
		return fmt.Sprintf("resolves to the %s in scope %q", kindNoun(kind), rowScope)
	}
	if runScope == "" {
		return fmt.Sprintf("resolves to the global %s", kindNoun(kind))
	}
	return fmt.Sprintf("resolves to the global %s (no row specific to scope %q)", kindNoun(kind), runScope)
}

// scopeAgencyNames resolves a scope name to the agency NAMES it belongs to
// (migration 670). This is the run-side half of the AG-Q1(b)/AG-Q5 predicate at
// AUTHORING time: dispatch reads the run's frozen snapshot (AG-Q8), but no run
// exists yet here, so the live membership of the scope being previewed is the only
// available — and the correct — answer. A scope with no membership yields an empty
// set, which matches only rows that are themselves unrestricted.
func scopeAgencyNames(ctx context.Context, database *sql.DB, scope string) ([]string, error) {
	out := []string{}
	if scope == "" {
		return out, nil
	}
	rows, err := database.QueryContext(ctx, `
		SELECT a.name FROM scope_agencies sa
		JOIN scopes   s ON s.id = sa.scope_id
		JOIN agencies a ON a.id = sa.agency_id
		WHERE s.name = ?
		ORDER BY a.name`, scope)
	if err != nil {
		return out, nil // best-effort on a pre-670 schema
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// keyAgencyNames lists the agencies an SSH-key label belongs to, for the
// validator's precise out-of-agency reason.
func keyAgencyNames(ctx context.Context, database *sql.DB, label string) ([]string, error) {
	out := []string{}
	rows, err := database.QueryContext(ctx, `
		SELECT a.name FROM ssh_credential_agencies ca
		JOIN ssh_credentials c ON c.id = ca.credential_id
		JOIN agencies a        ON a.id = ca.agency_id
		WHERE c.label = ?
		ORDER BY a.name`, label)
	if err != nil {
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return out, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// joinAgencies renders an agency set for an operator-facing reason, saying "no
// agency" rather than printing an empty list at someone.
func joinAgencies(names []string) string {
	if len(names) == 0 {
		return "no agency"
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	if len(quoted) == 1 {
		return "agency " + quoted[0]
	}
	return "agencies " + strings.Join(quoted, ", ")
}
