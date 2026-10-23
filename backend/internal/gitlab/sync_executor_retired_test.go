package gitlab

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSyncSurvivesAnExecutorValueTheKeyNeverHad: `executor` is retired (2.3.0,
// LR-48), so validation no longer refuses a value outside ssh|runner — but the
// two columns it is stored in still carry that CHECK. A value such as `auto`
// or `Runner` must therefore be stored as nothing: stored as written it fails
// the INSERT, the transaction rolls back, and ONE stale line in one file stops
// every definition in the repository from syncing, after `cronomicon validate`
// passed it with a warning.
func TestSyncSurvivesAnExecutorValueTheKeyNeverHad(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	var logs bytes.Buffer
	svc.log = slog.New(slog.NewTextHandler(&logs, nil))

	gitCommitFile(t, repo, remote, "inventory/dmz.ini", "[web]\nweb1\n", "add dmz")
	gitCommitFile(t, repo, remote, "jobs/odd.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: odd\nspec:\n  run_type: bash\n  command: echo hi\n  scope: dmz\n  executor: auto\n", "add odd")
	gitCommitFile(t, repo, remote, "scripts/odd-script.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: odd-script\nspec:\n  run_type: bash\n  command: echo hi\n  executor: Runner\n", "add odd-script")
	gitCommitFile(t, repo, remote, "jobs/via.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: via\nspec:\n  script_ref: odd-script\n  scope: dmz\n", "add via")
	gitCommitFile(t, repo, remote, "jobs/plain.yaml",
		"apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: plain\nspec:\n  run_type: bash\n  command: echo hi\n  scope: dmz\n", "add plain")

	if r := svc.SyncBlocking(ctx, "t"); r.Status != "success" {
		t.Fatalf("sync = %q, want success — a retired key is never an error, whatever it says (%s)", r.Status, r.ErrorMessage)
	}
	for _, q := range []struct{ what, sql string }{
		{"job odd", `SELECT COALESCE(executor, '') FROM jobs WHERE name = 'odd'`},
		{"job via", `SELECT COALESCE(executor, '') FROM jobs WHERE name = 'via'`},
		{"job plain", `SELECT COALESCE(executor, '') FROM jobs WHERE name = 'plain'`},
		{"script odd-script", `SELECT COALESCE(executor, '') FROM scripts WHERE name = 'odd-script'`},
	} {
		var got string
		if err := svc.db.QueryRow(q.sql).Scan(&got); err != nil {
			t.Errorf("%s did not sync: %v", q.what, err)
		} else if got != "" {
			t.Errorf("%s stores executor %q, want nothing: the value is not one the column accepts", q.what, got)
		}
	}
	// The sidecar is still named: its line is there, whatever it says.
	if !strings.Contains(logs.String(), "script still declares executor") || !strings.Contains(logs.String(), "executor=Runner") {
		t.Errorf("the sidecar's line drew no warning:\n%s", logs.String())
	}
}

// TestValidateWarnsAboutARetiredExecutor — `cronomicon validate` reports the
// retired key as a WARNING with its line, for a Job and for a Script sidecar,
// on a single file and across a checkout (the form the CI template runs), and
// never as an error, whatever the value: a job-definitions repository written
// before the change must keep passing its CI check.
func TestValidateWarnsAboutARetiredExecutor(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"jobs", "scripts"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	job := "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: %s\nspec:\n  run_type: bash\n  command: echo hi\n"
	script := "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: %s\nspec:\n  run_type: bash\n  command: echo hi\n"
	named := func(tpl, name string) string { return strings.Replace(tpl, "%s", name, 1) }
	sshJob := write("jobs/ssh.yaml", named(job, "ssh-job")+"  executor: ssh\n")
	oddJob := write("jobs/odd.yaml", named(job, "odd-job")+"  executor: bogus\n")
	plainJob := write("jobs/plain.yaml", named(job, "plain-job"))
	sidecar := write("scripts/runner.yaml", named(script, "runner-script")+"  executor: runner\n")
	write("scripts/plain.yaml", named(script, "plain-script"))

	for _, path := range []string{sshJob, oddJob, sidecar} {
		errs, warnings, err := ValidateFile(path)
		if err != nil || len(errs) != 0 {
			t.Errorf("ValidateFile(%s) = errs %v, err %v; want none — the key is retired, not invalid", filepath.Base(path), errs, err)
		}
		if len(warnings) != 1 || warnings[0].Line != 8 || warnings[0].Field != "spec.executor" ||
			!strings.Contains(warnings[0].Message, "no longer used") {
			t.Errorf("ValidateFile(%s) warnings = %+v, want one, on line 8, for spec.executor", filepath.Base(path), warnings)
		}
	}
	if _, warnings, _ := ValidateFile(plainJob); len(warnings) != 0 {
		t.Errorf("a job without the key drew warnings: %+v", warnings)
	}

	errs, warnings, err := ValidateRepo(dir)
	if err != nil || len(errs) != 0 {
		t.Fatalf("ValidateRepo = errs %v, err %v; want none", errs, err)
	}
	got := map[string]int{}
	for _, w := range warnings {
		if w.Field == "spec.executor" {
			got[filepath.Base(filepath.Dir(w.File))+"/"+filepath.Base(w.File)]++
		}
	}
	want := map[string]int{"jobs/ssh.yaml": 1, "jobs/odd.yaml": 1, "scripts/runner.yaml": 1}
	if len(got) != len(want) {
		t.Errorf("ValidateRepo executor warnings = %v, want %v", got, want)
	}
	for f, n := range want {
		if got[f] != n {
			t.Errorf("ValidateRepo executor warnings for %s = %d, want %d (all: %v)", f, got[f], n, got)
		}
	}
}
