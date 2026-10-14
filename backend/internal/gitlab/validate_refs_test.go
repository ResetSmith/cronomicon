package gitlab

import (
	"path/filepath"
	"strings"
	"testing"
)

// `cronomicon validate <dir>` checks the references to a job or a workflow
// (2.4.0 Phase R7; present defect 22, R0's item 2 for R7). It checked a job's
// script_ref and the scheduleRefs only.
//
// A reaction's upstream is checked as sync checks it: a definition of the
// checkout of the kind named, or an ERROR (sync does not write the definition);
// one that names a definition built in the app (onSource: cronomicon) cannot be
// seen and is not checked; a definition that reacts to itself is an error.
//
// A workflow's step job and sub-workflow are resolved by the engine when the
// workflow runs, and sync does not check them. One that names nothing in the
// checkout is a WARNING: a step whose source is not given falls back to the
// definitions built in the app, and an error would fail CI on what sync
// accepts.
func TestValidateRepoChecksTheReferencesToAJobOrAWorkflow(t *testing.T) {
	dir := t.TempDir()
	job := func(name, extra string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: " + name + "\nspec:\n  run_type: bash\n  command: echo\n  scope: Prod\n" + extra
	}
	flow := func(name, extra string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Workflow\nmetadata:\n  name: " + name + "\nspec:\n" + extra
	}
	react := func(kind, name, source string) string {
		r := "  reactions:\n    - name: after\n      onKind: " + kind + "\n      onName: " + name + "\n      onOutcome: success\n"
		if source != "" {
			r += "      onSource: " + source + "\n"
		}
		return r
	}
	mustWrite(t, filepath.Join(dir, "jobs", "a.yaml"), job("a", react("workflow", "zz-later", "")))   // a workflow read after it
	mustWrite(t, filepath.Join(dir, "jobs", "b.yaml"), job("b", react("job", "ghost", "")))           // nothing of that name
	mustWrite(t, filepath.Join(dir, "jobs", "c.yaml"), job("c", react("job", "built-in-app", "cronomicon"))) // cannot be seen
	mustWrite(t, filepath.Join(dir, "jobs", "d.yaml"), job("d", react("job", "d", "")))               // itself
	mustWrite(t, filepath.Join(dir, "jobs", "e.yaml"), job("e", react("workflow", "a", "")))          // a job of that name, not a workflow
	mustWrite(t, filepath.Join(dir, "workflows", "zz-later.yaml"), flow("zz-later",
		"  steps:\n"+
			"    - {type: job, name: a}\n"+
			"    - {type: job, name: missing-job}\n"+
			"    - {type: job, name: pinned-git, jobSource: git}\n"+
			"    - {type: job, name: in-app, jobSource: cronomicon}\n"+
			"    - type: parallel\n      jobs: [{type: job, name: nested-missing}]\n"+
			"    - {type: workflow, name: go, workflow: no-such-flow}\n"+
			"    - {type: workflow, name: go2, workflow: zz-later}\n"+
			react("job", "ghost-of-a-flow", "")))

	errs, warnings, err := ValidateRepo(dir)
	if err != nil {
		t.Fatalf("ValidateRepo: %v", err)
	}
	find := func(list []ValidationError, file, text string) bool {
		for _, e := range list {
			if strings.HasSuffix(e.File, file) && strings.Contains(e.Error(), text) {
				return true
			}
		}
		return false
	}
	for _, c := range []struct{ what, file, text string }{
		{"a reaction to a job that is not there", "jobs/b.yaml", `no such job "ghost"`},
		{"a reaction to itself", "jobs/d.yaml", "cannot react to itself"},
		{"a reaction to a workflow that is a job's name", "jobs/e.yaml", `no such workflow "a"`},
		{"a workflow's reaction to a job that is not there", "workflows/zz-later.yaml", `no such job "ghost-of-a-flow"`},
	} {
		if !find(errs, c.file, c.text) {
			t.Errorf("not an error: %s\nerrors: %v", c.what, errs)
		}
	}
	for _, c := range []struct{ what, text string }{
		{"a step job that is not in the checkout", `step job "missing-job" is not a job of this repository: when the workflow runs`},
		{"a step job, in a parallel arm, that is not in the checkout", `"nested-missing"`},
		{"a step job pinned to Git that is not in the checkout", `step job "pinned-git" is not a job of this repository: the step fails`},
		{"a sub-workflow that is not in the checkout", `step workflow "no-such-flow"`},
	} {
		if !find(warnings, "workflows/zz-later.yaml", c.text) {
			t.Errorf("not a warning: %s\nwarnings: %v", c.what, warnings)
		}
	}
	// Not reported: what resolves, and what a checkout cannot see.
	for _, quiet := range []string{`"zz-later"`, `"built-in-app"`, `step job "a"`, `"in-app"`} {
		for _, e := range append(errs, warnings...) {
			if strings.Contains(e.Message, quiet) {
				t.Errorf("%s is reported: %v", quiet, e)
			}
		}
	}
	// None of these is an error: a step is the engine's to resolve.
	for _, e := range errs {
		if strings.HasSuffix(e.File, "workflows/zz-later.yaml") && e.Field == "spec.steps" {
			t.Errorf("a workflow's step is an error, which sync would not make it: %v", e)
		}
	}
}
