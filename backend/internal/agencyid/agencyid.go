// Package agencyid names the one agency every installation has.
//
// Global is the built-in agency of the global administrators (LR-8, LR-21,
// migration 1220). Until v2.3.0 "global" was the ABSENCE of a membership row,
// and absence meant a different thing for each kind of entity: a secret with no
// row was usable by every agency's runs, a runner with none claimed only untagged
// work, a scope with none was visible to global administrators alone. Every one
// of those is now a row that names this agency, and a missing row is an error.
//
// The package is a leaf so that every layer — the claim query, the reference
// resolver, the API gates, the migrations' tests — spells the id and the name the
// same way without importing one another.
package agencyid

import (
	"context"
	"database/sql"
)

const (
	// Global is the agency's id. It is fixed, unlike every other agency's
	// (which is generated), because code and triggers name it.
	Global = "global"

	// GlobalName is the name migration 1220 gave it. Since 2.5.2 (GR) the name
	// may change — a new installation calls it "Central", and a global
	// administrator may rename it — so code that writes or compares the
	// agency's name reads it by id (CurrentGlobalName); this is only what an
	// installation from before 2.5.2 calls it until then, and the fallback
	// when the catalog cannot be read. Runs store their agencies by NAME, and
	// the rename path carries a new name onto waiting runs.
	GlobalName = "Global"
)

// Queryer is what CurrentGlobalName reads through (a *sql.DB or *sql.Tx).
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// CurrentGlobalName is the built-in agency's name now, read by its id (2.5.2
// GR): what a run snapshot must carry to be this agency's, and what a name
// comparison must compare with. GlobalName when it cannot be read.
func CurrentGlobalName(ctx context.Context, q Queryer) string {
	var name string
	if err := q.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, Global).Scan(&name); err != nil || name == "" {
		return GlobalName
	}
	return name
}

// IsGlobalName reports whether name is "Global", in any letter case: the name
// migration 1220 gave the built-in agency. Until 2.5.2 it was the reservation
// check; since GR the name is not reserved and may change, so a comparison
// with the agency's name now uses CurrentGlobalName.
func IsGlobalName(name string) bool {
	if len(name) != len(GlobalName) {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		g := GlobalName[i]
		if g >= 'A' && g <= 'Z' {
			g += 'a' - 'A'
		}
		if c != g {
			return false
		}
	}
	return true
}
