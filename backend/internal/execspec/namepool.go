package execspec

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Per-agency name uniqueness (R2-5, the rbac2 plan).
//
// The invariant: within a source pool, a definition's name must be unique
// within EACH agency its scope maps to, and an All-pool definition (empty
// scope) must be unique against EVERYTHING — the All pool overlaps every
// agency. That is a SET-OVERLAP rule, which no SQLite UNIQUE constraint can
// express, so it lives here and is called by every write path that can create
// or re-scope a definition: compose create, compose edit (scope change),
// restore from the recycle bin, and sync.
//
// Schedules are NOT checked here: they have no scope, keep full per-source
// uniqueness in the schema (R2-Q1), and their conflict is the plain 409 the
// compose path has always issued.

// NamePoolConflict reports whether a definition named `name` with scope
// `scope` would collide with an EXISTING definition in the same table and
// source pool, excluding the row identified by excludeUID (pass "" on create).
//
// Two definitions collide when their names match AND their agency sets
// overlap — where the empty scope (the All pool) overlaps everything, and a
// scope that maps to NO agency is treated as All-pool for collision purposes:
// it is visible only to unrestricted admins, and fail-closed here means
// refusing a duplicate rather than allowing one that would collide the day
// the mapping is fixed.
//
// The table name is interpolated and MUST be one of the two callers' constants
// ("jobs" / "workflows") — never caller input.
func NamePoolConflict(ctx context.Context, db *sql.DB, table, source, name, scope, excludeUID string) (bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(uid,''), COALESCE(scope,'') FROM `+table+`
		  WHERE source = ? AND name = ?`, source, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	type cand struct{ uid, scope string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.uid, &c.scope); err != nil {
			return false, err
		}
		if c.uid != excludeUID {
			cands = append(cands, c)
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(cands) == 0 {
		return false, nil
	}

	// Any same-named row at all conflicts with an All-pool candidate, and vice
	// versa — no agency resolution needed for that half.
	if strings.TrimSpace(scope) == "" {
		return true, nil
	}
	mine, err := ScopeAgencies(ctx, db, scope)
	if err != nil {
		return false, err
	}
	if len(mine) == 0 {
		return true, nil // agency-less scope ⇒ All-pool for collision purposes
	}
	mineSet := map[string]bool{}
	for _, a := range mine {
		mineSet[a] = true
	}
	for _, c := range cands {
		if strings.TrimSpace(c.scope) == "" {
			return true, nil // an existing All-pool row overlaps everything
		}
		theirs, err := ScopeAgencies(ctx, db, c.scope)
		if err != nil {
			return false, err
		}
		if len(theirs) == 0 {
			return true, nil
		}
		for _, a := range theirs {
			if mineSet[a] {
				return true, nil
			}
		}
	}
	return false, nil
}

// NamePoolRefusal is the one wording every refusing surface uses. GENERIC on
// purpose — the AF oracle-hygiene rule: it never names the owning agency and
// never points at the recycle bin for a conflict the caller cannot see, so a
// name-taken answer cannot be used to enumerate another department's catalog.
const NamePoolRefusal = "name already in use"

// WorkflowNamePoolConflict is NamePoolConflict for workflows, whose agency set
// derives from their STEPS' jobs rather than from a scope of their own. The
// step scopes are resolved by the caller (which already walks the graph for
// authorization) and passed here.
//
// An empty stepScopes means the workflow touches only All-pool jobs — or none
// at all — and is therefore All-pool itself: it collides with every same-named
// workflow, and they with it.
func WorkflowNamePoolConflict(ctx context.Context, db *sql.DB, source, name string, stepScopes []string, excludeUID string) (bool, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COALESCE(uid,''), steps FROM workflows WHERE source = ? AND name = ?`, source, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	type cand struct{ uid, steps string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.uid, &c.steps); err != nil {
			return false, err
		}
		if c.uid != excludeUID {
			cands = append(cands, c)
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(cands) == 0 {
		return false, nil
	}

	mine, allPool, err := agenciesForScopes(ctx, db, stepScopes)
	if err != nil {
		return false, err
	}
	if allPool {
		return true, nil
	}
	for _, c := range cands {
		theirScopes, err := workflowStepScopes(ctx, db, c.steps)
		if err != nil {
			return false, err
		}
		theirs, theirAll, err := agenciesForScopes(ctx, db, theirScopes)
		if err != nil {
			return false, err
		}
		if theirAll {
			return true, nil
		}
		for a := range theirs {
			if mine[a] {
				return true, nil
			}
		}
	}
	return false, nil
}

// agenciesForScopes folds a scope list into an agency set; allPool is true when
// any scope is empty/agency-less (or the list itself is empty).
func agenciesForScopes(ctx context.Context, db *sql.DB, scopes []string) (map[string]bool, bool, error) {
	if len(scopes) == 0 {
		return nil, true, nil
	}
	out := map[string]bool{}
	for _, sc := range scopes {
		if strings.TrimSpace(sc) == "" {
			return nil, true, nil
		}
		ags, err := ScopeAgencies(ctx, db, sc)
		if err != nil {
			return nil, false, err
		}
		if len(ags) == 0 {
			return nil, true, nil
		}
		for _, a := range ags {
			out[a] = true
		}
	}
	return out, false, nil
}

// workflowStepScopes resolves the scopes of the jobs an EXISTING workflow's
// stored step graph references. A minimal JSON walk rather than
// workflow.FlattenSteps because the import runs the other way (workflow
// depends on execspec); it must visit every node shape the engine's walk does
// — job nodes, parallel arms, sequences, branches — which collecting every
// object bearing type=="job" achieves without knowing the schema.
func workflowStepScopes(ctx context.Context, db *sql.DB, stepsJSON string) ([]string, error) {
	var root any
	if err := json.Unmarshal([]byte(stepsJSON), &root); err != nil {
		return nil, nil // an unparsable graph has no scopes; treated as All-pool
	}
	type ref struct{ name, source string }
	var refs []ref
	var walk func(v any)
	walk = func(v any) {
		switch n := v.(type) {
		case []any:
			for _, e := range n {
				walk(e)
			}
		case map[string]any:
			if t, _ := n["type"].(string); t == "job" {
				name, _ := n["name"].(string)
				src, _ := n["jobSource"].(string)
				if name != "" {
					refs = append(refs, ref{name, src})
				}
			}
			for _, e := range n {
				walk(e)
			}
		}
	}
	walk(root)
	var scopes []string
	for _, r := range refs {
		order := []string{"cronomicon", "git"}
		if r.source != "" {
			order = []string{r.source}
		}
		found := false
		for _, src := range order {
			var scope sql.NullString
			err := db.QueryRowContext(ctx,
				`SELECT scope FROM jobs WHERE name = ? AND source = ? AND deleted_at IS NULL LIMIT 1`,
				r.name, src).Scan(&scope)
			if err == nil {
				scopes = append(scopes, scope.String)
				found = true
				break
			}
		}
		if !found {
			// A dangling step: no scope to contribute. The empty string would
			// force All-pool; a missing job simply doesn't narrow the set.
			continue
		}
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	return scopes, nil
}
