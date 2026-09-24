package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDefinitionSchedulesCascadeDelete verifies the migration-090 triggers
// remove a definition's schedule rows when the parent job/workflow row is
// deleted (SQLite has no polymorphic FK), without touching other definitions.
func TestDefinitionSchedulesCascadeDelete(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "casc.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	mustExec := func(q string, args ...any) {
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	count := func(kind, owner string) int {
		var n int
		_ = pool.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind=? AND owner_name=?`, kind, owner).Scan(&n)
		return n
	}

	// R2-5: the cascade key is the uid — every real writer stamps it, so the
	// fixture does too (a row seeded without one is no longer trigger-reachable,
	// by design; see cascade_uid_test.go).
	mustExec(`INSERT INTO jobs (uid, name, run_type, synced_at) VALUES ('uid-j1','j1','bash','t')`)
	mustExec(`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position, owner_uid) VALUES ('job','j1','default','0 2 * * *',0,'uid-j1')`)
	mustExec(`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position, owner_uid) VALUES ('job','j1','extra','0 */6 * * *',1,'uid-j1')`)
	// A second job whose schedule must survive j1's deletion.
	mustExec(`INSERT INTO jobs (uid, name, run_type, synced_at) VALUES ('uid-j2','j2','bash','t')`)
	mustExec(`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position, owner_uid) VALUES ('job','j2','default','0 3 * * *',0,'uid-j2')`)
	mustExec(`INSERT INTO workflows (uid, name, synced_at) VALUES ('uid-w1','w1','t')`)
	mustExec(`INSERT INTO definition_schedules (owner_kind, owner_name, name, cron, position, owner_uid) VALUES ('workflow','w1','default','0 1 * * *',0,'uid-w1')`)

	if count("job", "j1") != 2 || count("workflow", "w1") != 1 {
		t.Fatalf("setup: expected seeded child rows (j1=%d, w1=%d)", count("job", "j1"), count("workflow", "w1"))
	}

	mustExec(`DELETE FROM jobs WHERE name='j1'`)
	if got := count("job", "j1"); got != 0 {
		t.Errorf("job cascade: %d child rows remain, want 0", got)
	}
	if got := count("job", "j2"); got != 1 {
		t.Errorf("unrelated job's schedule was deleted (%d), want 1", got)
	}

	mustExec(`DELETE FROM workflows WHERE name='w1'`)
	if got := count("workflow", "w1"); got != 0 {
		t.Errorf("workflow cascade: %d child rows remain, want 0", got)
	}
}

// TestPausedJobsCascadeDelete closes the coverage gap §7.1 names: the
// definition_schedules pair above was tested, its paused_jobs sibling was not.
//
// That asymmetry mattered because of what the missing cascade DOES. A paused
// definition that is deleted — pruned from git, or deleted in-app — leaves its
// paused_jobs row behind; when a definition of the same name later returns, the
// scheduler finds isPaused()=true and SILENTLY SKIPS EVERY FIRE. Nothing errors,
// nothing logs, the job simply never runs. Migration 200 added the triggers to
// close it, and its own header warns that any future table rebuild of jobs,
// workflows or paused_jobs silently DROPS them — which is exactly the regression
// nothing here would have caught.
//
// Both the source-qualification and the isolation assertions are load-bearing:
// paused_jobs keys on `source` where definition_schedules keys on `owner_source`
// (migration 200 flags that trap explicitly), so a trigger written against the
// wrong column name would either fail to fire or delete another source's pause.
func TestPausedJobsCascadeDelete(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "paused.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	paused := func(kind, source, name string) int {
		var n int
		_ = pool.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM paused_jobs WHERE owner_kind=? AND source=? AND name=?`,
			kind, source, name).Scan(&n)
		return n
	}

	// The same NAME under both sources: the git and amadeus namespaces are
	// deliberately disjoint (PRIMARY KEY (source, name)), so deleting one must not
	// disturb the other's pause.
	mustExec(`INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('uid-dep-git','deploy','git','bash','t')`)
	mustExec(`INSERT INTO jobs (uid, name, source, run_type, synced_at) VALUES ('uid-dep-ama','deploy','amadeus','bash','t')`)
	mustExec(`INSERT INTO workflows (uid, name, source, synced_at) VALUES ('uid-ngt','nightly','git','t')`)
	mustExec(`INSERT INTO paused_jobs (owner_kind, source, name, paused_at, paused_by, owner_uid) VALUES ('job','git','deploy','t','tester','uid-dep-git')`)
	mustExec(`INSERT INTO paused_jobs (owner_kind, source, name, paused_at, paused_by, owner_uid) VALUES ('job','amadeus','deploy','t','tester','uid-dep-ama')`)
	mustExec(`INSERT INTO paused_jobs (owner_kind, source, name, paused_at, paused_by, owner_uid) VALUES ('workflow','git','nightly','t','tester','uid-ngt')`)

	if paused("job", "git", "deploy") != 1 || paused("job", "amadeus", "deploy") != 1 || paused("workflow", "git", "nightly") != 1 {
		t.Fatal("setup: expected three seeded pause rows")
	}

	mustExec(`DELETE FROM jobs WHERE source='git' AND name='deploy'`)
	if got := paused("job", "git", "deploy"); got != 0 {
		t.Errorf("job pause survived the delete (%d rows): a same-named job returning later would be silently never fired", got)
	}
	if got := paused("job", "amadeus", "deploy"); got != 1 {
		t.Errorf("the amadeus-source pause was collateral (%d rows, want 1): the trigger is not source-qualified", got)
	}

	mustExec(`DELETE FROM workflows WHERE source='git' AND name='nightly'`)
	if got := paused("workflow", "git", "nightly"); got != 0 {
		t.Errorf("workflow pause survived the delete (%d rows), want 0", got)
	}
	// The job pause for a DIFFERENT owner_kind must be untouched by the workflow
	// delete — the two triggers differ only in owner_kind, so a copy-paste slip
	// between them shows up here.
	if got := paused("job", "amadeus", "deploy"); got != 1 {
		t.Errorf("the workflow delete removed a JOB pause (%d rows, want 1): owner_kind is not being matched", got)
	}
}
