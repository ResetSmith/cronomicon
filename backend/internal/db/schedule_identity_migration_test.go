package db

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrate1300ScheduleIdentity takes a database at 1290 holding the shapes
// a real one has (a name shared by Git's schedule and an in-app one, entries
// written while it was shared, the leftovers of a recycle-bin restore) and
// checks what 1300 makes of each. Then the way back.
func TestMigrate1300ScheduleIdentity(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "schid.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1290); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1290: %v", err)
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

	// `nightly` twice (Git's and an in-app one), `solo` once, and a binned
	// in-app schedule with every optional column filled.
	sched := `INSERT INTO schedules(uid, name, source, cron, content_hash) VALUES(?,?,?,?,'h')`
	exec(sched, "s-git", "nightly", "git", "0 0 1 * * *")
	exec(sched, "s-app", "nightly", "cronomicon", "0 0 3 * * *")
	exec(sched, "s-solo", "solo", "git", "0 0 4 * * *")
	exec(`INSERT INTO schedules(uid, name, source, description, cron, env, content_hash, source_path, synced_at, created_by, created_at,
	                            last_modified_by, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars, deleted_at, deleted_by)
	      VALUES('s-old','old','cronomicon','d','0 0 5 * * *','{"A":"1"}','h2','p','sy','cb','ca','mb','ma','["t"]','sa','ea','7d','["hol"]','["work"]','da','db')`)

	job := `INSERT INTO jobs(uid, name, source, run_type, synced_at) VALUES(?,?,?,'bash','t')`
	exec(job, "j-g1", "g1", "git")
	exec(job, "j-a1", "a1", "cronomicon")
	exec(job, "j-a2", "a2", "cronomicon")

	entry := `INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	          VALUES(?, 'job', ?, ?, ?, ?, ?, ?, ?)`
	// (a) a Git job's entry written while the name was shared: no schedule uid.
	exec(entry, "git", "g1", "nightly", "0 0 1 * * *", 0, "nightly", "j-g1", nil)
	// (b) an in-app job's entry written while the name was shared: ambiguous.
	exec(entry, "cronomicon", "a1", "nightly", "0 0 3 * * *", 0, "nightly", "j-a1", nil)
	// (c) an in-app job's entry on a name only one schedule holds.
	exec(entry, "cronomicon", "a1", "solo", "0 0 4 * * *", 1, "solo", "j-a1", nil)
	// (d) an entry that already knows its schedule keeps it.
	exec(entry, "cronomicon", "a2", "nightly", "0 0 3 * * *", 0, "nightly", "j-a2", "s-app")
	// (e) an inline entry (no source_ref) has no schedule.
	exec(entry, "cronomicon", "a2", "inline", "0 0 6 * * *", 1, nil, "j-a2", nil)
	// (f) what a recycle-bin restore left: neither uid.
	exec(entry, "cronomicon", "a2", "solo", "0 0 4 * * *", 2, "solo", nil, nil)
	// (g) a restore's row that a later save duplicated: the uid-less twin of (d).
	exec(entry, "cronomicon", "a2", "nightly", "0 0 3 * * *", 0, "nightly", nil, nil)
	// (h) two uid-less twins with no uid'd one (two restores).
	exec(entry, "cronomicon", "a1", "extra", "0 0 7 * * *", 2, "solo", nil, nil)
	exec(entry, "cronomicon", "a1", "extra", "0 0 7 * * *", 2, "solo", nil, nil)
	// (i) an entry whose uid names a schedule that has gone (its Git schedule
	// was pruned), on a name another schedule holds now.
	exec(job, "j-a3", "a3", "cronomicon")
	exec(entry, "cronomicon", "a3", "solo", "0 0 4 * * *", 0, "solo", "j-a3", "s-dead")
	// (j) two agencies' in-app jobs of ONE name. What a restore left under that
	// name (two uid-less rows, one per job) and what a later save of ONE of the
	// jobs added (a row with its uid). The uid-less rows are not that job's
	// stale copies: one of them is the other job's only entry.
	exec(job, "j-t1", "twin", "cronomicon")
	exec(job, "j-t2", "twin", "cronomicon")
	exec(entry, "cronomicon", "twin", "solo", "0 0 4 * * *", 0, "solo", nil, nil)
	exec(entry, "cronomicon", "twin", "solo", "0 0 4 * * *", 0, "solo", nil, nil)
	exec(entry, "cronomicon", "twin", "solo", "0 0 4 * * *", 0, "solo", "j-t1", "s-solo")
	// (k) a workflow's entry, as a restore left it.
	exec(`INSERT INTO workflows(uid, name, source, steps, synced_at) VALUES('w-1','flow','cronomicon','[]','t')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	      VALUES('cronomicon', 'workflow', 'flow', 'solo', '0 0 4 * * *', 0, 'solo', NULL, NULL)`)

	if err := m.Migrate(1300); err != nil {
		t.Fatalf("migrate to 1300: %v", err)
	}

	// Schedules: every one is Global's; a Git one has Global's repository, an
	// in-app one has none; nothing else moved.
	for uid, repo := range map[string]string{"s-git": "global", "s-solo": "global", "s-app": "", "s-old": ""} {
		if got := str(`SELECT owner_agency FROM schedules WHERE uid=?`, uid); got != "global" {
			t.Errorf("%s owner_agency = %q, want global", uid, got)
		}
		if got := str(`SELECT repo_id FROM schedules WHERE uid=?`, uid); got != repo {
			t.Errorf("%s repo_id = %q, want %q", uid, got, repo)
		}
	}
	var carried [18]sql.NullString
	if err := pool.QueryRow(`SELECT name, source, description, cron, env, content_hash, source_path, synced_at, created_by, created_at,
	                                last_modified_by, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars
	                           FROM schedules WHERE uid='s-old'`).Scan(&carried[0], &carried[1], &carried[2], &carried[3], &carried[4], &carried[5],
		&carried[6], &carried[7], &carried[8], &carried[9], &carried[10], &carried[11], &carried[12], &carried[13], &carried[14], &carried[15],
		&carried[16], &carried[17]); err != nil {
		t.Fatalf("read s-old: %v", err)
	}
	want := []string{"old", "cronomicon", "d", "0 0 5 * * *", `{"A":"1"}`, "h2", "p", "sy", "cb", "ca", "mb", "ma", `["t"]`, "sa", "ea", "7d", `["hol"]`, `["work"]`}
	for i := range want {
		if carried[i].String != want[i] {
			t.Errorf("s-old column %d = %q, want %q", i, carried[i].String, want[i])
		}
	}
	if got := str(`SELECT deleted_at FROM schedules WHERE uid='s-old'`); got != "da" {
		t.Errorf("s-old lost its place in the recycle bin: deleted_at = %q", got)
	}

	// The name is unique per source AND owner now.
	exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, owner_agency, repo_id) VALUES('s-git-x','nightly','git','0 0 9 * * *','h','ag-x','repo-x')`)
	if _, err := pool.Exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, owner_agency) VALUES('s-git-x2','nightly','git','0','h','ag-x')`); err == nil {
		t.Error("two Git schedules of one name for ONE agency were accepted")
	}

	// Entries.
	sch := func(owner, name string) string {
		return str(`SELECT schedule_uid FROM definition_schedules WHERE owner_name=? AND name=?`, owner, name)
	}
	if got := sch("g1", "nightly"); got != "s-git" {
		t.Errorf("(a) the Git job's entry is tied to %q, want Git's schedule s-git", got)
	}
	if got := sch("a1", "nightly"); got != "" {
		t.Errorf("(b) the ambiguous entry was tied to %q, want it left for a person to settle", got)
	}
	if got := sch("a1", "solo"); got != "s-solo" {
		t.Errorf("(c) the entry on an unshared name is tied to %q, want s-solo", got)
	}
	if got := sch("a2", "inline"); got != "" {
		t.Errorf("(e) an inline entry was given a schedule: %q", got)
	}
	// (d) and (g): one entry, the one that knew its schedule.
	if n := count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='a2' AND name='nightly'`); n != 1 {
		t.Fatalf("(d)(g) a2's nightly entries = %d, want the uid'd one alone", n)
	}
	if got := sch("a2", "nightly"); got != "s-app" {
		t.Errorf("(d) the entry that knew its schedule is tied to %q, want s-app", got)
	}
	// (f): stamped with its owner and its schedule.
	if got := str(`SELECT owner_uid FROM definition_schedules WHERE owner_name='a2' AND name='solo'`); got != "j-a2" {
		t.Errorf("(f) the restored entry's owner_uid = %q, want j-a2", got)
	}
	if got := sch("a2", "solo"); got != "s-solo" {
		t.Errorf("(f) the restored entry is tied to %q, want s-solo", got)
	}
	// (h): one of the twins, stamped.
	if n := count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='a1' AND name='extra'`); n != 1 {
		t.Errorf("(h) a1's extra entries = %d, want 1", n)
	}
	if got := str(`SELECT owner_uid FROM definition_schedules WHERE owner_name='a1' AND name='extra'`); got != "j-a1" {
		t.Errorf("(h) the kept twin's owner_uid = %q, want j-a1", got)
	}
	// (i): the dead uid is replaced by the schedule that holds the name.
	if got := sch("a3", "solo"); got != "s-solo" {
		t.Errorf("(i) the entry that named a schedule that is gone is tied to %q, want s-solo", got)
	}
	// (j): the two uid-less rows under a name two jobs share are left exactly
	// as they were, and so is the row of the job that was saved.
	if n := count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='twin' AND owner_uid IS NULL`); n != 2 {
		t.Errorf("(j) uid-less entries under a name two jobs share = %d, want both left alone (one may be the other job's only entry)", n)
	}
	if n := count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='twin' AND owner_uid='j-t1'`); n != 1 {
		t.Errorf("(j) the saved job's own entry = %d, want 1", n)
	}
	// (k): the workflow's entry is stamped with its owner and its schedule.
	if got := str(`SELECT owner_uid FROM definition_schedules WHERE owner_kind='workflow' AND owner_name='flow'`); got != "w-1" {
		t.Errorf("(k) the workflow's entry owner_uid = %q, want w-1", got)
	}
	if got := str(`SELECT schedule_uid FROM definition_schedules WHERE owner_kind='workflow' AND owner_name='flow'`); got != "s-solo" {
		t.Errorf("(k) the workflow's entry is tied to %q, want s-solo", got)
	}
	if n := count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_uid IS NULL AND owner_name != 'twin'`); n != 0 {
		t.Errorf("%d entries of a definition whose name is its own still have no owner uid", n)
	}
	if n := count(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_def_schedules_schedule_uid'`); n != 1 {
		t.Error("the index on schedule_uid is missing")
	}

	// Back to 1290: Global's `nightly` keeps the name another agency's also has.
	if err := m.Migrate(1290); err != nil {
		t.Fatalf("migrate down to 1290: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM schedules WHERE source='git' AND name='nightly'`); n != 1 {
		t.Errorf("Git schedules named nightly after the down = %d, want 1", n)
	}
	if got := str(`SELECT uid FROM schedules WHERE source='git' AND name='nightly'`); got != "s-git" {
		t.Errorf("the down kept %q, want Global's s-git", got)
	}
	for _, col := range []string{"owner_agency", "repo_id"} {
		if n := count(`SELECT COUNT(*) FROM pragma_table_info('schedules') WHERE name=?`, col); n != 0 {
			t.Errorf("schedules.%s is still there after the down", col)
		}
	}
	if _, err := pool.Exec(`INSERT INTO schedules(uid, name, source, cron, content_hash) VALUES('dup','nightly','git','0','h')`); err == nil {
		t.Error("after the down, two Git schedules of one name were accepted")
	}
	if got := str(`SELECT deleted_at FROM schedules WHERE uid='s-old'`); got != "da" {
		t.Errorf("the down lost the binned schedule's deleted_at: %q", got)
	}
}
