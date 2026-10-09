package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1290ScriptIdentity takes a 2.3 database (1280) with scripts, the
// jobs and runs that name them and their reference bindings, and checks what
// 1290 makes of each: a uid per script, Global's repository on every row, the
// uid on the job, the run and the binding, and the two triggers. Then the way
// back.
func TestMigrate1290ScriptIdentity(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "si.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1280); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1280: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	str := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := pool.QueryRow(q, args...).Scan(&s); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return s.String
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return n
	}

	// Two scripts, as 2.3 holds them: the name is the key. One carries every
	// column that was added after the last rebuild (490), and tags, which are
	// the operator's and must survive.
	exec(`INSERT INTO scripts(name, description, run_type, script_path, executor, content_hash, source_path, synced_at,
	                          warnings, variables, tags, project_root, prompts_json)
	      VALUES('deploy.sh','rolls out','bash','scripts/deploy.sh','runner','sha256:d','scripts/deploy.sh','t',
	             '["w"]','["V"]','["blue"]','scripts/app','[{"name":"WHO"}]')`)
	exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at) VALUES('ping','bash','echo hi','sha256:p','t')`)

	// A Git job and an in-app job on deploy.sh; an in-app job whose script was
	// pruned while it kept its copy; a job with no script at all.
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref) VALUES('j-git','roll','git','bash','t','deploy.sh')`)
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref) VALUES('j-app','roll','cronomicon','bash','t','deploy.sh')`)
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, script_ref) VALUES('j-lost','old','cronomicon','bash','t','gone.sh')`)
	exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-inline','inline','git','bash','t')`)

	// Runs: one that named deploy.sh (queued: it needs its bindings at
	// dispatch), one that named a script since pruned, one with no script.
	run := `INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, script_ref) VALUES(?,?,'bash',?,'t','manual','t',?)`
	exec(run, "r-queued", "roll", "queued", "deploy.sh")
	exec(run, "r-lost", "old", "success", "gone.sh")
	exec(run, "r-inline", "inline", "success", nil)

	// Bindings: the script's (name-keyed, no uid), an orphan left by a script
	// that no longer exists, and a job's (uid-keyed since 1050).
	bind := `INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at, owner_uid) VALUES(?,?,?,?,?,'t','t',?)`
	exec(bind, "script", "", "deploy.sh", "secret", "DB_PASSWORD", nil)
	exec(bind, "script", "", "deploy.sh", "var", "REGION", nil)
	exec(bind, "script", "", "gone.sh", "secret", "STALE", nil)
	exec(bind, "job", "git", "roll", "secret", "JOB_ONLY", "j-git")

	if err := m.Migrate(1290); err != nil {
		t.Fatalf("migrate to 1290: %v", err)
	}

	// Every script has a uid and is Global's; nothing else about it moved.
	deploy := str(`SELECT uid FROM scripts WHERE repo_id='global' AND name='deploy.sh'`)
	ping := str(`SELECT uid FROM scripts WHERE repo_id='global' AND name='ping'`)
	if deploy == "" || ping == "" || deploy == ping {
		t.Fatalf("uids after 1290: deploy.sh %q, ping %q; want two distinct uids", deploy, ping)
	}
	if n := count(`SELECT COUNT(*) FROM scripts`); n != 2 {
		t.Fatalf("scripts after 1290 = %d, want 2", n)
	}
	var desc, path, exe, hash, src, warn, vars, tags, root, prompts string
	if err := pool.QueryRow(`SELECT description, script_path, executor, content_hash, source_path, warnings, variables, tags, project_root, prompts_json
	                           FROM scripts WHERE uid=?`, deploy).Scan(&desc, &path, &exe, &hash, &src, &warn, &vars, &tags, &root, &prompts); err != nil {
		t.Fatalf("read deploy.sh: %v", err)
	}
	got := []string{desc, path, exe, hash, src, warn, vars, tags, root, prompts}
	want := []string{"rolls out", "scripts/deploy.sh", "runner", "sha256:d", "scripts/deploy.sh", `["w"]`, `["V"]`, `["blue"]`, "scripts/app", `[{"name":"WHO"}]`}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("deploy.sh column %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Jobs are joined to their script; the job whose script is gone, and the
	// job that never had one, are joined to nothing.
	for uid, wantScript := range map[string]string{"j-git": deploy, "j-app": deploy, "j-lost": "", "j-inline": ""} {
		if got := str(`SELECT script_uid FROM jobs WHERE uid=?`, uid); got != wantScript {
			t.Errorf("jobs.script_uid of %s = %q, want %q", uid, got, wantScript)
		}
	}
	if got := str(`SELECT script_ref FROM jobs WHERE uid='j-lost'`); got != "gone.sh" {
		t.Errorf("the authored name was not kept: script_ref = %q", got)
	}
	// Runs the same.
	for id, wantScript := range map[string]string{"r-queued": deploy, "r-lost": "", "r-inline": ""} {
		if got := str(`SELECT script_uid FROM runs WHERE id=?`, id); got != wantScript {
			t.Errorf("runs.script_uid of %s = %q, want %q", id, got, wantScript)
		}
	}

	// The script's bindings are under its uid, the orphan is gone, the job's
	// row is as it was.
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid=?`, deploy); n != 2 {
		t.Errorf("deploy.sh's bindings under its uid = %d, want 2", n)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid IS NULL`); n != 0 {
		t.Errorf("script bindings with no uid = %d, want 0 (the orphan should have gone)", n)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='job' AND owner_uid='j-git' AND ref_name='JOB_ONLY'`); n != 1 {
		t.Errorf("the job's binding = %d, want 1", n)
	}

	// A hand-written insert, as dozens of fixtures do it, still gets a uid and
	// lands in Global's repository; and a second repository may hold the name.
	exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at) VALUES('by-hand','bash','x','h','t')`)
	if uid, repo := str(`SELECT uid FROM scripts WHERE name='by-hand'`), str(`SELECT repo_id FROM scripts WHERE name='by-hand'`); uid == "" || repo != "global" {
		t.Errorf("a script inserted without a uid: uid %q, repo_id %q; want a uid and 'global'", uid, repo)
	}
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, command, content_hash, synced_at) VALUES('s-other','repo-b','deploy.sh','bash','x','h','t')`)
	if _, err := pool.Exec(`INSERT INTO scripts(repo_id, name, run_type, command, content_hash, synced_at) VALUES('repo-b','deploy.sh','bash','x','h','t')`); err == nil {
		t.Error("two scripts of one name in ONE repository were accepted")
	}
	exec(bind, "script", "", "deploy.sh", "secret", "OTHERS", "s-other")

	// Deleting a script takes ITS bindings and lets go of ITS jobs, and leaves
	// the same-named script of the other repository alone.
	exec(`DELETE FROM scripts WHERE uid=?`, deploy)
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid=?`, deploy); n != 0 {
		t.Errorf("the deleted script still has %d bindings", n)
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid='s-other'`); n != 1 {
		t.Errorf("the other repository's deploy.sh has %d bindings after the first was deleted, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM jobs WHERE script_uid IS NOT NULL`); n != 0 {
		t.Errorf("%d jobs still point at the deleted script", n)
	}
	if got := str(`SELECT script_ref FROM jobs WHERE uid='j-app'`); got != "deploy.sh" {
		t.Errorf("the in-app job lost the name it was written with: %q", got)
	}

	// Back to 1280. Global's script keeps a name another repository also has;
	// here Global's deploy.sh is already deleted, so repo-b's is the one left.
	if err := m.Migrate(1280); err != nil {
		t.Fatalf("migrate down to 1280: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM scripts WHERE name='deploy.sh'`); n != 1 {
		t.Errorf("scripts named deploy.sh after the down = %d, want 1", n)
	}
	for _, col := range []struct{ table, col string }{{"scripts", "uid"}, {"scripts", "repo_id"}, {"jobs", "script_uid"}, {"runs", "script_uid"}} {
		if n := count(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, col.table, col.col); n != 0 {
			t.Errorf("%s.%s is still there after the down", col.table, col.col)
		}
	}
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_uid IS NOT NULL`); n != 0 {
		t.Errorf("script bindings still carry a uid after the down: %d", n)
	}
	// And the old trigger is back, keyed on the name.
	exec(`DELETE FROM scripts WHERE name='deploy.sh'`)
	if n := count(`SELECT COUNT(*) FROM reference_bindings WHERE owner_kind='script' AND owner_name='deploy.sh'`); n != 0 {
		t.Errorf("after the down, deleting a script left %d of its bindings", n)
	}
}
