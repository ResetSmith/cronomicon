package db

import "testing"

// 1480 (2.4.4 S2): a run records whose it is. Existing runs take their job's
// owner (a Git job's repository's agency, an app job's owner_agency), else
// their one snapshot agency, else Global's; a run inserted afterwards is
// stamped by the trigger, so FIN's job with no scope has FIN's runs, and a
// renamed agency keeps its runs. Down and up again with the rows present.
func TestMigrate1480RunsRecordTheirOwner(t *testing.T) {
	pool, m := migrateTo(t, "run-owner.db", 1470)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO git_repos (id, agency_id, created_at) VALUES ('repo-tax', 'ag-tax', 't')`)
	must(`INSERT INTO jobs (uid, name, source, run_type, owner_agency) VALUES
	        ('j-fin0', 'fin-noscope', 'cronomicon', 'bash', 'ag-fin'),
	        ('j-tax',  'tax-git',     'git',        'bash', NULL)`)
	must(`UPDATE jobs SET repo_id = 'repo-tax' WHERE uid = 'j-tax'`)
	run := func(id, jobUID, snapshot string) {
		t.Helper()
		var uid any
		if jobUID != "" {
			uid = jobUID
		}
		must(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
		      VALUES (?, 'x', ?, 'bash', 'success', 'x', 'manual', 't', ?)`, id, uid, snapshot)
	}
	run("r-fin0", "j-fin0", "[]")         // FIN's job with no scope
	run("r-tax", "j-tax", "[]")           // TAX's repository's job
	run("r-gone", "j-gone", `["Tax"]`)    // its job is gone: the snapshot's one agency
	run("r-none", "", "[]")               // nothing to go by: Global's
	run("r-two", "", `["Finance","Tax"]`) // a scope two agencies share: Global's

	if err := m.Migrate(1480); err != nil {
		t.Fatalf("migrate to 1480: %v", err)
	}
	owner := func(id string) string {
		t.Helper()
		var o *string
		if err := pool.QueryRow(`SELECT owner_agency FROM runs WHERE id = ?`, id).Scan(&o); err != nil {
			t.Fatal(err)
		}
		if o == nil {
			return "<NULL>"
		}
		return *o
	}
	for id, want := range map[string]string{
		"r-fin0": "ag-fin", "r-tax": "ag-tax", "r-gone": "ag-tax", "r-none": "global", "r-two": "global",
	} {
		if got := owner(id); got != want {
			t.Errorf("backfilled %s = %s, want %s", id, got, want)
		}
	}

	// From now on the trigger stamps each run as it is inserted.
	run("r-fin0-new", "j-fin0", "[]")
	if got := owner("r-fin0-new"); got != "ag-fin" {
		t.Errorf("a new run of FIN's job with no scope = %s, want ag-fin", got)
	}
	// An insert that names its owner keeps it.
	must(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, owner_agency)
	      VALUES ('r-named', 'x', 'j-fin0', 'bash', 'success', 'x', 'manual', 't', 'ag-tax')`)
	if got := owner("r-named"); got != "ag-tax" {
		t.Errorf("a run inserted with its owner = %s, want ag-tax", got)
	}
	// A renamed agency keeps its runs: the owner is an id, not a name.
	must(`UPDATE agencies SET name = 'Finance and Budget' WHERE id = 'ag-fin'`)
	if got := owner("r-fin0"); got != "ag-fin" {
		t.Errorf("after FIN's rename, its run = %s, want ag-fin", got)
	}
	var idx int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_runs_owner_started'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("the index on (owner_agency, started_at): %d %v", idx, err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 1480: %v", err)
	}
	if err := m.Migrate(1480); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := owner("r-fin0-new"); got != "ag-fin" {
		t.Errorf("after down and up, r-fin0-new = %s, want ag-fin", got)
	}
}
