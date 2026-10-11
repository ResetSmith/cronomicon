package scheduler

import (
	"context"
	"database/sql"
	"testing"
)

// GR-22: a checkout run is pinned, at enqueue, to the repository its tree is
// fetched from and that repository's last synced commit; and the repository is
// the SCRIPT's. Until 2.4.0 there was one repository: the commit was always
// Global's and the run recorded no repository.
func TestACheckoutRunIsPinnedToItsScriptsRepository(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const (
		shaG = "1111111111111111111111111111111111111111"
		shaA = "2222222222222222222222222222222222222222"
	)
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-a', 'A', 't'), ('ag-n', 'N', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch, last_sha) VALUES
	      ('repo-a', 'ag-a', 'u', 'main', ?), ('repo-never', 'ag-n', 'u', 'main', NULL)`, shaA)
	exec(`UPDATE git_repos SET last_sha = ? WHERE id = 'global'`, shaG)
	// A project in each repository, at the SAME path.
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, project_root, content_hash, synced_at) VALUES
	      ('s-g', 'global',     'proj', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'h', 't'),
	      ('s-a', 'repo-a',     'proj', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'h', 't'),
	      ('s-n', 'repo-never', 'proj', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'h', 't')`)
	job := func(uid, source string, repo, script any, projectRoot any) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, script_ref, script_uid, script_path, project_root,
		                        concurrency_policy, synced_at)
		      VALUES (?, ?, ?, ?, 'ansible', 'proj', ?, 'scripts/proj/site.yml', ?, 'Allow', 't')`,
			uid, uid, source, repo, script, projectRoot)
	}
	job("git-own", "git", "repo-a", "s-a", "scripts/proj")         // an agency's job on its own repository's project
	job("git-borrows", "git", "repo-a", "s-g", "scripts/proj")     // an agency's job on Global's project (GR-16)
	job("app-on-agency", "cronomicon", nil, "s-a", "scripts/proj") // built in the app, on its agency's repository's project
	job("app-on-global", "cronomicon", nil, "s-g", "scripts/proj") // built in the app, on Global's
	job("never-synced", "git", "repo-never", "s-n", "scripts/proj")
	job("git-script-gone", "git", "repo-a", nil, "scripts/proj")   // its script row has gone
	job("app-script-gone", "cronomicon", nil, nil, "scripts/proj") // the same, built in the app
	job("not-a-project", "git", "repo-a", "s-a", nil)              // a body job: no pin at all

	for _, c := range []struct{ job, repo, sha string }{
		{"git-own", "repo-a", shaA},
		{"git-borrows", "global", shaG},
		{"app-on-agency", "repo-a", shaA},
		{"app-on-global", "global", shaG},
		{"never-synced", "repo-never", ""},
		{"git-script-gone", "repo-a", shaA},
		{"app-script-gone", "global", shaG},
		{"not-a-project", "", ""},
	} {
		var source string
		if err := pool.QueryRowContext(ctx, `SELECT source FROM jobs WHERE uid = ?`, c.job).Scan(&source); err != nil {
			t.Fatal(err)
		}
		id, err := EnqueueRunWithID(ctx, pool, EnqueueParams{
			JobName: c.job, JobSource: source, JobUID: c.job, RunType: "ansible", TriggerKind: "manual", TriggeredBy: "t",
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", c.job, err)
		}
		var repo, sha, entry sql.NullString
		if err := pool.QueryRowContext(ctx,
			`SELECT checkout_repo, checkout_sha, checkout_entry FROM runs WHERE id = ?`, id).Scan(&repo, &sha, &entry); err != nil {
			t.Fatalf("read the run of %s: %v", c.job, err)
		}
		if repo.String != c.repo || sha.String != c.sha {
			t.Errorf("the run of %s is pinned to (%q, %q), want (%q, %q)", c.job, repo.String, sha.String, c.repo, c.sha)
		}
		wantEntry := "scripts/proj/site.yml"
		if c.job == "not-a-project" {
			wantEntry = ""
		}
		if entry.String != wantEntry {
			t.Errorf("the run of %s has the entry %q, want %q", c.job, entry.String, wantEntry)
		}
	}
}
