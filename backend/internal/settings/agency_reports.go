package settings

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"sort"

	"github.com/ResetSmith/cronomicon/internal/envref"
)

// Helpers shared by the agency reports (the shadow report and the RBAC
// preflight). They lived in agency_preflight.go beside AgencyPreflight, the
// report of "what would break if agency isolation were switched on". That switch
// has been on since Phase 3 of the agencies plan, the report had no caller in the
// console, and its central case — a row with no membership — cannot exist since
// Global became an agency (migration 1220). The report is gone; these stay.

// scopedRow is one secret/env_vars row reduced to what the scope predicate needs.
type scopedRow struct{ id, key, scope, owner string }

func loadScopedRows(ctx context.Context, database *sql.DB, table string) ([]scopedRow, error) {
	rows, err := database.QueryContext(ctx,
		`SELECT t.id, t.key, COALESCE(t.scope,''),
		        CASE WHEN t.owner_agency = '`+agencyid.Global+`' THEN '' ELSE COALESCE(ag.name,'') END
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
