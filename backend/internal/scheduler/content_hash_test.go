package scheduler

import (
	"database/sql"
	"testing"
)

// The hash a run records is the hash of what will run. A job built in the app
// runs its SCRIPT as the server last synced it (the owner's decision of
// 2026-10-13), so its run records the script's hash and not the hash on the
// job's own copy; a job from Git, and a job built in the app that names no
// script, record their own.
func TestARunRecordsTheHashOfWhatWillRun(t *testing.T) {
	pool := mustPool(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script, content_hash, synced_at)
	      VALUES ('s-1', 'global', 'task', 'bash', 'echo as synced', 'sha256:the-script', 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, script_ref, script_uid, script, content_hash, concurrency_policy, synced_at) VALUES
	      ('app-job',  'app-job',  'cronomicon', 'bash', 'task', 's-1', 'echo a stale copy', 'sha256:the-job-copy', 'Allow', 't'),
	      ('git-job',  'git-job',  'git',        'bash', 'task', 's-1', 'echo its own copy', 'sha256:the-git-job',  'Allow', 't'),
	      ('own-body', 'own-body', 'cronomicon', 'bash', NULL,   NULL,  'echo mine',         'sha256:its-own',      'Allow', 't')`)
	for job, want := range map[string]string{
		"app-job":  "sha256:the-script",
		"git-job":  "sha256:the-git-job",
		"own-body": "sha256:its-own",
	} {
		var source string
		if err := pool.QueryRow(`SELECT source FROM jobs WHERE uid = ?`, job).Scan(&source); err != nil {
			t.Fatal(err)
		}
		id, err := EnqueueRunWithID(ctxb(), pool, EnqueueParams{
			JobName: job, JobSource: source, JobUID: job, RunType: "bash", TriggerKind: "manual", TriggeredBy: "t",
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", job, err)
		}
		var got sql.NullString
		if err := pool.QueryRow(`SELECT content_hash FROM runs WHERE id = ?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.String != want {
			t.Errorf("the run of %s records the hash %q, want %q", job, got.String, want)
		}
	}
}
