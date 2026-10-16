package gitlab

import (
	"path/filepath"
	"strings"
	"testing"
)

// `cronomicon validate <dir>` reads a checkout as Git sync reads it (2.4.0
// Phase R7; present defect 22).
//
// Sync walks jobs/, schedules/ and workflows/ into their sub-folders
// (walkManifests). The validator read their top level only: a definition in a
// sub-folder was synced and never validated. And it did not read inventory/ at
// all in directory mode, which sync reads at its top level only: an inventory
// that carries a secret, which sync refuses, passed. A file in a sub-folder of
// inventory/ is no scope to sync, and is said to be none.
func TestValidateRepoReadsACheckoutAsSyncReadsIt(t *testing.T) {
	dir := t.TempDir()
	job := func(name, extra string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: bash\n  command: echo\n  scope: Prod\n" + extra
	}
	mustWrite(t, filepath.Join(dir, "jobs", "top.yaml"), job("top", "  scheduleRefs: [deep-nightly]\n"))
	mustWrite(t, filepath.Join(dir, "jobs", "team", "deep.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: deep\nspec:\n  script_ref: nowhere\n  scope: Prod\n")
	mustWrite(t, filepath.Join(dir, "jobs", "team", "again", "twin.yaml"), job("top", ""))
	mustWrite(t, filepath.Join(dir, "schedules", "nights", "nightly.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: deep-nightly\nspec:\n  cron: '0 2 * * *'\n")
	mustWrite(t, filepath.Join(dir, "workflows", "flow.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  steps: [{type: job, name: top}]\n")
	mustWrite(t, filepath.Join(dir, "workflows", "more", "flow.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  steps: [{type: job, name: top}]\n")
	mustWrite(t, filepath.Join(dir, "workflows", "more", "bad.yaml"), "apiVersion: cronomicon.io/v9\nkind: Workflow\n")
	mustWrite(t, filepath.Join(dir, "inventory", "fin.ini"), "[web]\nhost1 ansible_password=hunter2\n")
	mustWrite(t, filepath.Join(dir, "inventory", "tax.ini"), "# cronomicon:v1 colour=blue\n[web]\nhost2\n")
	mustWrite(t, filepath.Join(dir, "inventory", "ok.ini"), "# cronomicon:v1 owner=ops\n[web]\nhost3\n")
	mustWrite(t, filepath.Join(dir, "inventory", "old", "hr.ini"), "[web]\nhost4 ansible_password=not-read\n")
	mustWrite(t, filepath.Join(dir, "inventory", "notes.txt"), "not a scope, and not in a sub-folder\n")

	errs, warnings, err := ValidateRepo(dir)
	if err != nil {
		t.Fatalf("ValidateRepo: %v", err)
	}
	has := func(list []ValidationError, file, field, text string) bool {
		for _, e := range list {
			if strings.HasSuffix(e.File, file) && (field == "" || e.Field == field) && strings.Contains(e.Error(), text) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ what, file, field, text string }{
		{"a job in a sub-folder whose script_ref resolves to nothing", "jobs/team/deep.yaml", "spec.script_ref", "nowhere"},
		// (Reported against whichever of the two is read second, naming the other.)
		{"a job name taken by a file in a sub-folder", ".yaml", "metadata.name", `duplicate job name "top" (also defined in`},
		{"a workflow name taken by a file in a sub-folder", "workflows/more/flow.yaml", "metadata.name", `duplicate workflow name "flow"`},
		{"a workflow in a sub-folder of an unknown apiVersion", "workflows/more/bad.yaml", "", "apiVersion"},
		{"an inventory that carries a secret", "inventory/fin.ini", "", "ansible_password"},
		{"an inventory with an unknown pragma directive", "inventory/tax.ini", "", "colour"},
	} {
		if !has(errs, c.file, c.field, c.text) {
			t.Errorf("not reported as an error: %s (%s)\nerrors: %v", c.what, c.file, errs)
		}
	}
	// A scheduleRef to a schedule in a sub-folder resolves.
	if has(errs, "jobs/top.yaml", "spec.scheduleRefs", "deep-nightly") {
		t.Errorf("a scheduleRef to a schedule in a sub-folder of schedules/ did not resolve: %v", errs)
	}
	// Sync does not read a sub-folder of inventory/: what is there is said not
	// to be a scope, and is not checked as one.
	if !has(warnings, "inventory/old/hr.ini", "", "does not read") {
		t.Errorf("a file in a sub-folder of inventory/ is not said to be read by nothing: %v", warnings)
	}
	if has(errs, "inventory/old/hr.ini", "", "") {
		t.Errorf("a file in a sub-folder of inventory/, which sync does not read, was checked as a scope: %v", errs)
	}
	for _, e := range append(errs, warnings...) {
		if strings.HasSuffix(e.File, "inventory/ok.ini") || strings.HasSuffix(e.File, "inventory/notes.txt") {
			t.Errorf("a good inventory, or a file sync does not read, is reported: %v", e)
		}
	}
}
