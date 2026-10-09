package gitlab

// Phase R3 of 2.4.0, after its review: the arms of the per-repository sync
// that no test reached. Each of these was a statement that could be changed
// back to what it was and leave every test passing.

import (
	"testing"
)

// A reaction's upstream is found in the reaction's OWN repository, then in
// Global's, and in no other (GR-16).
//
// A sync refuses a reaction to a name its repository has no file for, so the
// way here is a file that is there and does not sync: `nightly` names a script
// that is missing, and is dropped. As Phase R3 was first built the reaction
// then fell back to "the one Git definition of that name, wherever it is": it
// was pointed at ANOTHER agency's `nightly` whenever that was the only one.
func TestGR3_AReactionsUpstreamIsNeverAnotherRepositorys(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	c, repoC, remoteC := grRepo(t, a, "repo-c", "ag-c")

	grCommitFiles(t, repoC, remoteC, map[string]string{"jobs/nightly.yaml": grJob("nightly", "echo from-c")}, "the third repository's nightly")
	grSync(t, c, "third repository")
	const after = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: after\nspec:\n  run_type: bash\n  command: echo after\n" +
		"  reactions:\n    - name: when-nightly\n      onKind: job\n      onName: nightly\n      onOutcome: success\n"
	const dropped = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: nightly\nspec:\n  script_ref: no-such-script\n"
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/after.yaml": after, "jobs/nightly.yaml": dropped},
		"a job that reacts to a nightly that does not sync")
	grSync(t, b, "second repository")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name='nightly'`); n != 0 {
		t.Fatalf("the second repository's nightly was imported although its script is missing: the test proves nothing")
	}

	watched := func() string {
		t.Helper()
		return grString(t, a.db, `SELECT COALESCE(r.on_uid, '(none)') FROM reactions r JOIN jobs j ON j.uid = r.owner_uid
		                           WHERE j.repo_id = 'repo-b' AND j.name = 'after'`)
	}
	uidOf := func(repo string) string {
		t.Helper()
		return grString(t, a.db, `SELECT uid FROM jobs WHERE source = 'git' AND repo_id = ? AND name = 'nightly'`, repo)
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM reactions r JOIN jobs j ON j.uid = r.owner_uid WHERE j.repo_id = 'repo-b'`); n != 1 {
		t.Fatalf("the second repository's reaction rows = %d, want 1", n)
	}
	if got := watched(); got == uidOf("repo-c") {
		t.Fatalf("the second repository's reaction watches the THIRD repository's nightly")
	} else if got != "(none)" {
		t.Errorf("the reaction watches %q although neither its repository nor Global's has a nightly that synced", got)
	}

	// Global's repository gets one: that is where the name resolves next.
	grCommitFiles(t, repoA, remoteA, map[string]string{"jobs/nightly.yaml": grJob("nightly", "echo from-global")}, "Global's nightly")
	grSync(t, a, "Global's repository with a nightly")
	grSync(t, b, "second repository again")
	if got, want := watched(), uidOf("global"); got != want {
		t.Errorf("the reaction watches %q, want Global's nightly %q", got, want)
	}

	// And its own repository's comes before Global's.
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/nightly.yaml": grJob("nightly", "echo from-b")}, "its own nightly")
	grSync(t, b, "second repository with its own nightly")
	if got, want := watched(), uidOf("repo-b"); got != want {
		t.Errorf("the reaction watches %q, want its own repository's nightly %q", got, want)
	}
}

// The sweep of pauses left without a definition goes by the owner's uid. By
// name, as it was, a pause whose job is gone was KEPT whenever another
// repository had a job of that name, and a returning job would have inherited
// it. A pause from before the uid, which has none, is still matched by name.
func TestGR3_AnOrphanedPauseIsSweptByItsOwnersUid(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t) // jobs/keep.yaml
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"jobs/other.yaml":        grJob("other", "echo other"),
		"workflows/nightly.yaml": grWorkflow,
	}, "another job and a workflow")
	grSync(t, a, "first sync")

	pause := func(kind, name string, uid any) {
		t.Helper()
		if _, err := a.db.Exec(`INSERT INTO paused_jobs (source, owner_kind, name, paused_by, paused_at, owner_uid)
		                        VALUES ('git', ?, ?, 'ops@example', 't', ?)`, kind, name, uid); err != nil {
			t.Fatalf("pause %s %s: %v", kind, name, err)
		}
	}
	otherUID := grString(t, a.db, `SELECT uid FROM jobs WHERE source='git' AND name='other'`)
	pause("job", "keep", "uid-of-a-job-that-is-gone")              // a job named keep exists: by name this stayed
	pause("job", "other", otherUID)                                // its job is there
	pause("job", "nobody", nil)                                    // from before the uid, no such job
	pause("job", "keep", nil)                                      // from before the uid, the job is there
	pause("workflow", "nightly", "uid-of-a-workflow-that-is-gone") // a workflow named nightly exists
	pause("workflow", "nobody", nil)

	grSync(t, a, "second sync")
	for _, c := range []struct {
		what, where string
		want        int
	}{
		{"a job's pause whose owner uid names no job", `owner_kind='job' AND owner_uid='uid-of-a-job-that-is-gone'`, 0},
		{"a pause of a job that is there", `owner_kind='job' AND name='other'`, 1},
		{"a uid-less pause of a job that is not there", `owner_kind='job' AND name='nobody'`, 0},
		{"a uid-less pause of a job that is there", `owner_kind='job' AND name='keep' AND owner_uid IS NULL`, 1},
		{"a workflow's pause whose owner uid names no workflow", `owner_kind='workflow' AND owner_uid='uid-of-a-workflow-that-is-gone'`, 0},
		{"a uid-less pause of a workflow that is not there", `owner_kind='workflow' AND name='nobody'`, 0},
	} {
		if n := grCount(t, a.db, `SELECT COUNT(*) FROM paused_jobs WHERE `+c.where); n != c.want {
			t.Errorf("%s: %d row(s) after the sync, want %d", c.what, n, c.want)
		}
	}
}

// A satellite row with no owner uid is from before the uid, when the one
// repository there was is the one that is Global's now. Only Global's sync
// clears those by name; another repository's sync, which may have a definition
// of the same name, leaves them.
func TestGR3_RowsFromBeforeTheUidAreGlobalsToClear(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	const timed = "apiVersion: cronomicon.io/v1\nkind: Job\nmetadata:\n  name: timed\nspec:\n  run_type: bash\n  command: echo hi\n"
	grCommitFiles(t, repoA, remoteA, map[string]string{
		"jobs/timed.yaml": timed,
		"jobs/going.yaml": grJob("going", "echo going"),
	}, "Global's jobs")
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{
		"jobs/timed.yaml": timed,
		"jobs/going.yaml": grJob("going", "echo going"),
	}, "the second repository's jobs of the same names")
	grSync(t, b, "second repository")

	legacy := func() {
		t.Helper()
		for _, name := range []string{"timed", "going"} {
			if _, err := a.db.Exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
			                        VALUES('git', 'job', ?, 'from-before', '0 0 5 * * *', 9)`, name); err != nil {
				t.Fatalf("a schedule entry with no owner uid: %v", err)
			}
		}
		if _, err := a.db.Exec(`INSERT INTO reactions(owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome, position)
		                        VALUES('git', 'job', 'timed', 'from-before', 'git', 'job', 'keep', 'success', 9)`); err != nil {
			t.Fatalf("a reaction with no owner uid: %v", err)
		}
	}
	left := func(table, name string) int {
		t.Helper()
		return grCount(t, a.db, `SELECT COUNT(*) FROM `+table+` WHERE owner_source='git' AND owner_uid IS NULL AND owner_name=? AND name='from-before'`, name)
	}
	legacy()

	// The second repository's sync rewrites ITS `timed` and prunes ITS `going`.
	gitRemoveFile(t, repoB, "jobs/going.yaml", "remove going")
	grBackdateAll(t, a)
	grSync(t, b, "second repository again")
	if grCount(t, a.db, `SELECT COUNT(*) FROM jobs WHERE source='git' AND repo_id='repo-b' AND name='going'`) != 0 {
		t.Fatalf("the second repository's job was not pruned: the test proves nothing")
	}
	for _, c := range [][2]string{{"definition_schedules", "timed"}, {"reactions", "timed"}, {"definition_schedules", "going"}} {
		if left(c[0], c[1]) != 1 {
			t.Errorf("another repository's sync cleared the uid-less %s row of %q", c[0], c[1])
		}
	}

	// Global's sync does both.
	gitRemoveFile(t, repoA, "jobs/going.yaml", "remove going")
	grBackdateAll(t, a)
	grSync(t, a, "Global's repository again")
	for _, c := range [][2]string{{"definition_schedules", "timed"}, {"reactions", "timed"}, {"definition_schedules", "going"}} {
		if left(c[0], c[1]) != 0 {
			t.Errorf("Global's sync left the uid-less %s row of %q", c[0], c[1])
		}
	}
}

// An imported host record that belongs to no scope at all goes at the next
// sync, whichever repository it is; one that belongs to another repository's
// scope stays.
func TestGR3_AHostRecordOfNoScopeIsReaped(t *testing.T) {
	a, repoA, remoteA := newSyncFixture(t)
	grCommitFiles(t, repoA, remoteA, map[string]string{"inventory/web.ini": "# cronomicon:v1 owner=t\n[web]\nweb1 ansible_host=10.0.0.1\n"}, "Global's scope")
	grSync(t, a, "Global's repository")
	b, repoB, remoteB := grSecondRepo(t, a)
	grCommitFiles(t, repoB, remoteB, map[string]string{"jobs/other.yaml": grJob("other", "echo other")}, "the second repository")

	record := func(id, host string, scope any) {
		t.Helper()
		if _, err := a.db.Exec(`INSERT INTO ssh_hosts(id, hostname, port, source, scope_id, synced_at, created_at)
		                        VALUES(?, ?, 22, 'git', ?, '2020-01-01T00:00:00Z', 't')`, id, host, scope); err != nil {
			t.Fatalf("a host record: %v", err)
		}
	}
	record("h-none", "of-no-scope", nil)
	// A scope that is gone. The column has a foreign key in some schemas; where
	// it is enforced this arm cannot be reached and the row is not made.
	gone := true
	if _, err := a.db.Exec(`INSERT INTO ssh_hosts(id, hostname, port, source, scope_id, synced_at, created_at)
	                        VALUES('h-gone', 'of-a-scope-that-is-gone', 22, 'git', 'no-such-scope', '2020-01-01T00:00:00Z', 't')`); err != nil {
		gone = false
	}
	// An operator's own record is nobody's to reap.
	if _, err := a.db.Exec(`INSERT INTO ssh_hosts(id, hostname, port, source, created_at) VALUES('h-own', 'operators-own', 22, 'cronomicon', 't')`); err != nil {
		t.Fatalf("an operator's host record: %v", err)
	}

	grBackdateAll(t, a)
	grSync(t, b, "second repository")
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM ssh_hosts WHERE id='h-none'`); n != 0 {
		t.Errorf("the imported record of no scope is still there")
	}
	if gone {
		if n := grCount(t, a.db, `SELECT COUNT(*) FROM ssh_hosts WHERE id='h-gone'`); n != 0 {
			t.Errorf("the imported record of a scope that is gone is still there")
		}
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM ssh_hosts WHERE id='h-own'`); n != 1 {
		t.Errorf("an operator's own record was reaped")
	}
	if n := grCount(t, a.db, `SELECT COUNT(*) FROM ssh_hosts WHERE source='git' AND hostname='web1'`); n != 1 {
		t.Errorf("Global's scope's record was reaped by another repository's sync (count %d)", n)
	}
}
