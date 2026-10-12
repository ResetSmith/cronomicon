package execspec

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// TestAPinnedRunIsReadAtItsOwnCommit (present defect 23, the entry's half; in
// the released code). A checkout run is pinned, when it is queued, to a
// repository, a commit and an entry: what the agent will check out. Its body
// was read from the working tree all the same, so a later sync that removed or
// moved the entry left the run with no body, and the manifest answered 500
// although the pinned tree was intact.
//
// A pinned run is read at its pin. A run that is not pinned is read at the last
// synced commit, as before.
func TestAPinnedRunIsReadAtItsOwnCommit(t *testing.T) {
	pool := commitReadPool(t)
	ctx := context.Background()
	root := t.TempDir()
	clones := map[string]string{"global": filepath.Join(root, "global"), "repo-a": filepath.Join(root, "repo-a")}

	// Global's repository at two commits: the entry is in the first and has
	// moved in the second. An agency's repository holds the same path.
	first := gittest.CommitObjects(t, clones["global"], map[string]string{"scripts/proj/site.yml": "- hosts: all # first\n"}, nil)
	second := gittest.CommitObjects(t, clones["global"], map[string]string{"scripts/proj/main.yml": "- hosts: all # second\n"}, nil)
	agency := gittest.CommitObjects(t, clones["repo-a"], map[string]string{"scripts/proj/site.yml": "- hosts: all # the agency's\n"}, nil)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't')`,
		`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`,
		// The job as the second sync left it: its entry is the new file.
		`INSERT INTO jobs (uid, name, source, repo_id, run_type, script_path, project_root, synced_at)
		 VALUES ('proj', 'proj', 'git', 'global', 'ansible', 'scripts/proj/main.yml', 'scripts/proj', 't')`,
	} {
		if _, err := pool.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	gittest.Synced(t, pool, "global", second)
	gittest.Synced(t, pool, "repo-a", agency)
	var asked []string
	dirs := func(repo string) string {
		asked = append(asked, repo)
		return clones[repo]
	}
	resolve := func(at Pin) (string, error) {
		t.Helper()
		asked = nil
		_, body, err := ResolveCommand(ctx, pool, dirs, JobRef{UID: "proj"}, at, "ansible")
		return body, err
	}

	// Queued before the second sync: its entry is read at its commit.
	if body, err := resolve(Pin{Repo: "global", SHA: first, Entry: "scripts/proj/site.yml"}); err != nil || body != "- hosts: all # first\n" {
		t.Errorf("a run pinned before the entry moved is handed %q (%v), want the entry at its pinned commit", body, err)
	}
	// Queued after it.
	if body, err := resolve(Pin{Repo: "global", SHA: second, Entry: "scripts/proj/main.yml"}); err != nil || body != "- hosts: all # second\n" {
		t.Errorf("a run pinned after the entry moved is handed %q (%v)", body, err)
	}
	// A run from before 2.4.0 records no repository: Global's.
	if body, err := resolve(Pin{SHA: first, Entry: "scripts/proj/site.yml"}); err != nil || body != "- hosts: all # first\n" || len(asked) != 1 || asked[0] != "global" {
		t.Errorf("a pin that records no repository is handed %q (%v) from %v, want Global's", body, err, asked)
	}
	// The pin's repository is read, and not the job's.
	if body, err := resolve(Pin{Repo: "repo-a", SHA: agency, Entry: "scripts/proj/site.yml"}); err != nil || body != "- hosts: all # the agency's\n" || len(asked) != 1 || asked[0] != "repo-a" {
		t.Errorf("a run pinned to an agency's repository is handed %q (%v) from %v, want that repository's entry", body, err, asked)
	}
	// A pin that records no entry is the job's path, at the pinned commit.
	if body, err := resolve(Pin{Repo: "global", SHA: second}); err != nil || body != "- hosts: all # second\n" {
		t.Errorf("a pin that records no entry is handed %q (%v), want the job's path at the pinned commit", body, err)
	}
	// An entry the pinned commit does not hold is refused with a reason, and
	// the last synced commit is not read in its place.
	body, err := resolve(Pin{Repo: "global", SHA: first, Entry: "scripts/proj/main.yml"})
	var unread *CommitReadError
	if err == nil || !errors.As(err, &unread) || !strings.Contains(err.Error(), "not in commit "+first[:12]) {
		t.Errorf("an entry its pinned commit does not hold is handed %q, err %v; want it refused at that commit", body, err)
	}
	// Not pinned: the last synced commit.
	if body, err := resolve(Pin{}); err != nil || body != "- hosts: all # second\n" {
		t.Errorf("a run that is not pinned is handed %q (%v), want the job's path at the last synced commit", body, err)
	}
}

// The job's half of present defect 23: a run whose job a later sync removed is
// told so. It was "sql: no rows in result set", which the manifest answered as
// a 500.
func TestARunWhoseJobHasGoneIsToldSo(t *testing.T) {
	pool := commitReadPool(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx, `INSERT INTO jobs (uid, name, source, run_type, command, synced_at)
	      VALUES ('here', 'here', 'cronomicon', 'bash', 'true', 't')`); err != nil {
		t.Fatal(err)
	}
	for what, job := range map[string]JobRef{
		"a uid that no job has":                 {UID: "gone", Name: "here", Source: "cronomicon"},
		"a name that no job has, with no uid":   {Name: "gone", Source: "cronomicon"},
		"a name of another source, with no uid": {Name: "here", Source: "git"},
	} {
		_, body, err := ResolveCommand(ctx, pool, nil, job, Pin{}, "bash")
		if !errors.Is(err, ErrJobGone) {
			t.Errorf("%s: body %q, err %v; want ErrJobGone", what, body, err)
		}
	}
	if _, body, err := ResolveCommand(ctx, pool, nil, JobRef{UID: "here"}, Pin{}, "bash"); err != nil || body != "true" {
		t.Errorf("the job that is there = %q (%v)", body, err)
	}
	// Two jobs of the name and no uid is not "gone": it is the refusal to pick.
	if _, err := pool.ExecContext(ctx, `INSERT INTO jobs (uid, name, source, run_type, command, synced_at)
	      VALUES ('here-2', 'here', 'cronomicon', 'bash', 'false', 't')`); err != nil {
		t.Fatal(err)
	}
	var ambiguous *AmbiguousJobError
	if _, _, err := ResolveCommand(ctx, pool, nil, JobRef{Name: "here", Source: "cronomicon"}, Pin{}, "bash"); !errors.As(err, &ambiguous) || errors.Is(err, ErrJobGone) {
		t.Errorf("two jobs of the name and no uid: err %v, want the refusal to pick and not ErrJobGone", err)
	}
}
