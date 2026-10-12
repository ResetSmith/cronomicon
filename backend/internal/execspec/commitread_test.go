package execspec

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
)

func commitReadPool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "commitread.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// TestARunsBodyIsReadFromTheCommitAndNotTheWorkingTree (present defect 27; in
// the released code). A sync resets the clone's working tree in place: each
// changed file is truncated and written again. A run dispatched in that moment
// was handed an empty or cut-off script, and ran it.
//
// The body is read from the objects of the last synced commit. Whatever state
// the working tree is in, the run is handed the whole committed file; and it is
// the commit the database was synced from, not the one the clone's HEAD has
// moved to while a sync has not committed yet.
func TestARunsBodyIsReadFromTheCommitAndNotTheWorkingTree(t *testing.T) {
	pool := commitReadPool(t)
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "global")
	const whole = "#!/bin/bash\nset -e\nrm -rf \"$STAGING/old\"\necho done\n"
	first := gittest.CommitSynced(t, pool, "global", clone, map[string]string{"scripts/run.sh": whole})
	if _, err := pool.ExecContext(ctx, `INSERT INTO jobs (uid, name, source, repo_id, run_type, script_path, synced_at)
	      VALUES ('j1', 'j1', 'git', 'global', 'bash', 'scripts/run.sh', 't')`); err != nil {
		t.Fatal(err)
	}
	dirs := OneCloneDir(clone)
	body := func() string {
		t.Helper()
		_, b, err := ResolveCommand(ctx, pool, dirs, JobRef{UID: "j1"}, "bash")
		if err != nil {
			t.Fatalf("ResolveCommand: %v", err)
		}
		return b
	}
	file := filepath.Join(clone, "scripts", "run.sh")

	// A sync has truncated the file and not written it yet.
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := body(); got != whole {
		t.Fatalf("with the working-tree file truncated the run is handed %q, want the whole committed file", got)
	}
	// It has written half of it: a script cut off at an arbitrary byte.
	if err := os.WriteFile(file, []byte(whole[:27]), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := body(); got != whole {
		t.Fatalf("with the working-tree file half written the run is handed %q, want the whole committed file", got)
	}
	// It is not in the working tree at all.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if got := body(); got != whole {
		t.Fatalf("with the working-tree file removed the run is handed %q, want the committed file", got)
	}

	// The clone has moved to a new commit and the sync's transaction has not
	// committed: the definitions in the database are still the old commit's,
	// and so is the body.
	const next = "#!/bin/bash\necho the next version\n"
	second := gittest.Commit(t, clone, map[string]string{"scripts/run.sh": next})
	if second == first {
		t.Fatal("the second commit is the first")
	}
	if got := body(); got != whole {
		t.Errorf("before the sync has committed the run is handed %q, want the body of the commit the database was synced from", got)
	}
	gittest.Synced(t, pool, "global", second)
	if got := body(); got != next {
		t.Errorf("after the sync has committed the run is handed %q, want the new commit's body", got)
	}
}

// What cannot be read from the objects is an error with a reason, never a read
// of the working tree, and the reason names no path on the server.
func TestAFileThatIsNotInTheObjectsIsRefusedWithAReason(t *testing.T) {
	pool := commitReadPool(t)
	ctx := context.Background()
	root := t.TempDir()
	clone := filepath.Join(root, "global")

	// The file is in the working tree of a directory that is no repository.
	if err := os.MkdirAll(filepath.Join(clone, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, "scripts", "run.sh"), []byte("echo from the working tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refused := func(what string, data []byte, err error, wants ...string) {
		t.Helper()
		var unread *CommitReadError
		if err == nil || !errors.As(err, &unread) {
			t.Errorf("%s: read %q, err %v; want a refusal with a reason", what, data, err)
			return
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: the reason %q does not say %q", what, err, w)
			}
		}
		if strings.Contains(err.Error(), root) {
			t.Errorf("%s: the reason names a path on the server: %q", what, err)
		}
	}

	// Never synced: there is no commit to read at.
	data, _, err := ReadSyncedFile(ctx, pool, OneCloneDir(clone), "global", "scripts/run.sh", 0)
	refused("a repository that has not been synced", data, err, "global", "has not been synced")

	// A commit is recorded and the directory is not a clone.
	const someCommit = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	data, _, err = ReadCommitFile(clone, someCommit, "scripts/run.sh", 0)
	refused("a directory that is not a clone", data, err, "no clone")

	sha := gittest.Commit(t, clone, map[string]string{"scripts/run.sh": "echo committed\n", "scripts/lib/util.sh": "true\n"})

	// The clone does not hold the commit.
	data, _, err = ReadCommitFile(clone, someCommit, "scripts/run.sh", 0)
	refused("a commit the clone does not hold", data, err, "deadbeefdead", "not in the clone")

	// In the working tree and not in the commit.
	if err := os.WriteFile(filepath.Join(clone, "scripts", "untracked.sh"), []byte("echo untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, _, err = ReadCommitFile(clone, sha, "scripts/untracked.sh", 0)
	refused("a file that is only in the working tree", data, err, "not in commit")

	data, _, err = ReadCommitFile(clone, sha, "scripts/lib", 0)
	refused("a directory", data, err, "a directory")
	data, _, err = ReadCommitFile(clone, sha, "scripts/run.sh/more", 0)
	refused("a path through a file", data, err, "not in commit")
	data, _, err = ReadCommitFile(clone, "main", "scripts/run.sh", 0)
	refused("a ref in place of a commit", data, err, "not a commit")

	for _, unsafe := range []string{"../outside.sh", "scripts/../../outside.sh", "/etc/passwd", ".", ""} {
		data, _, err = ReadCommitFile(clone, sha, unsafe, 0)
		refused("the path "+unsafe, data, err, "unsafe scriptPath")
	}

	// And what is there is read, whole or capped as the display asks.
	if data, truncated, err := ReadCommitFile(clone, sha, "scripts/run.sh", 0); err != nil || string(data) != "echo committed\n" || truncated {
		t.Errorf("the committed file = %q, truncated %v, err %v", data, truncated, err)
	}
	if data, truncated, err := ReadCommitFile(clone, sha, "scripts/run.sh", 4); err != nil || string(data) != "echo" || !truncated {
		t.Errorf("the committed file capped at 4 bytes = %q, truncated %v, err %v; want \"echo\", truncated", data, truncated, err)
	}
	if data, truncated, err := ReadCommitFile(clone, sha, "scripts/run.sh", int64(len("echo committed\n"))); err != nil || string(data) != "echo committed\n" || truncated {
		t.Errorf("a file exactly at the cap = %q, truncated %v, err %v; want it whole and not truncated", data, truncated, err)
	}
}

// A symbolic link committed in the repository was followed by the working-tree
// read while it stayed inside the repository, and refused when it left it. The
// read from the objects keeps both: the link is followed in the commit's tree.
func TestASymbolicLinkIsFollowedInsideTheCommit(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "global")
	gittest.Commit(t, clone, map[string]string{
		"scripts/real.sh":   "echo real\n",
		"lib/shared/run.sh": "echo shared\n",
	})
	repo, err := gogit.PlainOpen(clone)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"scripts/link.sh":     "real.sh",          // a file beside it
		"scripts/shared":      "../lib/shared",    // a directory elsewhere in the repository
		"scripts/chain.sh":    "link.sh",          // a link to a link
		"scripts/outside.sh":  "../../outside.sh", // out of the repository
		"scripts/absolute.sh": "/etc/hostname",    // an absolute path
		"scripts/loop-a.sh":   "loop-b.sh",        // a loop
		"scripts/loop-b.sh":   "loop-a.sh",
		"scripts/dotgit.sh":   "../.git/config",      // the clone's own .git: not in any tree
		"scripts/updir":       "../lib/shared/../..", // back to the root, by way of ..
		"scripts/missing.sh":  "nothing-is-here.sh",  // a dangling link
	} {
		if err := os.Symlink(target, filepath.Join(clone, link)); err != nil {
			t.Fatalf("symlink %s: %v", link, err)
		}
		if _, err := wt.Add(link); err != nil {
			t.Fatalf("add %s: %v", link, err)
		}
	}
	sha := gittest.CommitStaged(t, repo)

	for path, want := range map[string]string{
		"scripts/link.sh":               "echo real\n",
		"scripts/chain.sh":              "echo real\n",
		"scripts/shared/run.sh":         "echo shared\n",
		"scripts/updir/scripts/real.sh": "echo real\n",
	} {
		// The working-tree reader, which this replaces, is the reference.
		was, _, werr := SafeReadRepoFile(clone, path, 0)
		if werr != nil || string(was) != want {
			t.Fatalf("the working-tree read of %s = %q (%v): the fixture is wrong", path, was, werr)
		}
		got, _, err := ReadCommitFile(clone, sha, path, 0)
		if err != nil || string(got) != want {
			t.Errorf("%s read from the commit = %q (%v), want %q as the working-tree read gave", path, got, err, want)
		}
	}
	for path, why := range map[string]string{
		"scripts/outside.sh":  "escapes repository root",
		"scripts/absolute.sh": "escapes repository root",
		"scripts/loop-a.sh":   "too many symbolic links",
		"scripts/dotgit.sh":   "not in commit",
		"scripts/missing.sh":  "not in commit",
	} {
		if was, _, werr := SafeReadRepoFile(clone, path, 0); werr == nil && path != "scripts/absolute.sh" {
			t.Fatalf("the working-tree read of %s gave %q: the fixture is wrong", path, was)
		}
		got, _, err := ReadCommitFile(clone, sha, path, 0)
		var unread *CommitReadError
		if err == nil || !errors.As(err, &unread) || !strings.Contains(err.Error(), why) {
			t.Errorf("%s read from the commit = %q, err %v; want it refused (%s)", path, got, err, why)
		}
	}
}
