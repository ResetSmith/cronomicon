// Package sortparam resolves the ?sort=&order= query parameters of the paged
// list endpoints into a SQL ORDER BY clause (TS-20, the sorting-update plan).
//
// Every endpoint declares an allowlist mapping its wire-level sort keys to SQL
// expressions; the requested key is looked up, NEVER interpolated — an unknown
// key or order value is an error the handler maps to 400. Callers keep their
// existing default clause for requests that send no sort key, so adding the
// parameter changes nothing for existing clients.
package sortparam

import (
	"fmt"
	"net/url"
)

// OrderBy builds " ORDER BY <expr> <dir>, <secondary>" from ?sort=&order=.
//
//   - q: the request query values (reads "sort" and "order").
//   - allow: wire key → SQL expression (a column or a CASE rank expression).
//   - def: the endpoint's pre-existing default clause, returned verbatim when
//     no sort key is present (e.g. " ORDER BY created_at DESC").
//   - secondary: deterministic tiebreak appended after the requested column
//     (e.g. "created_at DESC, id DESC") so LIMIT/OFFSET pages don't shear when
//     the sorted column has equal values. Empty to skip.
//
// The requested expression is wrapped in an IS-NULL guard so empty cells sort
// last in BOTH directions — matching the client-side comparator the
// non-paged tables use (utils/sort.ts).
func OrderBy(q url.Values, allow map[string]string, def, secondary string) (string, error) {
	key := q.Get("sort")
	if key == "" {
		return def, nil
	}
	expr, ok := allow[key]
	if !ok {
		return "", fmt.Errorf("unknown sort key %q", key)
	}
	dir := "ASC"
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		dir = "DESC"
	default:
		return "", fmt.Errorf("order must be \"asc\" or \"desc\"")
	}
	clause := " ORDER BY (" + expr + ") IS NULL, " + expr + " " + dir
	if secondary != "" {
		clause += ", " + secondary
	}
	return clause, nil
}

// RunStatusRank is the SQL twin of the frontend's worst-first status rank
// (TS-Q5): ascending floats failures. Shared by /runs and /workflow-runs.
const RunStatusRank = `CASE status
	WHEN 'failure' THEN 0
	WHEN 'warning' THEN 1
	WHEN 'killed' THEN 2
	WHEN 'running' THEN 3
	WHEN 'queued' THEN 4
	WHEN 'success' THEN 5
	WHEN 'skipped' THEN 6
	ELSE 7 END`
