package execspec

import (
	"context"
	"errors"
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
	job("git-script-gone", "git", nil, nil)        // a Git job that records no repository and has no script: Global's, as before
	job("app-script-gone", "cronomicon", nil, nil) // built in the app, and its script row has gone

	for uid, repo := range map[string]string{
		"git-own": "repo-a", "git-borrows": "global",
		"app-on-agency": "repo-a", "app-on-global": "global",
		"own-path": "repo-a", "git-script-gone": "global",
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

	// Present defect 41: a job built in the app whose script has gone has no
	// repository that is its own to read in. It was read in Global's, which
	// holds a file at this path: the job is refused, pinned or not, by uid or
	// by name, and no clone is asked for anything.
	for what, try := range map[string]func() (string, error){
		"by uid": func() (string, error) {
			_, b, err := ResolveCommand(ctx, pool, dirs, JobRef{UID: "app-script-gone"}, Pin{}, "bash")
			return b, err
		},
		"by name": func() (string, error) {
			_, b, err := ResolveCommand(ctx, pool, dirs, JobRef{Name: "app-script-gone", Source: "cronomicon"}, Pin{}, "bash")
			return b, err
		},
		"pinned to Global's commit": func() (string, error) {
			_, b, err := ResolveCommand(ctx, pool, dirs, JobRef{UID: "app-script-gone"},
				Pin{Repo: "global", SHA: synced["global"], Entry: "scripts/run.sh"}, "bash")
			return b, err
		},
	} {
		asked = nil
		body, err := try()
		if !errors.Is(err, ErrScriptGone) || body != "" || len(asked) != 0 {
			t.Errorf("a job built in the app whose script has gone, %s: body %q, err %v, clones asked %v; want ErrScriptGone and nothing read", what, body, err, asked)
		}
	}
	for ref, want := range map[JobRef]bool{
		{UID: "app-script-gone"}:                        true,
		{Name: "app-script-gone", Source: "cronomicon"}: true,
		{UID: "app-on-agency"}:                          false,
		{UID: "git-script-gone"}:                        false,
		{UID: "own-path"}:                               false,
		{UID: "no-such-job"}:                            false,
		{Name: "app-script-gone", Source: "git"}:        false,
	} {
		if got, err := ScriptGone(ctx, pool, ref); err != nil || got != want {
			t.Errorf("ScriptGone(%+v) = %v (%v), want %v", ref, got, err, want)
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
	// It is a job built in the app with no script row: its body is its own
	// copy, nothing is read anywhere, and it is not "its script has gone".
	exec(`INSERT INTO jobs (uid, name, source, run_type, command, synced_at) VALUES ('inline', 'inline', 'cronomicon', 'bash', 'true', 't')`)
	if _, body, err := ResolveCommand(ctx, pool, nil, JobRef{UID: "inline"}, Pin{}, "bash"); err != nil || body != "true" {
		t.Errorf("a body kept on the job's row is %q (%v) for a caller with no clone, want it resolved", body, err)
	}
	if gone, err := ScriptGone(ctx, pool, JobRef{UID: "inline"}); err != nil || gone {
		t.Errorf("ScriptGone for a job whose body is its own copy = %v (%v), want false", gone, err)
	}
}
