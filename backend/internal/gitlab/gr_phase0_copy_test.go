package gitlab

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// A job holds a COPY of its script: the run type, the body (or the path to
// it), the hash. Sync rewrites that copy for a job that comes from Git, every
// time. Nothing rewrites it for a job that was built in the app. So when a
// script whose body is written inline in its YAML is edited in Git, the Git
// jobs that use it run the new body from the next sync on, and the in-app
// jobs that use the same script go on running the OLD one until somebody opens
// and saves each of them. The catalogue shows the new body; the job runs the
// old.
//
// A script kept as a separate file is not affected in the same way: its body is
// read from the clone at every run, by the path the job copied.
//
// Phase R0 read this; this test reproduces it. It passes on the code as it is.
// No phase of the 2.4.0 plan fixes it as first written. Since migration 1290 a
// job is joined to its script (jobs.script_uid), so the fix is one statement in
// upsertScripts; whether an in-app job SHOULD follow its script's edits without
// being re-saved is the owner's to say.
func TestGR0_AnInAppJobKeepsAnOldCopyOfItsInlineScript(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	script := func(command string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: greet\nspec:\n  run_type: bash\n  command: " + command + "\n"
	}
	grCommitFiles(t, repo, remote, map[string]string{
		"scripts/greet.yaml":  script("echo one"),
		"jobs/greet-git.yaml": "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: greet-git\nspec:\n  script_ref: greet\n",
	}, "the script and a Git job that uses it")
	grSync(t, svc, "first sync")
	uid := grString(t, svc.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='greet'`)
	hashOne := grString(t, svc.db, `SELECT content_hash FROM scripts WHERE uid=?`, uid)

	// An in-app job on the same script, as the composer writes one: the name,
	// the join, and a copy of the script as it is at that moment.
	if _, err := svc.db.Exec(`
		INSERT INTO jobs(uid, name, source, run_type, command, content_hash, synced_at, script_ref, script_uid)
		VALUES('j-app', 'greet-app', 'cronomicon', 'bash', 'echo one', ?, 't', 'greet', ?)`, hashOne, uid); err != nil {
		t.Fatalf("seed the in-app job: %v", err)
	}

	// The script is edited in Git and synced.
	gitCommitFile(t, repo, remote, "scripts/greet.yaml", script("echo two"), "edit the script")
	grSync(t, svc, "sync after the edit")
	if got := grString(t, svc.db, `SELECT command FROM scripts WHERE uid=?`, uid); got != "echo two" {
		t.Fatalf("the catalogue's script = %q, want the edited body", got)
	}

	// What each job would execute now, asked the way dispatch asks.
	runs := func(job, source string) string {
		t.Helper()
		_, body, err := execspec.ResolveCommand(ctx, svc.db, svc.cloneDir, job, source, "bash")
		if err != nil {
			t.Fatalf("ResolveCommand(%s): %v", job, err)
		}
		return body
	}
	if got := runs("greet-git", "git"); got != "echo two" {
		t.Errorf("the Git job would run %q, want the edited body", got)
	}
	switch got := runs("greet-app", "cronomicon"); got {
	case "echo one":
		// Today: still the body it copied when it was saved.
		if h := grString(t, svc.db, `SELECT content_hash FROM jobs WHERE uid='j-app'`); h != hashOne {
			t.Errorf("the in-app job's hash moved (%q) although its body did not", h)
		}
	case "echo two":
		t.Errorf("the in-app job follows its script's edit: this is fixed, and the test is to be inverted")
	default:
		t.Errorf("the in-app job would run %q: neither the old body nor the new", got)
	}
}
