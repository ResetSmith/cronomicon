package gitlab

import (
	"path/filepath"
	"strings"
	"testing"
)

// `cronomicon validate` is told what a checkout is (2.4.0 Phase R7; GR-14,
// GR-25, R0's item 3 for R7, R4's note for R7).
//
// A checkout cannot say which repository it is, and one rule differs: an
// agency's job must name a scope (GR-14: sync refuses one with none), where
// Global's may name none (it is Global's then).
//
//   - With no option, the checkout is Global's: as before.
//   - -agency: a job with no scope is an error; one whose scope is not the
//     checkout's own is a warning.
//   - In both, a reference the checkout does not have is an ERROR, as at sync
//     (§0a, IN-8). From R7 until §0a an agency's was a warning that named
//     `-library`, which looked in a checkout of Global's repository too; that
//     flag went with borrowing (§0a, open point 3).
func TestValidateRepoIsToldWhatTheCheckoutIs(t *testing.T) {
	repo := t.TempDir()
	job := func(name, spec string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n" + spec
	}
	mustWrite(t, filepath.Join(repo, "inventory", "fin.ini"), "[web]\nhost1\n")
	mustWrite(t, filepath.Join(repo, "jobs", "borrows.yaml"), job("borrows",
		"  script_ref: global-script\n  scope: fin\n  scheduleRefs: [global-nightly]\n"+
			"  reactions:\n    - {name: after, onKind: job, onName: global-job, onOutcome: success}\n"))
	mustWrite(t, filepath.Join(repo, "jobs", "noscope.yaml"), job("noscope", "  run_type: bash\n  command: echo\n"))
	mustWrite(t, filepath.Join(repo, "jobs", "other.yaml"), job("other", "  run_type: bash\n  command: echo\n  scope: built-in-app\n"))
	mustWrite(t, filepath.Join(repo, "jobs", "dangling.yaml"), job("dangling", "  script_ref: nowhere\n  scope: fin\n"))
	mustWrite(t, filepath.Join(repo, "workflows", "flow.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: flow\nspec:\n  steps:\n    - {type: job, name: global-job}\n")

	type found struct{ errs, warnings []ValidationError }
	run := func(opt ValidateOptions) found {
		t.Helper()
		errs, warnings, err := ValidateRepoWith(repo, opt)
		if err != nil {
			t.Fatalf("ValidateRepoWith(%+v): %v", opt, err)
		}
		return found{errs, warnings}
	}
	in := func(list []ValidationError, file, field, text string) bool {
		for _, e := range list {
			if strings.HasSuffix(e.File, file) && e.Field == field && strings.Contains(e.Message, text) {
				return true
			}
		}
		return false
	}
	of := func(list []ValidationError, file string) []ValidationError {
		var out []ValidationError
		for _, e := range list {
			if strings.HasSuffix(e.File, file) {
				out = append(out, e)
			}
		}
		return out
	}

	// ── Global's repository (no option): as it always was.
	g := run(ValidateOptions{})
	for _, c := range []struct{ field, text string }{
		{"spec.script_ref", `"global-script"`}, {"spec.scheduleRefs", `"global-nightly"`}, {"spec.reactions[after].onName", `no such job "global-job"`},
	} {
		if !in(g.errs, "jobs/borrows.yaml", c.field, c.text) {
			t.Errorf("Global's checkout: %s is not an error (Global's has nothing to borrow from): %v", c.text, g.errs)
		}
	}
	if !in(g.errs, "jobs/dangling.yaml", "spec.script_ref", `"nowhere"`) {
		t.Errorf("Global's checkout: a dangling script_ref is not an error: %v", g.errs)
	}
	if len(of(g.errs, "jobs/noscope.yaml"))+len(of(g.warnings, "jobs/noscope.yaml"))+len(of(g.warnings, "jobs/other.yaml")) != 0 {
		t.Errorf("Global's checkout: a job with no scope, or another's, is reported (Global's may name any or none): %v %v", g.errs, g.warnings)
	}
	if !in(g.warnings, "workflows/flow.yaml", "spec.steps", `step job "global-job"`) || in(g.warnings, "workflows/flow.yaml", "spec.steps", "may be Global's") {
		t.Errorf("Global's checkout: the step is not the plain warning: %v", g.warnings)
	}

	// ── An agency's.
	a := run(ValidateOptions{Agency: true})
	if !in(a.errs, "jobs/noscope.yaml", "spec.scope", "must name one of that agency's scopes") {
		t.Errorf("an agency's checkout: a job with no scope is not an error: %v", a.errs)
	}
	if !in(a.warnings, "jobs/other.yaml", "spec.scope", `the scope "built-in-app" is not one of this repository's`) {
		t.Errorf("an agency's checkout: a scope not the checkout's own is not a warning: %v", a.warnings)
	}
	if n := len(of(a.errs, "jobs/other.yaml")); n != 0 {
		t.Errorf("an agency's checkout: a scope built in the app is an error, where sync may accept it: %v", a.errs)
	}
	for _, c := range []struct{ file, field, text string }{
		{"jobs/borrows.yaml", "spec.script_ref", `"global-script"`},
		{"jobs/borrows.yaml", "spec.scheduleRefs", `"global-nightly"`},
		{"jobs/borrows.yaml", "spec.reactions[after].onName", `no such job "global-job"`},
		{"jobs/dangling.yaml", "spec.script_ref", `"nowhere"`},
	} {
		if !in(a.errs, c.file, c.field, c.text) {
			t.Errorf("an agency's checkout: %s %s is not an error (a repository takes nothing from Global's): %v", c.file, c.text, a.errs)
		}
	}
	for _, list := range [][]ValidationError{a.errs, a.warnings} {
		for _, e := range list {
			if strings.Contains(e.Message, "-library") || strings.Contains(e.Message, "may be Global's") {
				t.Errorf("an agency's checkout is still told a reference may be Global's: %v", e)
			}
		}
	}
	if !in(a.warnings, "workflows/flow.yaml", "spec.steps", `step job "global-job"`) {
		t.Errorf("an agency's checkout: the step is not the plain warning: %v", a.warnings)
	}
	if in(a.warnings, "jobs/borrows.yaml", "spec.scope", "") {
		t.Errorf("an agency's checkout: the job whose scope is the checkout's own is reported for its scope: %v", a.warnings)
	}
}
