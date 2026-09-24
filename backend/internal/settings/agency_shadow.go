package settings

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// Shadow detection (RA-10, the runas-update plan Phase D).
//
// THE FAILURE MODE. Reference resolution prefers a scope-exact row over a global
// one of the same key — `ORDER BY (COALESCE(t.scope,'') = ?) DESC` in
// runref.lookupScoped. That is deliberate and useful: it is how a scope overrides
// shared configuration. But combined with the AG-Q1(b) convention that an EMPTY
// membership set means "no agency restriction", it produces a silent hole:
//
//	X  (global)  membership: none    → shared infrastructure, reachable by all
//	X  (prod)    membership: NONE    → every department's prod run now resolves
//	                                    THIS row instead of the global one
//
// Nothing errors. Nothing warns. Nothing in the run log distinguishes which row
// was injected. And unmembered is the state migration 670 leaves every row in, so
// in the field this is not an exotic misconfiguration — it is the default outcome
// of adding a scoped row and not thinking about departments.
//
// Aliasing (Phase A) makes departments far likelier to create same-purpose rows in
// shared scopes, and Phase E will make same-NAMED rows a supported pattern, so this
// goes from latent to probable. Detection is cheap; the point is that today nothing
// tells anyone.
//
// ⚠️ This reports; it does not enforce. A shadow is often exactly what an operator
// meant — a prod-specific override of a global default is the feature working. What
// makes it worth surfacing is that the UNMEMBERED case cannot be told apart from
// the intentional one by looking at the list, which is precisely when a warning
// earns its place.

// ShadowFinding is one scoped row that shadows a global row of the same key.
type ShadowFinding struct {
	Kind      string `json:"kind"`      // secret | var
	Name      string `json:"name"`      // the bare row name both rows share
	Reference string `json:"reference"` // the derived AMADEUS_<SECTION>_<name>
	Scope     string `json:"scope"`     // the SHADOWING row's scope (never "")
	// Agencies is the shadowing row's membership; empty means no restriction, which
	// is the case this exists to surface.
	Agencies []string `json:"agencies"`
	// GlobalAgencies is the shadowed global row's membership, for contrast: a global
	// row that is itself departmental narrows who ever saw it in the first place.
	GlobalAgencies []string `json:"globalAgencies"`
	// Unrestricted is true when the shadowing row has NO membership. This is the
	// severe case — the scoped row wins for every department's runs in that scope,
	// with no department restriction of its own.
	Unrestricted bool   `json:"unrestricted"`
	Reason       string `json:"reason"`
}

// AmbiguityFinding is a set of same-key rows in one scope owned by DIFFERENT
// departments (RA-18). It is not a misconfiguration — it is the Phase E model
// working — but a run whose agency snapshot spans more than one of these owners
// resolves to nothing at all (RA-17 fails closed), so it must be visible at
// AUTHORING time rather than first met as a refused run.
//
// The likeliest victim is an UNRESTRICTED admin's ad-hoc run: restricted users
// rarely carry multi-department snapshots, so the people who hit this are exactly
// the people who will file it as a bug.
type AmbiguityFinding struct {
	Kind      string `json:"kind"`      // secret | var | key
	Name      string `json:"name"`      // the shared key/label
	Reference string `json:"reference"` // the derived AMADEUS_<SECTION>_<name>
	Scope     string `json:"scope"`     // "" for keys, which carry no scope
	// Owners are the departments holding a row of this name here, sorted. Always
	// two or more — one owner is not an ambiguity.
	Owners []string `json:"owners"`
	Reason string   `json:"reason"`
}

// ShadowFindings lists every scoped Secrets/Variables row that shadows a global row
// of the same key, sorted (kind, name, scope) so the report is stable and diffable.
//
// Kinds are handled INDEPENDENTLY and never compared against each other: a secret
// and a variable of the same name resolve through different tables under different
// derived prefixes, so neither can shadow the other. (The A13 cross-kind check
// exists to stop them colliding within one scope; that is a different concern.)
//
// SSH keys are absent by construction — ssh_credentials has no scope column, so
// there is no scope-exact-beats-global tier for a key label to be caught in.
func ShadowFindings(ctx context.Context, database *sql.DB) ([]ShadowFinding, error) {
	secretAgencies, err := loadStringSets(ctx, database, `
		SELECT sa.secret_id, a.name FROM secret_agencies sa JOIN agencies a ON a.id = sa.agency_id`)
	if err != nil {
		return nil, err
	}
	varAgencies, err := loadStringSets(ctx, database, `
		SELECT va.env_var_id, a.name FROM env_var_agencies va JOIN agencies a ON a.id = va.agency_id`)
	if err != nil {
		return nil, err
	}

	out := []ShadowFinding{}
	for _, spec := range []struct {
		kind      string
		table     string
		agencyMap map[string][]string
	}{
		{"secret", "secrets", secretAgencies},
		{"var", "env_vars", varAgencies},
	} {
		rows, err := loadScopedRows(ctx, database, spec.table)
		if err != nil {
			return nil, err
		}
		// Index the global rows once; a key with no global row cannot be shadowing
		// anything, which is the overwhelmingly common case and the cheap exit.
		globalByKey := map[string]scopedRow{}
		for _, r := range rows {
			if r.scope == "" {
				globalByKey[r.key] = r
			}
		}
		for _, r := range rows {
			if r.scope == "" {
				continue
			}
			g, shadows := globalByKey[r.key]
			if !shadows {
				continue
			}
			// Both agency lists serialize as [] rather than null — a client rendering
			// "belongs to no department" should not have to special-case a nil, and
			// "no membership" is the single most important value here.
			f := ShadowFinding{
				Kind: spec.kind, Name: r.key, Reference: derivedReference(spec.kind, r.key),
				Scope:          r.scope,
				Agencies:       orEmpty(spec.agencyMap[r.id]),
				GlobalAgencies: orEmpty(spec.agencyMap[g.id]),
			}
			f.Unrestricted = len(f.Agencies) == 0
			if f.Unrestricted {
				f.Reason = fmt.Sprintf(
					"the %s in scope %q belongs to no department, so it shadows the global %s "+
						"for EVERY department's runs in that scope — assign it to a department, "+
						"or delete it if the global row was meant to win",
					kindNoun(spec.kind), r.scope, kindNoun(spec.kind))
			} else {
				f.Reason = fmt.Sprintf(
					"the %s in scope %q (%v) shadows the global %s for runs in that scope",
					kindNoun(spec.kind), r.scope, f.Agencies, kindNoun(spec.kind))
			}
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}

// orEmpty normalizes a nil slice to an empty one so it marshals as [] rather than
// null. "Belongs to no department" is the single most consequential value in a
// finding; a client should read it as an empty list, not a missing field.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// Ambiguities lists every (kind, name, scope) held by MORE THAN ONE department
// (RA-18). Sorted for a stable, diffable report.
//
// Note what is NOT reported: a department's row sitting alongside a SHARED row of
// the same name. That resolves cleanly — owned beats shared (RA-17) — and flagging
// it would bury the real ambiguity in noise. Only owner-vs-owner collides.
//
// Keys are included with an empty scope: ssh_credentials has no scope column, so two
// departments owning `deploy_key` collide outright with nothing to separate them,
// which makes the key case the EASIEST of the three to hit.
func Ambiguities(ctx context.Context, database *sql.DB) ([]AmbiguityFinding, error) {
	out := []AmbiguityFinding{}
	type spec struct {
		kind, query string
	}
	for _, sp := range []spec{
		{"secret", `SELECT s.key, COALESCE(s.scope,''), ag.name FROM secrets s
		            JOIN agencies ag ON ag.id = s.owner_agency WHERE COALESCE(s.owner_agency,'') <> ''`},
		{"var", `SELECT e.key, COALESCE(e.scope,''), ag.name FROM env_vars e
		         JOIN agencies ag ON ag.id = e.owner_agency WHERE COALESCE(e.owner_agency,'') <> ''`},
		{"key", `SELECT c.label, '', ag.name FROM ssh_credentials c
		         JOIN agencies ag ON ag.id = c.owner_agency WHERE COALESCE(c.owner_agency,'') <> ''`},
	} {
		rows, err := database.QueryContext(ctx, sp.query)
		if err != nil {
			// Best-effort on a pre-830 schema: the report is advisory and must not 500
			// because the column is not there yet.
			continue
		}
		owners := map[[2]string][]string{}
		for rows.Next() {
			var name, scope, owner string
			if err := rows.Scan(&name, &scope, &owner); err != nil {
				rows.Close()
				return nil, err
			}
			k := [2]string{name, scope}
			owners[k] = append(owners[k], owner)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		for k, os := range owners {
			if len(os) < 2 {
				continue
			}
			sort.Strings(os)
			f := AmbiguityFinding{
				Kind: sp.kind, Name: k[0], Reference: derivedReference(sp.kind, k[0]),
				Scope: k[1], Owners: os,
			}
			where := "scope " + scopeName(k[1])
			if sp.kind == "key" {
				where = "the key catalogue" // labels carry no scope
			}
			f.Reason = fmt.Sprintf(
				"%v each own a %s named %q in %s; a run whose departments span more than one "+
					"of them resolves to nothing (it fails closed rather than pick one) — "+
					"attach the row explicitly as a per-run reference, or bind it under an alias",
				os, kindNoun(sp.kind), k[0], where)
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Scope < out[j].Scope
	})
	return out, nil
}
