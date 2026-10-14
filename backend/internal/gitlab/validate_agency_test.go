package gitlab

import (
	"path/filepath"
	"strings"
	"testing"
)

// `cronomicon validate` is told what a checkout is (2.4.0 Phase R7; GR-14,
// GR-25, R0's item 3 for R7, R4's note for R7).
//
// A checkout cannot say which repository it is, and the rules differ. Global's
// repository has nothing to borrow from: a reference it does not have is an
// error, and a job may name any scope or none. An agency's repository borrows
// from Global's (GR-16), so a job that validated could be refused at sync
// (GR-14: no scope) and one sync accepts on Global's script failed here.
//
//   - With no option, the checkout is Global's: as before.
//   - -agency: a job with no scope is an error; one whose scope is not the
//     checkout's own is a warning; a reference the checkout does not have is a
//     WARNING that names -library (GR-25).
//   - -library <Global's checkout>: such a reference is looked for there, and
//     one found in neither is the error. It implies -agency.
func TestValidateRepoIsToldWhatTheCheckoutIs(t *testing.T) {
	repo, lib := t.TempDir(), t.TempDir()
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

	mustWrite(t, filepath.Join(lib, "scripts", "global-script.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: global-script\nspec:\n  run_type: bash\n  command: echo\n")
	mustWrite(t, filepath.Join(lib, "schedules", "nights", "g.yaml"),
		"apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: global-nightly\nspec:\n  cron: '0 2 * * *'\n")
	mustWrite(t, filepath.Join(lib, "jobs", "team", "global-job.yaml"), job("global-job", "  run_type: bash\n  command: echo\n"))

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

	// ── An agency's, without a library.
	a := run(ValidateOptions{Agency: true})
	if !in(a.errs, "jobs/noscope.yaml", "spec.scope", "must name one of that agency's scopes") {
		t.Errorf("an agency's checkout: a job with no scope is not an error: %v", a.errs)
	}
	if !in(a.warnings, "jobs/other.yaml", "spec.scope", `the scope "built-in-app" is not one of this repository's`) {
		t.Errorf("an agency's checkout: a scope not the checkout's own is not a warning: %v", a.warnings)
	}
	if n := len(of(a.errs, "jobs/borrows.yaml")) + len(of(a.errs, "jobs/dangling.yaml")) + len(of(a.errs, "jobs/other.yaml")); n != 0 {
		t.Errorf("an agency's checkout without a library: %d errors for what may be Global's or its agency's: %v", n, a.errs)
	}
	for _, c := range []struct{ file, field, text string }{
		{"jobs/borrows.yaml", "spec.script_ref", `"global-script"`},
		{"jobs/borrows.yaml", "spec.scheduleRefs", `"global-nightly"`},
		{"jobs/borrows.yaml", "spec.reactions[after].onName", `no such job "global-job"`},
		{"jobs/dangling.yaml", "spec.script_ref", `"nowhere"`},
		{"workflows/flow.yaml", "spec.steps", "may be Global's"},
	} {
		if !in(a.warnings, c.file, c.field, c.text) || !in(a.warnings, c.file, c.field, "-library") {
			t.Errorf("an agency's checkout without a library: %s %s is not a warning that names -library: %v", c.file, c.text, a.warnings)
		}
	}
	if len(of(a.errs, "jobs/borrows.yaml"))+len(of(a.warnings, "jobs/borrows.yaml")) == 0 || in(a.warnings, "jobs/borrows.yaml", "spec.scope", "") {
		t.Errorf("an agency's checkout: the job whose scope is the checkout's own is reported for its scope: %v", a.warnings)
	}

	// ── An agency's, with Global's checkout as the library (which implies -agency).
	l := run(ValidateOptions{Library: lib})
	if len(of(l.errs, "jobs/borrows.yaml"))+len(of(l.warnings, "jobs/borrows.yaml"))+len(of(l.warnings, "workflows/flow.yaml")) != 0 {
		t.Errorf("with the library: what Global's checkout supplies is reported: %v %v", l.errs, l.warnings)
	}
	if !in(l.errs, "jobs/dangling.yaml", "spec.script_ref", "nor in Global's repository") {
		t.Errorf("with the library: a script_ref found in neither is not the error: %v", l.errs)
	}
	if !in(l.errs, "jobs/noscope.yaml", "spec.scope", "must name") {
		t.Errorf("-library does not imply -agency: %v", l.errs)
	}
	if _, _, err := ValidateRepoWith(repo, ValidateOptions{Library: filepath.Join(lib, "not-there")}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a library that is not there: %v, want it said", err)
	}
}
