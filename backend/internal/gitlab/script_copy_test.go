package gitlab

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// An in-app job follows its script.
//
// A job holds a COPY of its script: the run type, the body or the path to it,
// the executor, the hash. Sync has always rewritten that copy for a job that
// comes from Git. Until this test was inverted
// (TestGR0_AnInAppJobKeepsAnOldCopyOfItsInlineScript pinned the opposite) it
// rewrote nothing for a job built in the app, which went on running the body it
// had when it was last saved while the catalogue showed the new one. Since
// migration 1290 a job is joined to its script (jobs.script_uid), and sync
// refreshes the copy of every in-app job joined to a script it writes.
func TestAnInAppJobFollowsItsScript(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	inline := func(command string) string {
		return "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: greet\nspec:\n  run_type: bash\n  command: " + command + "\n"
	}
	const project = "apiVersion: cronomicon.io/v1\nkind: Script\nmetadata:\n  name: proj\nspec:\n  run_type: ansible\n  project_root: scripts/proj\n  entry: scripts/proj/site.yml\n"
	grCommitFiles(t, repo, remote, map[string]string{
		"scripts/greet.yaml":    inline("echo one"),
		"scripts/proj.yaml":     project,
		"scripts/proj/site.yml": "- hosts: localhost\n  gather_facts: false\n  tasks: []\n",
		"jobs/greet-git.yaml":   "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: greet-git\nspec:\n  script_ref: greet\n",
	}, "an inline script, a project, and a Git job")
	grSync(t, svc, "first sync")
	greet := grString(t, svc.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='greet'`)
	proj := grString(t, svc.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='proj'`)
	hashOne := grString(t, svc.db, `SELECT content_hash FROM scripts WHERE uid=?`, greet)

	// In-app jobs, as the composer writes them: the name, the join, and a copy
	// of the script as it was then. The one on the project has no checkout
	// marker: the composer does not copy it, and the refresh must not add it.
	job := `INSERT INTO jobs(uid, name, source, run_type, command, script_path, content_hash, synced_at, script_ref, script_uid,
	                         last_modified_by, last_modified_at, deleted_at)
	        VALUES(?, ?, 'cronomicon', ?, ?, ?, ?, 't', ?, ?, 'alice', '2026-01-01T00:00:00Z', ?)`
	exec(job, "j-app", "greet-app", "bash", "echo one", nil, hashOne, "greet", greet, nil)
	exec(job, "j-binned", "greet-binned", "bash", "echo one", nil, hashOne, "greet", greet, "2026-02-01T00:00:00Z")
	exec(job, "j-proj", "proj-app", "ansible", nil, "scripts/proj/site.yml", "h", "proj", proj, nil)
	// One that names the script and is NOT joined to it (its script had gone
	// when it was saved, say) and that names another script altogether.
	exec(job, "j-other", "other-app", "bash", "echo mine", nil, "h-mine", "something-else", nil, nil)

	gitCommitFile(t, repo, remote, "scripts/greet.yaml", inline("echo two"), "edit the script")
	grSync(t, svc, "sync after the edit")
	hashTwo := grString(t, svc.db, `SELECT content_hash FROM scripts WHERE uid=?`, greet)
	if hashTwo == hashOne {
		t.Fatal("the edit did not change the script's hash")
	}

	// What each job would execute now, asked the way dispatch asks.
	runs := func(name, source string) string {
		t.Helper()
		_, body, err := execspec.ResolveCommand(ctx, svc.db, svc.cloneDir, name, source, "bash")
		if err != nil {
			t.Fatalf("ResolveCommand(%s): %v", name, err)
		}
		return body
	}
	if got := runs("greet-git", "git"); got != "echo two" {
		t.Errorf("the Git job would run %q, want the edited body", got)
	}
	if got := runs("greet-app", "cronomicon"); got != "echo two" {
		t.Errorf("the in-app job would run %q, want the edited body: it follows its script", got)
	}
	col := func(uid, column string) string {
		t.Helper()
		return grString(t, svc.db, `SELECT COALESCE(`+column+`,'') FROM jobs WHERE uid=?`, uid)
	}
	if got := col("j-app", "content_hash"); got != hashTwo {
		t.Errorf("the in-app job's hash = %q, want the script's %q", got, hashTwo)
	}
	// Nobody edited the job: its authorship is as it was.
	if by, at := col("j-app", "last_modified_by"), col("j-app", "last_modified_at"); by != "alice" || at != "2026-01-01T00:00:00Z" {
		t.Errorf("the refresh rewrote who last modified the job: %q at %q", by, at)
	}
	// A binned job is refreshed too, so that restoring it does not bring an old
	// body back; it stays binned.
	if got := col("j-binned", "command"); got != "echo two" {
		t.Errorf("the binned in-app job holds %q, want the edited body", got)
	}
	if got := col("j-binned", "deleted_at"); got == "" {
		t.Errorf("the refresh took the job out of the recycle bin")
	}
	// The job on a project is refreshed like any other and is NOT made a
	// checkout job by it: the composer never gave it the marker, and whether it
	// should have one is a decision this change does not take
	// (TestGR0_AnInAppJobOnAProjectIsNotACheckoutJob, internal/api).
	if got := col("j-proj", "project_root"); got != "" {
		t.Errorf("the refresh gave the in-app job on a project a checkout marker (%q)", got)
	}
	if got := col("j-proj", "script_path"); got != "scripts/proj/site.yml" {
		t.Errorf("the in-app job on a project has script_path %q, want the entry", got)
	}
	// A job that is not joined to a script this sync wrote is not touched.
	if got := col("j-other", "command"); got != "echo mine" {
		t.Errorf("a job joined to no script was rewritten: %q", got)
	}
}
