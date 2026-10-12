package execspec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// GR-22, the dispatch half: a body kept as a file is read from the clone of
// the SCRIPT's repository. Until 2.4.0 there was one clone and both callers
// read Global's, so with a repository per agency a run of an agency's job
// would have been handed whatever Global's clone holds at that path, or
// nothing.
//
// Two repositories hold a script of one name at the SAME path, with different
// bodies. Each job is handed the body of the repository its script is in.
func TestABodyIsReadFromItsScriptsRepository(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// A clone per repository, each with scripts/run.sh committed in it.
	root := t.TempDir()
	clones := map[string]string{"global": filepath.Join(root, "global"), "repo-a": filepath.Join(root, "repo-a")}
	synced := map[string]string{}
	for repo, dir := range clones {
		synced[repo] = gittest.Commit(t, dir, map[string]string{"scripts/run.sh": "echo the body in " + repo + "\n"})
	}
	var asked []string
	dirs := func(repo string) string {
		asked = append(asked, repo)
		return clones[repo]
	}

	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`)
	for repo, sha := range synced {
		gittest.Synced(t, pool, repo, sha)
	}
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES
	      ('s-g', 'global', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't'),
	      ('s-a', 'repo-a', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't')`)
	job := func(uid, source string, repo, script any) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, script_uid, script_path, synced_at)
		      VALUES (?, ?, ?, ?, 'bash', ?, 'scripts/run.sh', 't')`, uid, uid, source, repo, script)
	}
	job("git-own", "git", "repo-a", "s-a")         // an agency's job on its own repository's script
	job("git-borrows", "git", "repo-a", "s-g")     // an agency's job on Global's script (GR-16)
	job("app-on-agency", "cronomicon", nil, "s-a") // built in the app, on its agency's repository's script
	job("app-on-global", "cronomicon", nil, "s-g") // built in the app, on Global's
	job("own-path", "git", "repo-a", nil)          // a Git job with a path of its own: its own repository
	job("app-script-gone", "cronomicon", nil, nil) // its script row has gone: Global's, as before

	for uid, repo := range map[string]string{
		"git-own": "repo-a", "git-borrows": "global",
		"app-on-agency": "repo-a", "app-on-global": "global",
		"own-path": "repo-a", "app-script-gone": "global",
	} {
		asked = nil
		_, body, err := ResolveCommand(ctx, pool, dirs, JobRef{UID: uid}, Pin{}, "bash")
		if err != nil {
			t.Errorf("ResolveCommand(%s): %v", uid, err)
			continue
		}
		if want := "echo the body in " + repo + "\n"; body != want {
			t.Errorf("a run of %s is handed %q, want the body in the repository %s", uid, strings.TrimSpace(body), repo)
		}
		if len(asked) != 1 || asked[0] != repo {
			t.Errorf("a run of %s read the clone of %v, want the one clone of %s", uid, asked, repo)
		}
	}

	// A run with no uid is read in its script's repository too.
	if _, body, err := ResolveCommand(ctx, pool, dirs, JobRef{Name: "git-own", Source: "git"}, Pin{}, "bash"); err != nil ||
		body != "echo the body in repo-a\n" {
		t.Errorf("a run of git-own with no uid is handed %q (%v), want the body in repo-a", strings.TrimSpace(body), err)
	}

	// A caller with no clone cannot run a body kept in a file, and says so; a
	// body kept on the job's row needs no clone.
	if _, _, err := ResolveCommand(ctx, pool, nil, JobRef{UID: "git-own"}, Pin{}, "bash"); err == nil {
		t.Errorf("a body kept in a file was resolved by a caller that has no clone")
	}
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, synced_at) VALUES ('inline', 'inline', 'cronomicon', 'bash', 'true', 't')`)
	if _, body, err := ResolveCommand(ctx, pool, nil, JobRef{UID: "inline"}, Pin{}, "bash"); err != nil || body != "true" {
		t.Errorf("a body kept on the job's row is %q (%v) for a caller with no clone, want it resolved", body, err)
	}
}
