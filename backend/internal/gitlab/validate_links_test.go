package gitlab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `cronomicon validate` reads a checkout through sync's own reader (2.4.0
// Phase R7; found by the review of R7).
//
// Sync does not follow a symbolic link out of its clone, or into its .git: a
// file that is one is refused ("leads out of the repository"), and so is an
// inventory/ directory that is one. The validator followed them, so a job file
// linked to one outside the checkout validated, and in agency mode a linked
// inventory/ supplied scopes that sync would refuse, and a job naming one was
// not said to be anything. A link that stays inside the checkout is read, as
// at sync.
//
// And an inventory sidecar that is not YAML is skipped by sync in silence: the
// validator failed CI on it. It is a warning now.
func TestValidateRepoReadsACheckoutThroughSyncsReader(t *testing.T) {
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "job.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: linked\nspec:\n  run_type: bash\n  command: echo\n  scope: hr\n")
	mustWrite(t, filepath.Join(outside, "inv", "hr.ini"), "[web]\nhost9\n")

	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "jobs", "real.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: real\nspec:\n  run_type: bash\n  command: echo\n  scope: hr\n")
	if err := os.Symlink(filepath.Join(outside, "job.yaml"), filepath.Join(dir, "jobs", "linked.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", filepath.Join(dir, "jobs", "inside.yaml")); err != nil { // stays inside: read
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "inv"), filepath.Join(dir, "inventory")); err != nil {
		t.Fatal(err)
	}

	errs, warnings, err := ValidateRepoWith(dir, ValidateOptions{Agency: true})
	if err != nil {
		t.Fatalf("ValidateRepoWith: %v", err)
	}
	has := func(list []ValidationError, file, text string) bool {
		for _, e := range list {
			if strings.HasSuffix(e.File, file) && strings.Contains(e.Error(), text) {
				return true
			}
		}
		return false
	}
	if !has(errs, "jobs", "linked.yaml") || !has(errs, "jobs", "leads out of the repository") {
		t.Errorf("a job file linked out of the checkout is not refused as sync refuses it: %v", errs)
	}
	if !has(errs, "inventory", "leads out of the repository") {
		t.Errorf("an inventory/ linked out of the checkout is not refused as sync refuses it: %v", errs)
	}
	// Its scope is therefore not the checkout's, and a job naming it is said so.
	if !has(warnings, "jobs/real.yaml", `the scope "hr" is not one of this repository's`) {
		t.Errorf("a scope supplied only through a linked inventory/ counts as the checkout's: %v", warnings)
	}
	// A link inside the checkout is read: its job is a duplicate of the one it points at.
	if !has(errs, ".yaml", `duplicate job name "real"`) {
		t.Errorf("a link that stays inside the checkout was not read: %v", errs)
	}

	// A sidecar that is not YAML: a warning, not an error. And one inventory
	// FILE linked out of the checkout is refused, as sync refuses it.
	dir2 := t.TempDir()
	mustWrite(t, filepath.Join(dir2, "inventory", "prod.ini"), "[web]\nhost1\n")
	if err := os.Symlink(filepath.Join(outside, "inv", "hr.ini"), filepath.Join(dir2, "inventory", "hr.ini")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir2, "inventory", "prod.cronomicon.yaml"), "owner: [unclosed\n")
	errs, warnings, err = ValidateRepo(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if has(errs, "prod.cronomicon.yaml", "") {
		t.Errorf("a sidecar that is not YAML fails validation, which sync accepts: %v", errs)
	}
	if !has(warnings, "prod.cronomicon.yaml", "Git sync ignores it") {
		t.Errorf("a sidecar that is not YAML is not said to be ignored: %v", warnings)
	}
	if !has(errs, "inventory/hr.ini", "leads out of the repository") {
		t.Errorf("an inventory file linked out of the checkout is not refused as sync refuses it: %v", errs)
	}
}
