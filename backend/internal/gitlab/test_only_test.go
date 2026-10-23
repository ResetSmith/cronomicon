package gitlab

import (
	"os"

	"github.com/ResetSmith/cronomicon/internal/repoid"
)

// Test-only since 2.4.2: nothing in the server calls these any more (the review of 2.4.x).

// Global returns Global's repository's Service.
func (r *Registry) Global() *Service { return r.Service(repoid.Global) }

// ValidateRepo validates a whole job-definitions checkout rooted at dir. Unlike
// ValidateFile (one file, no cross-file context) it parses scripts/ first, then
// validates jobs/ INCLUDING script_ref resolution — a job whose script_ref names
// no scripts/<name>.yaml is a hard, line-attributed error (risk #1: caught at MR
// time, not run time). Orphan scripts (referenced by zero jobs) are returned
// separately as non-fatal warnings. A missing scripts/ or jobs/ dir is not an
// error (a repo may have only one).
//
// It reads the checkout as Git sync reads it (2.4.0): jobs/, schedules/ and
// workflows/ with their sub-folders, and inventory/ at its top level only
// (validateInventoryDir). It is ValidateRepoWith for Global's repository.
func ValidateRepo(dir string) (errs []ValidationError, warnings []ValidationError, err error) {
	return ValidateRepoWith(dir, ValidateOptions{})
}

// discoverScripts walks scripts/ and returns one ScriptYAML per catalog entry.
// It is TWO-PASS (§7 / review R1): a single WalkDir cannot both auto-synthesize
// bare files and honor project grouping, because WalkDir visits lexically — a
// project's member file (e.g. roles/x/tasks/main.yml) can be reached BEFORE the
// wrapper that claims its tree. Pass 1 collects every project_root claim; pass 2
// synthesizes and filters, skipping files inside a claimed root. Correctness no
// longer depends on visit order.
func discoverScripts(dir string) ([]ScriptYAML, []error) {
	return discoverScriptsWith(dir, os.ReadFile)
}
