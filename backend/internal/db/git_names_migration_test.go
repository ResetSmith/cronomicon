package db

import (
	"strings"
	"testing"
)

// TestMigrate1320GitNamesPerRepo: after 1320 a Git job's and a Git workflow's
// name is unique within its repository, where it was unique across all of
// them, and a log-folder code records the repository it was allocated for.
// Then the way back, which can keep only one definition of a name.
func TestMigrate1320GitNamesPerRepo(t *testing.T) {
	h := openAt(t, 1310)
	h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-g', 'nightly', 'git', 'bash', 't', 'global')`)
	h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-app', 'nightly', 'cronomicon', 'bash', 't')`)
	h.exec(`INSERT INTO workflows(uid, name, source, steps, synced_at, repo_id) VALUES('w-g', 'flow', 'git', '[]', 't', 'global')`)
	// A Git row with no repository: what an older binary left running through
	// the upgrade to 1310 would have written.
	h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-late', 'late', 'git', 'bash', 't')`)
	h.exec(`INSERT INTO entity_codes(kind, source, name, created_at, uid) VALUES('job', 'git', 'nightly', 't', 'j-g')`)
	h.exec(`INSERT INTO entity_codes(kind, source, name, created_at, uid) VALUES('job', 'cronomicon', 'nightly', 't', 'j-app')`)
	// Before: a second Git job of the name is refused, whatever its repository.
	if _, err := h.pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-b', 'nightly', 'git', 'bash', 't', 'repo-b')`); err == nil {
		t.Fatalf("at 1310 a second Git job named nightly was accepted")
	}

	h.to(1320)

	if got := h.str(`SELECT repo_id FROM jobs WHERE uid='j-late'`); got != "global" {
		t.Errorf("the Git job that had no repository has %q, want global", got)
	}
	if got := h.str(`SELECT COALESCE(repo_id,'(none)') FROM jobs WHERE uid='j-app'`); got != "(none)" {
		t.Errorf("the job built in the app has repo_id %q, want none", got)
	}
	if got := h.str(`SELECT COALESCE(repo_id,'(none)') FROM entity_codes WHERE uid='j-g'`); got != "global" {
		t.Errorf("the Git job's log-folder code has repo_id %q, want global", got)
	}
	if got := h.str(`SELECT COALESCE(repo_id,'(none)') FROM entity_codes WHERE uid='j-app'`); got != "(none)" {
		t.Errorf("the in-app job's log-folder code has repo_id %q, want none", got)
	}
	// Another repository may have the name; the same repository may not twice.
	h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-b', 'nightly', 'git', 'bash', 't', 'repo-b')`)
	h.exec(`INSERT INTO workflows(uid, name, source, steps, synced_at, repo_id) VALUES('w-b', 'flow', 'git', '[]', 't', 'repo-b')`)
	for _, q := range []string{
		`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-g2', 'nightly', 'git', 'bash', 't', 'global')`,
		`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-b2', 'nightly', 'git', 'bash', 't', 'repo-b')`,
		`INSERT INTO workflows(uid, name, source, steps, synced_at, repo_id) VALUES('w-b2', 'flow', 'git', '[]', 't', 'repo-b')`,
	} {
		if _, err := h.pool.Exec(q); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
			t.Errorf("a second definition of a name in ONE repository: %v, want a uniqueness refusal\n%s", err, q)
		}
	}
	// A name is no concern of these indexes for a job built in the app.
	h.exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES('j-app2', 'nightly', 'cronomicon', 'bash', 't')`)

	// What hangs off the second repository's job, to see it go with the job.
	h.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, owner_uid)
	        VALUES('git', 'job', 'nightly', 'default', '0 0 1 * * *', 0, 'j-b')`)
	h.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, owner_uid)
	        VALUES('git', 'job', 'nightly', 'default', '0 0 2 * * *', 0, 'j-g')`)

	// And its log-folder code, which no trigger retires.
	h.exec(`INSERT INTO entity_codes(kind, source, name, created_at, uid, repo_id) VALUES('job', 'git', 'nightly', 't', 'j-b', 'repo-b')`)

	// The way back: one Git definition of a name. Global's is kept.
	h.to(1310)
	if n := h.count(`SELECT COUNT(*) FROM entity_codes WHERE kind='job' AND source='git' AND name='nightly' AND deleted_at IS NULL`); n != 1 {
		t.Errorf("live log-folder codes of the Git job nightly after the way back = %d, want 1 (the deleted job's is retired)", n)
	}
	if got := h.str(`SELECT uid FROM entity_codes WHERE kind='job' AND source='git' AND name='nightly' AND deleted_at IS NULL`); got != "j-g" {
		t.Errorf("the live log-folder code after the way back is %q's, want Global's job's", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM entity_codes WHERE uid='j-b' AND deleted_at IS NOT NULL`); n != 1 {
		t.Errorf("the deleted job's log-folder code is not retired (%d rows)", n)
	}
	if got := h.str(`SELECT GROUP_CONCAT(uid, ',') FROM (SELECT uid FROM jobs WHERE source='git' AND name='nightly' ORDER BY uid)`); got != "j-g" {
		t.Errorf("Git jobs named nightly after the way back = %q, want Global's alone", got)
	}
	if got := h.str(`SELECT GROUP_CONCAT(uid, ',') FROM (SELECT uid FROM workflows WHERE source='git' AND name='flow' ORDER BY uid)`); got != "w-g" {
		t.Errorf("Git workflows named flow after the way back = %q, want Global's alone", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name='nightly'`); n != 2 {
		t.Errorf("in-app jobs named nightly after the way back = %d, want both", n)
	}
	if got := h.str(`SELECT GROUP_CONCAT(owner_uid, ',') FROM definition_schedules WHERE owner_name='nightly'`); got != "j-g" {
		t.Errorf("schedule entries after the way back belong to %q, want Global's job's alone", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM pragma_table_info('entity_codes') WHERE name='repo_id'`); n != 0 {
		t.Errorf("entity_codes.repo_id is still there after the way back")
	}
	if _, err := h.pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, synced_at, repo_id) VALUES('j-b3', 'nightly', 'git', 'bash', 't', 'repo-b')`); err == nil {
		t.Errorf("after the way back a second Git job named nightly was accepted")
	}
	h.to(1320)
}
