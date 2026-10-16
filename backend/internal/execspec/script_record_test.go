package execspec

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// TestAnInAppJobRunsItsScriptAsLastSynced (the owner's decision of 2026-10-13
// on present defect 45, "option B").
//
// A job built in the app holds a copy of its script, which sync refreshes. The
// copy was what ran. A run now reads the SCRIPT's record: the body is the
// script's as the server last synced it, whatever the job's copy says, and a
// job whose script has gone does not run, whatever kind its body was. A job
// from Git, and a job built in the app that names no script and holds its own
// inline body (from before scripts were what a job is built on), run what
// their own row holds, as before.
func TestAnInAppJobRunsItsScriptAsLastSynced(t *testing.T) {
	pool := commitReadPool(t)
	ctx := context.Background()
	clone := filepath.Join(t.TempDir(), "global")
	gittest.CommitSynced(t, pool, "global", clone, map[string]string{
		"scripts/new.sh": "echo the file the script names now\n",
		"scripts/old.sh": "echo the file the job's copy still names\n",
	})
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script, script_path, content_hash, synced_at) VALUES
	      ('s-inline', 'global', 'inline', 'bash', 'echo as synced', NULL,             'h', 't'),
	      ('s-file',   'global', 'file',   'bash', NULL,             'scripts/new.sh', 'h', 't'),
	      ('s-gone-1', 'global', 'gone-1', 'bash', 'echo x',         NULL,             'h', 't'),
	      ('s-gone-2', 'global', 'gone-2', 'bash', NULL,             'scripts/new.sh', 'h', 't')`)
	// Each job's own copy is deliberately NOT what its script holds.
	exec(`INSERT INTO jobs (uid, name, source, run_type, script_ref, script_uid, script, script_path, synced_at) VALUES
	      ('app-inline',      'app-inline',      'cronomicon', 'bash', 'inline', 's-inline', 'echo the stale copy', NULL,             't'),
	      ('app-file',        'app-file',        'cronomicon', 'bash', 'file',   's-file',   NULL,                  'scripts/old.sh', 't'),
	      ('app-gone-inline', 'app-gone-inline', 'cronomicon', 'bash', 'gone-1', 's-gone-1', 'echo the stale copy', NULL,             't'),
	      ('app-gone-file',   'app-gone-file',   'cronomicon', 'bash', 'gone-2', 's-gone-2', NULL,                  'scripts/old.sh', 't'),
	      ('app-own',         'app-own',         'cronomicon', 'bash', NULL,     NULL,       'echo its own body',   NULL,             't'),
	      ('git-inline',      'git-inline',      'git',        'bash', 'inline', 's-inline', 'echo the git job''s own copy', NULL,    't')`)
	exec(`UPDATE jobs SET repo_id = 'global' WHERE uid = 'git-inline'`)
	dirs := OneCloneDir(clone)
	body := func(ref JobRef) (string, error) {
		t.Helper()
		_, b, err := ResolveCommand(ctx, pool, dirs, ref, Pin{}, "bash")
		return b, err
	}

	for uid, want := range map[string]string{
		"app-inline": "echo as synced",
		"app-file":   "echo the file the script names now\n",
		"app-own":    "echo its own body",
		"git-inline": "echo the git job's own copy",
	} {
		if got, err := body(JobRef{UID: uid}); err != nil || got != want {
			t.Errorf("a run of %s is handed %q (%v), want %q", uid, got, err, want)
		}
	}
	// By name too, for a run that records no uid.
	if got, err := body(JobRef{Name: "app-inline", Source: "cronomicon"}); err != nil || got != "echo as synced" {
		t.Errorf("a run of app-inline with no uid is handed %q (%v), want its script's body", got, err)
	}

	// The scripts are removed (the trigger clears the jobs' script_uid). An
	// inline copy does not run any more than a file's path does.
	exec(`DELETE FROM scripts WHERE uid IN ('s-gone-1', 's-gone-2')`)
	for _, uid := range []string{"app-gone-inline", "app-gone-file"} {
		got, err := body(JobRef{UID: uid})
		if !errors.Is(err, ErrScriptGone) || got != "" {
			t.Errorf("a run of %s, whose script has gone, is handed %q (%v), want ErrScriptGone", uid, got, err)
		}
		if gone, err := ScriptGone(ctx, pool, JobRef{UID: uid}); err != nil || !gone {
			t.Errorf("ScriptGone(%s) = %v (%v), want true", uid, gone, err)
		}
	}
	// What is not "its script has gone": a job that still has its script, one
	// that never named one, and a Git job whose script is removed (it runs its
	// own copy, which its own repository's sync writes).
	exec(`DELETE FROM scripts WHERE uid = 's-inline'`)
	if got, err := body(JobRef{UID: "git-inline"}); err != nil || got != "echo the git job's own copy" {
		t.Errorf("a Git job whose script row has gone is handed %q (%v), want its own copy", got, err)
	}
	for uid, want := range map[string]bool{"app-file": false, "app-own": false, "git-inline": false, "app-inline": true} {
		if gone, err := ScriptGone(ctx, pool, JobRef{UID: uid}); err != nil || gone != want {
			t.Errorf("ScriptGone(%s) = %v (%v), want %v", uid, gone, err, want)
		}
	}
}
