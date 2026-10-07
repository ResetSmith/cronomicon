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

const (
	// Global is the agency's id. It is fixed, unlike every other agency's
	// (which is generated), because code and triggers name it.
	Global = "global"

	// GlobalName is its name, and it never changes: a run stores its agencies by
	// NAME (runs.agencies_json, run_agencies.agency), so a snapshot that says
	// "Global" must mean this agency for as long as the row exists. The catalog
	// refuses to rename or delete the built-in row, and refuses another agency
	// this name in any letter case.
	GlobalName = "Global"
)

// IsGlobalName reports whether name is Global's, in any letter case. It is the
// reservation check: "global" and "GLOBAL" would be read as this agency by
// every person who saw them, so no other agency may hold them.
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
