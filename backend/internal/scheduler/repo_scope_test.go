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
