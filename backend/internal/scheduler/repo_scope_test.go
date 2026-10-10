package scheduler

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// GR-15 (2.4.0) on the three producers the scheduler owns: the clock, a
// reaction, and a parked run when its time comes.
//
// A job of an agency's repository is accepted by sync because it names one of
// that agency's scopes. The scope can then be given to another agency. Until
// the repository next syncs the job is still there, and its runs would go to
// the other agency's hosts: this is what refuses them.

// strandable seeds an agency's repository, a scope that agency owns, and a
// Git job of that repository on that scope. strand gives the scope to another
// agency, which is what leaves the job stranded.
func strandable(t *testing.T, pool *sql.DB, job string) (strand func()) {
	t.Helper()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT OR IGNORE INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT OR IGNORE INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	seedJobRow(t, pool, job, 1)
	exec(`UPDATE jobs SET repo_id = 'repo-fin', scope = 'fin-hosts' WHERE name = ?`, job)
	return func() {
		t.Helper()
		exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
		exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	}
}

func runsOf(t *testing.T, pool *sql.DB, job, status string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name = ? AND status = ?`, job, status).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScheduledFireOfAStrandedJobIsRecordedNotEnqueued(t *testing.T) {
	pool := mustPool(t)
	strand := strandable(t, pool, "confined")
	s := New(pool, quietLog(), nil)
	fire := func() { s.fire("git", "confined", "uid-confined", "bash", "fin-hosts", "Allow", "", "nightly", "") }

	// On its agency's scope it fires.
	fire()
	if n := runsOf(t, pool, "confined", "queued"); n != 1 {
		t.Fatalf("queued runs of the job on its agency's scope = %d, want 1", n)
	}

	strand()
	fire()
	if n := runsOf(t, pool, "confined", "queued"); n != 1 {
		t.Errorf("queued runs after the scope was given to another agency = %d, want the one from before", n)
	}
	var reason string
	if err := pool.QueryRow(`SELECT COALESCE(queued_reason,'') FROM runs WHERE job_name='confined' AND status='skipped'`).Scan(&reason); err != nil {
		t.Fatalf("no skipped run recorded: the fire vanished silently: %v", err)
	}
	if reason != runref.ReasonRepoScopeMismatch {
		t.Errorf("queued_reason = %q, want %q", reason, runref.ReasonRepoScopeMismatch)
	}
	// A standing refusal: the next fire of the same day adds no row.
	fire()
	if n := runsOf(t, pool, "confined", "skipped"); n != 1 {
		t.Errorf("skipped rows after two refused fires of one day = %d, want 1", n)
	}

	// Global's repository's job on the same scope is nobody's to refuse.
	seedJobRow(t, pool, "of-global", 1)
	if _, err := pool.Exec(`UPDATE jobs SET repo_id = 'global', scope = 'fin-hosts' WHERE name = 'of-global'`); err != nil {
		t.Fatal(err)
	}
	s.fire("git", "of-global", "uid-of-global", "bash", "fin-hosts", "Allow", "", "nightly", "")
	if n := runsOf(t, pool, "of-global", "queued"); n != 1 {
		t.Errorf("queued runs of Global's repository's job = %d, want 1", n)
	}
}

func TestReactionToAStrandedJobRecordsADeliveryErrorNotARun(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	seedJobRow(t, pool, "upstream", 1)
	strand := strandable(t, pool, "downstream")
	seedReaction(t, pool, "downstream", "on-upstream", "upstream", "success")
	primeCursor(t, s)

	// On its agency's scope the reaction parks a run.
	seedFinishedRun(t, pool, "r1", "upstream", "success", time.Minute)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "downstream"); n != 1 {
		t.Fatalf("pending runs for the job on its agency's scope = %d, want 1", n)
	}

	strand()
	seedFinishedRun(t, pool, "r2", "upstream", "success", time.Second)
	s.ScanReactions(ctxb())
	if n := countPending(t, pool, "downstream"); n != 1 {
		t.Errorf("pending runs after the scope was given to another agency = %d, want the one from before", n)
	}
	var refused int
	for _, d := range deliveries(t, pool, "downstream") {
		if d.Result == "error" && strings.Contains(d.Detail, "agency's repository") {
			refused++
		}
	}
	if refused != 1 {
		t.Errorf("deliveries = %+v, want one error saying the job's scope is not its repository's agency's", deliveries(t, pool, "downstream"))
	}
}

// A run parked while the job's scope was its agency's, and due after the scope
// was given to another, is not enqueued: it is missed, with the reason.
func TestParkedRunOfAStrandedJobIsMissedNotEnqueued(t *testing.T) {
	pool := mustPool(t)
	strand := strandable(t, pool, "confined")
	s := New(pool, quietLog(), nil)
	park := func() string {
		t.Helper()
		id, err := InsertPendingRun(ctxb(), pool, "job", "confined", "git", "fin-hosts",
			time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "op@example.com",
			&EnqueueParams{JobName: "confined", JobSource: "git", JobUID: "uid-confined", RunType: "bash", Scope: "fin-hosts",
				TriggerKind: "manual", TriggeredBy: "op@example.com"})
		if err != nil {
			t.Fatalf("park: %v", err)
		}
		return id
	}

	park()
	s.PromotePending(ctxb())
	if n := runsOf(t, pool, "confined", "queued"); n != 1 {
		t.Fatalf("a parked run of the job on its agency's scope was not promoted (queued = %d)", n)
	}

	id := park()
	strand()
	s.PromotePending(ctxb())
	if n := runsOf(t, pool, "confined", "queued"); n != 1 {
		t.Errorf("queued runs after the scope was given to another agency = %d, want the one from before", n)
	}
	var status, reason string
	if err := pool.QueryRow(`SELECT status, COALESCE(miss_reason,'') FROM pending_runs WHERE id = ?`, id).Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "missed" || reason != runref.ReasonRepoScopeMismatch {
		t.Errorf("the parked run is %q with reason %q; want missed, %q", status, reason, runref.ReasonRepoScopeMismatch)
	}
}

// GR-16 (2.4.0) in the reaction engine: by NAME, a reaction to a Git
// definition fires on the definition of that name in its owner's own
// repository, else on Global's, and on no other repository's.
//
// A reaction has no usable upstream identity when its upstream had not synced
// when it was written, or was pruned and has returned. Until Phase R4 it then
// fired on "the one live definition of that name, wherever it is": a reaction
// in one agency's repository fired on ANOTHER agency's job whenever that was
// the only one of the name.
func TestAReactionByNameFiresOnItsOwnRepositorysDefinitionThenGlobals(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main'), ('repo-c', 'ag-c', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-b', 'b-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-b'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-b', 'ag-b')`)
	job := func(uid, name, source string, repo, scope any) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, repo_id, scope)
		      VALUES (?, ?, ?, 'bash', 'Allow', 1, 't', ?, ?)`, uid, name, source, repo, scope)
	}
	// Who reacts: a job of repo-b, a job built in the app in repo-b's agency,
	// and a job of Global's repository. None has an upstream identity.
	job("after-b", "after-b", "git", "repo-b", "b-hosts")
	job("after-app", "after-app", "cronomicon", nil, "b-hosts")
	job("after-g", "after-g", "git", "global", nil)
	for _, o := range [][2]string{{"git", "after-b"}, {"cronomicon", "after-app"}, {"git", "after-g"}} {
		exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
		                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
		      VALUES (?, 'job', ?, 'on-nightly', 'git', 'job', 'nightly', 'success', 0, 0, 0, 1, ?)`, o[0], o[1], o[1])
	}
	n := 0
	finish := func(uid string) {
		t.Helper()
		n++
		ts := time.Now().UTC().Add(-time.Duration(60-n) * time.Second).Format(time.RFC3339)
		exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, completed_at, created_at)
		      VALUES (?, 'nightly', 'git', ?, 'bash', 'success', 't', 'scheduled', ?, ?)`, "r-"+uid+"-"+string(rune('a'+n)), uid, ts, ts)
	}
	fired := func() map[string]int {
		t.Helper()
		s.ScanReactions(ctxb())
		out := map[string]int{}
		for _, name := range []string{"after-b", "after-app", "after-g"} {
			out[name] = countPending(t, pool, name)
		}
		return out
	}
	expect := func(when string, want map[string]int) {
		t.Helper()
		got := fired()
		for name, w := range want {
			if got[name] != w {
				t.Errorf("%s: %s has %d pending run(s), want %d", when, name, got[name], w)
			}
		}
	}
	primeCursor(t, s)

	// Only ANOTHER agency's repository has a nightly. Nobody's reaction fires
	// on its run: it is neither repo-b's nor Global's.
	job("n-c", "nightly", "git", "repo-c", nil)
	finish("n-c")
	expect("only another agency's nightly", map[string]int{"after-b": 0, "after-app": 0, "after-g": 0})

	// Global's repository gets one. Its run fires all three (theirs then
	// Global's; Global's own); the other agency's run still fires none.
	job("n-g", "nightly", "git", "global", nil)
	finish("n-g")
	expect("Global's nightly ran", map[string]int{"after-b": 1, "after-app": 1, "after-g": 1})
	finish("n-c")
	expect("the other agency's nightly ran again", map[string]int{"after-b": 1, "after-app": 1, "after-g": 1})

	// repo-b gets its own. Now its own comes first for repo-b's reaction and
	// for the in-app job of repo-b's agency; Global's reaction still means Global's.
	job("n-b", "nightly", "git", "repo-b", nil)
	finish("n-g")
	expect("Global's nightly ran, repo-b having its own", map[string]int{"after-b": 1, "after-app": 1, "after-g": 2})
	finish("n-b")
	expect("repo-b's nightly ran", map[string]int{"after-b": 2, "after-app": 2, "after-g": 2})
}

// With ONLY Global's repository the reaction engine's name rule is the one it
// was: a reaction with no upstream identity fires on the run of a job that was
// pruned before its run finished, and on an old run of a job that has returned
// under a new identity. (The rule for several repositories compares the run's
// definition with the one the name means, and so would fire on neither; as
// Phase R4 was first built it applied to every installation, which the review
// found by running both.)
func TestAReactionByNameIsUnchangedWithOneRepository(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	job := func(uid, name, source string, repo any) {
		t.Helper()
		exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, repo_id)
		      VALUES (?, ?, ?, 'bash', 'Allow', 1, 't', ?)`, uid, name, source, repo)
	}
	job("after", "after", "cronomicon", nil)
	exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
	                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
	      VALUES ('cronomicon', 'job', 'after', 'on-nightly', 'git', 'job', 'nightly', 'success', 0, 0, 0, 1, 'after')`)
	n := 0
	finish := func(uid string) {
		t.Helper()
		n++
		ts := time.Now().UTC().Add(-time.Duration(60-n) * time.Second).Format(time.RFC3339)
		exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, completed_at, created_at)
		      VALUES (?, 'nightly', 'git', ?, 'bash', 'success', 't', 'scheduled', ?, ?)`, "r-"+uid+"-"+string(rune('a'+n)), uid, ts, ts)
	}
	pending := func() int { t.Helper(); s.ScanReactions(ctxb()); return countPending(t, pool, "after") }
	primeCursor(t, s)

	job("n-1", "nightly", "git", "global")
	finish("n-1")
	if got := pending(); got != 1 {
		t.Fatalf("while the job is there: %d pending, want 1", got)
	}
	// The job is pruned while its last run is going; the run then finishes.
	exec(`DELETE FROM jobs WHERE uid = 'n-1'`)
	finish("n-1")
	if got := pending(); got != 2 {
		t.Errorf("after the job was pruned and its last run finished: %d pending, want 2 (as before 2.4.0)", got)
	}
	// It returns under a new identity, and an old run finishes; then a new one.
	job("n-2", "nightly", "git", "global")
	finish("n-1")
	if got := pending(); got != 3 {
		t.Errorf("after the job returned and an old run finished: %d pending, want 3 (as before 2.4.0)", got)
	}
	finish("n-2")
	if got := pending(); got != 4 {
		t.Errorf("after the returned job's run finished: %d pending, want 4", got)
	}
}

// The reaction engine's name rule for a WORKFLOW: as an upstream, and as the
// owner whose repository the name is looked up in. (The tests of 5f643c1 had a
// job in both places.)
func TestAReactionByNameResolvesWorkflowsTheSameWay(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main'), ('repo-c', 'ag-c', 'u', 'main')`)
	wf := func(uid, name, source string, repo any, owner string) {
		t.Helper()
		exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, repo_id, owner_agency) VALUES (?, ?, ?, '[]', 1, 't', ?, ?)`,
			uid, name, source, repo, owner)
	}
	// Who reacts, all WORKFLOWS, none with an upstream identity: one of repo-b,
	// one built in the app that agency B owns, one of Global's repository.
	wf("after-b", "after-b", "git", "repo-b", "ag-b")
	wf("after-app", "after-app", "cronomicon", nil, "ag-b")
	wf("after-g", "after-g", "git", "global", "global")
	for _, o := range [][2]string{{"git", "after-b"}, {"cronomicon", "after-app"}, {"git", "after-g"}} {
		exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
		                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
		      VALUES (?, 'workflow', ?, 'on-nightly', 'git', 'workflow', 'nightly', 'success', 0, 0, 0, 1, ?)`, o[0], o[1], o[1])
	}
	n := 0
	finish := func(uid string) {
		t.Helper()
		n++
		ts := time.Now().UTC().Add(-time.Duration(60-n) * time.Second).Format(time.RFC3339)
		exec(`INSERT INTO workflow_runs (id, workflow_id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind,
		                                 started_at, completed_at, created_at)
		      VALUES (?, 0, 'nightly', 'git', ?, 'success', 't', 'scheduled', ?, ?, ?)`, "wr-"+uid+"-"+string(rune('a'+n)), uid, ts, ts, ts)
	}
	expect := func(when string, want map[string]int) {
		t.Helper()
		s.ScanReactions(ctxb())
		for name, w := range want {
			if got := countPending(t, pool, name); got != w {
				t.Errorf("%s: %s has %d pending run(s), want %d", when, name, got, w)
			}
		}
	}
	primeCursor(t, s)

	wf("n-c", "nightly", "git", "repo-c", "ag-c")
	finish("n-c")
	expect("only another agency's nightly", map[string]int{"after-b": 0, "after-app": 0, "after-g": 0})
	wf("n-g", "nightly", "git", "global", "global")
	finish("n-g")
	expect("Global's nightly ran", map[string]int{"after-b": 1, "after-app": 1, "after-g": 1})
	wf("n-b", "nightly", "git", "repo-b", "ag-b")
	finish("n-g")
	expect("Global's nightly ran, repo-b having its own", map[string]int{"after-b": 1, "after-app": 1, "after-g": 2})
	finish("n-b")
	expect("repo-b's nightly ran", map[string]int{"after-b": 2, "after-app": 2, "after-g": 2})
}

// A job built in the app whose scope several agencies share is Global's for
// the purpose of where its reaction's names are looked up: not the first of
// the agencies that happens to come back.
func TestAReactionOwnersSharedScopeIsGlobals(t *testing.T) {
	pool := mustPool(t)
	s := New(pool, quietLog(), nil)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-both', 'both-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-both'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-both', 'ag-b'), ('sc-both', 'ag-c')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, scope) VALUES ('after', 'after', 'cronomicon', 'bash', 'Allow', 1, 't', 'both-hosts')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, concurrency_policy, enabled, synced_at, repo_id) VALUES
	      ('n-b', 'nightly', 'git', 'bash', 'Allow', 1, 't', 'repo-b'), ('n-g', 'nightly', 'git', 'bash', 'Allow', 1, 't', 'global')`)
	exec(`INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome,
	                             delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
	      VALUES ('cronomicon', 'job', 'after', 'on-nightly', 'git', 'job', 'nightly', 'success', 0, 0, 0, 1, 'after')`)
	finish := func(id, uid string, ago time.Duration) {
		ts := time.Now().UTC().Add(-ago).Format(time.RFC3339)
		exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, completed_at, created_at)
		      VALUES (?, 'nightly', 'git', ?, 'bash', 'success', 't', 'scheduled', ?, ?)`, id, uid, ts, ts)
	}
	primeCursor(t, s)
	finish("r1", "n-b", 50*time.Second)
	s.ScanReactions(ctxb())
	if got := countPending(t, pool, "after"); got != 0 {
		t.Errorf("a job on a scope two agencies share reacted to ONE agency's repository's nightly (%d pending)", got)
	}
	finish("r2", "n-g", 40*time.Second)
	s.ScanReactions(ctxb())
	if got := countPending(t, pool, "after"); got != 1 {
		t.Errorf("the same job did not react to Global's nightly (%d pending, want 1)", got)
	}
}
