package gitlab

import (
	"testing"
)

// A script's life, as sync writes it since migration 1290 (2.4.0, GR-5): it
// gets a uid when it first appears, keeps it through every edit, loses it
// with its row, and is a new script if it comes back. What hangs from the
// uid follows: reference bindings stay with the script through an edit and
// go when it goes, and an in-app job, which names its script and holds a copy
// of it, is let go when the script goes and joined again by name when a
// script of that name returns.
func TestSyncKeepsAScriptsIdentityThroughItsLife(t *testing.T) {
	svc, repo, remote := newSyncFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	scriptUID := func() string {
		t.Helper()
		if grCount(t, svc.db, `SELECT COUNT(*) FROM scripts WHERE repo_id='global' AND name='deploy.sh'`) == 0 {
			return ""
		}
		return grString(t, svc.db, `SELECT uid FROM scripts WHERE repo_id='global' AND name='deploy.sh'`)
	}
	jobScript := func(uid string) string {
		t.Helper()
		return grString(t, svc.db, `SELECT script_uid FROM jobs WHERE uid=?`, uid)
	}
	bindings := func(uid string) int {
		t.Helper()
		return grCount(t, svc.db, `SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid=?`, uid)
	}

	// The script appears, with a Git job that uses it.
	grCommitFiles(t, repo, remote, map[string]string{
		"scripts/deploy.sh": "#!/bin/bash\necho one\n",
		"jobs/roll.yaml":    "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: roll\nspec:\n  script_ref: deploy.sh\n",
	}, "deploy.sh and a job")
	grSync(t, svc, "first sync")
	first := scriptUID()
	if first == "" {
		t.Fatal("the script has no uid after its first sync")
	}
	if got := grString(t, svc.db, `SELECT repo_id FROM scripts WHERE uid=?`, first); got != "global" {
		t.Errorf("the script's repository = %q, want global", got)
	}
	gitJob := grString(t, svc.db, `SELECT uid FROM jobs WHERE source='git' AND name='roll'`)
	if got := jobScript(gitJob); got != first {
		t.Errorf("the Git job is joined to %q, want the script %q", got, first)
	}

	// An in-app job that names the script (as the composer would have written
	// it, but not yet joined), and a binding on the script.
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref) VALUES('j-app','roll-app','cronomicon','bash','t','deploy.sh')`)
	// And one left holding the uid of a script that no longer exists, which is
	// what the composer can write if a prune lands between its read of the
	// script and its transaction.
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref, script_uid) VALUES('j-stale','roll-stale','cronomicon','bash','t','deploy.sh','uid-of-a-script-that-is-gone')`)
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at, owner_uid)
	      VALUES('script','','deploy.sh','secret','DB_PASSWORD','t','t',?)`, first)
	exec(`UPDATE scripts SET tags='["blue"]' WHERE uid=?`, first)

	// An edit: same uid, so the binding, the tags and both jobs stay where
	// they are, and the in-app job is joined on this pass.
	gitCommitFile(t, repo, remote, "scripts/deploy.sh", "#!/bin/bash\necho two\n", "edit deploy.sh")
	grSync(t, svc, "sync after the edit")
	if got := scriptUID(); got != first {
		t.Fatalf("an edit gave the script a new uid: %q, was %q", got, first)
	}
	if n := bindings(first); n != 1 {
		t.Errorf("the script's bindings after an edit = %d, want 1", n)
	}
	if got := grString(t, svc.db, `SELECT tags FROM scripts WHERE uid=?`, first); got != `["blue"]` {
		t.Errorf("the script's tags after an edit = %s, want the operator's", got)
	}
	if got := jobScript("j-app"); got != first {
		t.Errorf("the in-app job is joined to %q, want %q", got, first)
	}
	if got := jobScript("j-stale"); got != first {
		t.Errorf("the in-app job that held a dead uid is joined to %q, want %q", got, first)
	}
	if got := jobScript(gitJob); got != first {
		t.Errorf("the Git job is joined to %q after the edit, want %q", got, first)
	}

	// The script goes (and the Git job with it, or the sync would refuse the
	// dangling script_ref and prune nothing).
	gitRemoveFile(t, repo, "scripts/deploy.sh", "remove deploy.sh")
	gitRemoveFile(t, repo, "jobs/roll.yaml", "remove the job")
	grBackdate(t, svc.db)
	grSync(t, svc, "sync after the removal")
	if got := scriptUID(); got != "" {
		t.Fatalf("the script is still there after its file was removed (uid %q)", got)
	}
	if n := bindings(first); n != 0 {
		t.Errorf("the removed script left %d bindings behind", n)
	}
	if got := jobScript("j-app"); got != "" {
		t.Errorf("the in-app job still points at the removed script: %q", got)
	}
	if got := grString(t, svc.db, `SELECT script_ref FROM jobs WHERE uid='j-app'`); got != "deploy.sh" {
		t.Errorf("the in-app job lost the name its author wrote: %q", got)
	}

	// It comes back: a new script, and the in-app job finds it by name.
	gitCommitFile(t, repo, remote, "scripts/deploy.sh", "#!/bin/bash\necho three\n", "deploy.sh again")
	grSync(t, svc, "sync after the return")
	second := scriptUID()
	if second == "" || second == first {
		t.Fatalf("the returned script's uid = %q (the first was %q): want a new one", second, first)
	}
	if got := jobScript("j-app"); got != second {
		t.Errorf("the in-app job is joined to %q, want the returned script %q", got, second)
	}
	if n := bindings(second); n != 0 {
		t.Errorf("the returned script inherited %d bindings of the old one", n)
	}
}

// Another repository's sync does not join Global's in-app jobs to ITS script
// of the same name: which repository an in-app job's name is looked up in is
// GR-16's rule, and until it is built (Phase R4) only Global's repository
// answers.
func TestAnotherRepositorysScriptDoesNotClaimAnInAppJob(t *testing.T) {
	a, _, _ := newSyncFixture(t)
	grSync(t, a, "global")
	if _, err := a.db.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref) VALUES('j-app','roll-app','cronomicon','bash','t','deploy.sh')`); err != nil {
		t.Fatal(err)
	}
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"scripts/deploy.sh": "#!/bin/bash\necho from-b\n"}, "b's deploy.sh")
	grSync(t, b, "second repository")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM scripts WHERE repo_id='repo-b' AND name='deploy.sh'`); n != 1 {
		t.Fatalf("the second repository's script was not written (count %d)", n)
	}
	if got := grString(t, a.db, `SELECT script_uid FROM jobs WHERE uid='j-app'`); got != "" {
		t.Errorf("the in-app job was joined to another repository's script: %q", got)
	}
}
