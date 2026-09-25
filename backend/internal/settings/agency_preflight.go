package settings

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// The T2.12 pre-flight report (the agencies plan Phase 2).
//
// Phase 2 and Phase 3 are deliberately NOT merged. Phase 2 populates the
// membership tables but changes no behavior, which is what lets this report be run
// against REAL production data before any predicate is switched. Merging the two
// would mean discovering AG-Q5's tightening in production rather than here.
//
// The report answers one question: **which reference bindings that work today
// would stop working if the Phase-3 predicates were switched on right now?**
//
//   - AG-Q1(b) — a secret/variable binding resolves only if the row's agency set
//     intersects the run's, AND the existing scope rule still holds. The scope
//     half is unchanged in Phase 3, so anything this report flags is flagged by
//     the AGENCY half alone.
//   - AG-Q5 — an SSH key binding gains the same agency clause. Today any job may
//     bind any key, so this is the tightening §7.1 warns about.
//
// The governing convention, in both cases and identical to the migration-670
// backfill: an EMPTY membership set means "no agency restriction", so a row with
// no membership is reachable from everywhere and is never flagged. That is what
// keeps a global secret global, and it is why this report is empty on a freshly
// migrated database — the tightening arrives only as an operator assigns
// membership, one row at a time, with this report to check it against.

// PreflightFinding is one binding that would stop resolving under Phase 3.
type PreflightFinding struct {
	Kind      string `json:"kind"`      // secret | var | key
	Name      string `json:"name"`      // the bare row name
	Reference string `json:"reference"` // the derived CRONOMICON_<SECTION>_<name>
	JobSource string `json:"jobSource"`
	JobName   string `json:"jobName"`
	JobScope  string `json:"jobScope"` // "" = global
	// JobAgencies is the agency set the job's scope belongs to; empty = the general
	// pool, which under Phase 3 intersects nothing — so a job in the general pool
	// cannot reach any row that HAS membership.
	JobAgencies []string `json:"jobAgencies"`
	// RowAgencies is the referenced row's membership. Non-empty by construction: a
	// row with no membership is unrestricted and never appears here.
	RowAgencies []string `json:"rowAgencies"`
	Reason      string   `json:"reason"`
}

// PreflightReport is the whole answer, split by the decision that causes each
// finding so the two can be reviewed — and accepted — independently.
type PreflightReport struct {
	// ReferenceFindings are secret/variable bindings that AG-Q1(b) would break.
	ReferenceFindings []PreflightFinding `json:"referenceFindings"`
	// KeyFindings are SSH-key bindings that AG-Q5's tightening would break.
	KeyFindings []PreflightFinding `json:"keyFindings"`
	// ShadowFindings are scoped rows that shadow a global row of the same key
	// (RA-10). Unlike the two above these are not about a pending tightening —
	// they describe what resolution does TODAY, and the unrestricted ones are the
	// silent hole Phase D exists to make visible. See agency_shadow.go.
	ShadowFindings []ShadowFinding `json:"shadowFindings"`
	// AmbiguityFindings are same-key rows owned by DIFFERENT departments (RA-18).
	// Not a misconfiguration — that is the Phase E model — but a run spanning more
	// than one of the owners fails closed, so it belongs in the report an operator
	// reads before they meet it as a refused run.
	AmbiguityFindings []AmbiguityFinding `json:"ambiguityFindings"`
	// JobBindingsChecked is how many job-owned bindings were evaluated, so an empty
	// report can be told apart from a report over no data.
	JobBindingsChecked int `json:"jobBindingsChecked"`
	// ScriptBindingsUnevaluated counts SCRIPT-owned bindings. A script has no scope
	// of its own — its bindings resolve against whatever scope the run carries — so
	// there is no static answer, and reporting a guess would be worse than saying so.
	ScriptBindingsUnevaluated int `json:"scriptBindingsUnevaluated"`
	// MembershipAssigned is the row count across the four membership tables. Zero
	// means the tightening currently has no teeth at all, which is the expected
	// state immediately after migration 670 — worth stating so an empty findings
	// list is not misread as "Phase 3 is safe".
	MembershipAssigned int `json:"membershipAssigned"`
}

// AgencyPreflight computes the report. It reads every input into memory FIRST and
// resolves in Go — never a query inside an open cursor (the pool deadlock in
// db.maxOpenConns).
func AgencyPreflight(ctx context.Context, database *sql.DB) (*PreflightReport, error) {
	rep := &PreflightReport{
		ReferenceFindings: []PreflightFinding{},
		KeyFindings:       []PreflightFinding{},
		ShadowFindings:    []ShadowFinding{},
		AmbiguityFindings: []AmbiguityFinding{},
	}
	// RA-10 — computed here rather than by a second endpoint call so the operator
	// reviewing membership before a tightening sees, in the same report, the rows
	// whose resolution is ALREADY ambiguous.
	shadows, err := ShadowFindings(ctx, database)
	if err != nil {
		return nil, err
	}
	rep.ShadowFindings = shadows
	ambiguities, err := Ambiguities(ctx, database)
	if err != nil {
		return nil, err
	}
	rep.AmbiguityFindings = ambiguities

	// scope name → agency names.
	scopeAgencies, err := loadStringSets(ctx, database, `
		SELECT s.name, a.name FROM scope_agencies sa
		JOIN scopes s   ON s.id = sa.scope_id
		JOIN agencies a ON a.id = sa.agency_id`)
	if err != nil {
		return nil, err
	}
	// Row membership, keyed by row id.
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
	// Keys are keyed by LABEL, because a binding names the label and ssh_credentials
	// has no scope to disambiguate on.
	keyAgencies, err := loadStringSets(ctx, database, `
		SELECT c.label, a.name FROM ssh_credential_agencies ca
		JOIN ssh_credentials c ON c.id = ca.credential_id
		JOIN agencies a        ON a.id = ca.agency_id`)
	if err != nil {
		return nil, err
	}
	for _, m := range []map[string][]string{scopeAgencies, secretAgencies, varAgencies, keyAgencies} {
		for _, v := range m {
			rep.MembershipAssigned += len(v)
		}
	}

	secrets, err := loadScopedRows(ctx, database, "secrets")
	if err != nil {
		return nil, err
	}
	vars, err := loadScopedRows(ctx, database, "env_vars")
	if err != nil {
		return nil, err
	}

	// Job-owned bindings joined to their owning job's scope. A binding whose job no
	// longer exists is skipped by the JOIN — it cannot break a run that cannot run.
	rows, err := database.QueryContext(ctx, `
		SELECT rb.ref_kind, rb.ref_name, rb.owner_kind, rb.owner_source, rb.owner_name,
		       COALESCE(j.scope, '')
		FROM reference_bindings rb
		LEFT JOIN jobs j
		  ON rb.owner_kind = 'job' AND j.source = rb.owner_source AND j.name = rb.owner_name
		WHERE rb.owner_kind = 'script' OR j.name IS NOT NULL
		ORDER BY rb.owner_name, rb.ref_kind, rb.ref_name`)
	if err != nil {
		return nil, fmt.Errorf("preflight: read bindings: %w", err)
	}
	defer rows.Close()

	type binding struct{ kind, name, ownerKind, ownerSource, ownerName, jobScope string }
	var bindings []binding
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.kind, &b.name, &b.ownerKind, &b.ownerSource, &b.ownerName, &b.jobScope); err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, b := range bindings {
		if b.ownerKind != "job" {
			rep.ScriptBindingsUnevaluated++
			continue
		}
		rep.JobBindingsChecked++
		jobAg := scopeAgencies[b.jobScope]

		var rowAg []string
		switch b.kind {
		case "key":
			rowAg = keyAgencies[b.name]
		case "secret":
			if id, ok := resolveScoped(secrets, b.name, b.jobScope, jobAg); ok {
				rowAg = secretAgencies[id]
			}
		case "var":
			if id, ok := resolveScoped(vars, b.name, b.jobScope, jobAg); ok {
				rowAg = varAgencies[id]
			}
		}
		// No membership ⇒ unrestricted ⇒ nothing changes for this binding. This is
		// the branch that keeps the report honest: it is empty on a freshly migrated
		// database, because the backfill deliberately assigns no membership that
		// would narrow anything.
		if len(rowAg) == 0 || intersects(jobAg, rowAg) {
			continue
		}
		f := PreflightFinding{
			Kind: b.kind, Name: b.name, Reference: derivedReference(b.kind, b.name),
			JobSource: b.ownerSource, JobName: b.ownerName, JobScope: b.jobScope,
			JobAgencies: jobAg, RowAgencies: rowAg,
		}
		if len(jobAg) == 0 {
			f.Reason = fmt.Sprintf("the job's scope %s belongs to no agency, so it intersects nothing; the %s belongs to %v",
				scopeName(b.jobScope), kindNoun(b.kind), rowAg)
		} else {
			f.Reason = fmt.Sprintf("the job's scope %s belongs to %v; the %s belongs to %v — no overlap",
				scopeName(b.jobScope), jobAg, kindNoun(b.kind), rowAg)
		}
		if b.kind == "key" {
			rep.KeyFindings = append(rep.KeyFindings, f)
		} else {
			rep.ReferenceFindings = append(rep.ReferenceFindings, f)
		}
	}
	return rep, nil
}

// scopedRow is one secret/env_vars row reduced to what the scope predicate needs.
type scopedRow struct{ id, key, scope, owner string }

func loadScopedRows(ctx context.Context, database *sql.DB, table string) ([]scopedRow, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT t.id, t.key, COALESCE(t.scope,''), COALESCE(ag.name,'')
		 FROM `+table+` t LEFT JOIN agencies ag ON ag.id = t.owner_agency`)
	if err != nil {
		return nil, fmt.Errorf("preflight: read %s: %w", table, err)
	}
	defer rows.Close()
	var out []scopedRow
	for rows.Next() {
		var r scopedRow
		if err := rows.Scan(&r.id, &r.key, &r.scope, &r.owner); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveScoped mirrors runref.lookupScoped's predicate in Go: the scope-exact row
// wins, else the global one. Reimplemented here rather than imported because runref
// resolves against a live DB per lookup and this report resolves thousands of
// bindings against one in-memory snapshot — but the RULE must stay identical, or
// the report describes a system that does not exist.
func resolveScoped(rows []scopedRow, key, scope string, jobAgencies []string) (string, bool) {
	// RA-17: ownership is the OUTER tier, so an owned candidate is preferred over a
	// shared one before scope is even considered — the same order the resolver
	// applies. Without this the report would resolve a binding to the shared row
	// while dispatch injected the department's, and its findings would describe a
	// system that does not exist (the exact failure this function's comment warns
	// about). Two owners in range is an AMBIGUITY at run time; the report leaves it
	// to Ambiguities() rather than guessing a winner here.
	var owned []scopedRow
	for _, r := range rows {
		if r.key == key && r.owner != "" && containsStr(jobAgencies, r.owner) {
			owned = append(owned, r)
		}
	}
	if len(owned) == 1 {
		return owned[0].id, true
	}
	if len(owned) > 1 {
		return "", false // ambiguous — nothing resolves, and nothing is "broken by" Phase 3
	}
	var globalID string
	var haveGlobal bool
	for _, r := range rows {
		if r.key != key || r.owner != "" {
			continue
		}
		if r.scope == scope && scope != "" {
			return r.id, true // scope-exact wins outright
		}
		if r.scope == "" && !haveGlobal {
			globalID, haveGlobal = r.id, true
		}
	}
	return globalID, haveGlobal
}

// loadStringSets runs a two-column (key, value) query into a key → sorted values map.
func loadStringSets(ctx context.Context, database *sql.DB, query string) (map[string][]string, error) {
	out := map[string][]string{}
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		// Best-effort on a pre-670 schema: the report is advisory and must not 500
		// just because the membership tables are not there yet.
		return out, nil
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = append(out[k], v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

func scopeName(s string) string {
	if s == "" {
		return "(global)"
	}
	return fmt.Sprintf("%q", s)
}

func kindNoun(kind string) string {
	switch kind {
	case "secret":
		return "secret"
	case "var":
		return "variable"
	case "key":
		return "SSH key"
	}
	return "reference"
}

func derivedReference(kind, name string) string {
	switch kind {
	case "secret":
		return envref.SecretReference(name)
	case "var":
		return envref.VarReference(name)
	case "key":
		return envref.KeyReference(name)
	}
	return name
}

// containsStr is a tiny local membership test — this package predates the slices
// import and the report has no other need for it.
func containsStr(hay []string, want string) bool {
	return slices.Contains(hay, want)
}
