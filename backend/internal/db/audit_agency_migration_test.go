package db

import "testing"

// 1520 (2.5.0 C1): every audit row and every workflow run records the agency
// it is of. Old rows take their object's agency where a key says whose (a run,
// a workflow run, a job, a workflow; a change log row's run, workflow run or
// agency id), else the agency 2.4.4 stored, else Global's. A reaction's
// depth-ceiling row is never placed by its trace (it names the UPSTREAM run).
// A workflow run inserted afterwards is stamped by the trigger, and one that
// names its agency keeps it. Down and up again with the rows present.
func TestMigrate1520AuditRowsRecordTheirObjectsAgency(t *testing.T) {
	pool, m := migrateTo(t, "audit-agency.db", 1510)
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	must(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	must(`INSERT INTO git_repos (id, agency_id, created_at) VALUES ('repo-tax', 'ag-tax', 't')`)
	must(`INSERT INTO jobs (uid, name, source, run_type, owner_agency) VALUES ('j-fin', 'load', 'cronomicon', 'bash', 'ag-fin')`)
	must(`INSERT INTO workflows (uid, name, source, steps, synced_at, repo_id) VALUES ('w-tax', 'close', 'git', '[]', 't', 'repo-tax')`)
	must(`INSERT INTO runs (id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES ('r-fin', 'load', 'j-fin', 'bash', 'success', 'x', 'manual', 't')`)
	wfRun := func(id, uid, name string) {
		t.Helper()
		var u any
		if uid != "" {
			u = uid
		}
		must(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at)
		      VALUES (?, ?, 'git', ?, 'success', 'x', 'manual', 't')`, id, name, u)
	}
	wfRun("wr-tax", "w-tax", "close")      // by uid
	wfRun("wr-tax-old", "", "close")       // before 1010: by source and name
	wfRun("wr-gone", "w-gone", "vanished") // its workflow is gone: Global's

	act := func(id int, kind, trace, jobUID, category, action, stored string) {
		t.Helper()
		var tr, ju, st any
		if trace != "" {
			tr = trace
		}
		if jobUID != "" {
			ju = jobUID
		}
		if stored != "" {
			st = stored
		}
		must(`INSERT INTO activity (id, kind, trace_id, job_uid, category, action, at, created_at, agency_id)
		      VALUES (?, ?, ?, ?, ?, ?, 't', 't', ?)`, id, kind, tr, ju, category, action, st)
	}
	act(1, "run-end", "r-fin", "", "", "", "")                            // its run: FIN
	act(2, "workflow-end", "wr-tax", "", "", "", "")                      // its workflow run: TAX
	act(3, "config", "", "j-fin", "Jobs", "paused", "ag-tax")             // its job, over 2.4.4's stamp: FIN
	act(4, "config", "", "", "Scopes", "updated", "ag-tax")               // no key: 2.4.4's stamp kept
	act(5, "config", "", "", "Settings", "updated", "")                   // nothing: Global's
	act(6, "config", "r-fin", "", "Reactions", "Depth ceiling", "ag-tax") // never by its trace
	act(7, "config", "r-fin", "", "Reactions", "Depth ceiling", "")       // nor when nothing was stored

	cl := func(id int, category, action, target, details string) {
		t.Helper()
		must(`INSERT INTO change_log (id, at, actor, category, action, target, details, created_at, acting_agency)
		      VALUES (?, 't', 'x', ?, ?, ?, ?, 't', 'ag-tax')`, id, category, action, target, details)
	}
	cl(1, "Secrets", "injected", "r-fin", "")                             // a run: FIN
	cl(2, "Workflows", "Cancelled", "wr-tax", "workflow run cancelled")   // a workflow run: TAX
	cl(3, "Workflows", "Triggered", "close", "workflow run wr-tax")       // a trigger's trace: TAX
	cl(4, "Agencies", "membership-updated", "agency members: ag-fin", "") // an agency id: FIN
	cl(5, "Scopes", "updated", "web", "")                                 // a name, and acting_agency: Global's (D6)

	if err := m.Migrate(1520); err != nil {
		t.Fatalf("migrate to 1520: %v", err)
	}
	col := func(table, key string, id any) string {
		t.Helper()
		var a *string
		if err := pool.QueryRow(`SELECT agency_id FROM `+table+` WHERE `+key+` = ?`, id).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if a == nil {
			return "<NULL>"
		}
		return *a
	}
	for id, want := range map[string]string{"wr-tax": "ag-tax", "wr-tax-old": "ag-tax", "wr-gone": "global"} {
		if got := col("workflow_runs", "id", id); got != want {
			t.Errorf("backfilled workflow run %s = %s, want %s", id, got, want)
		}
	}
	for id, want := range map[int]string{1: "ag-fin", 2: "ag-tax", 3: "ag-fin", 4: "ag-tax", 5: "global", 6: "ag-tax", 7: "global"} {
		if got := col("activity", "id", id); got != want {
			t.Errorf("backfilled activity row %d = %s, want %s", id, got, want)
		}
	}
	for id, want := range map[int]string{1: "ag-fin", 2: "ag-tax", 3: "ag-tax", 4: "ag-fin", 5: "global"} {
		if got := col("change_log", "id", id); got != want {
			t.Errorf("backfilled change_log row %d = %s, want %s", id, got, want)
		}
	}

	// From now on the trigger stamps each workflow run as it is inserted…
	wfRun("wr-tax-new", "w-tax", "close")
	if got := col("workflow_runs", "id", "wr-tax-new"); got != "ag-tax" {
		t.Errorf("a new run of TAX's workflow = %s, want ag-tax", got)
	}
	// …and one that names its agency keeps it (a refused child is its parent's).
	must(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at, agency_id)
	      VALUES ('wr-child', 'other', 'git', 'skipped', 'x', 'workflow', 't', 'ag-fin')`)
	if got := col("workflow_runs", "id", "wr-child"); got != "ag-fin" {
		t.Errorf("a workflow run inserted with its agency = %s, want ag-fin", got)
	}
	for _, idx := range []string{"idx_activity_agency_at", "idx_change_log_agency_at", "idx_workflow_runs_agency_started"} {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx).Scan(&n); err != nil || n != 1 {
			t.Errorf("index %s: %d %v", idx, n, err)
		}
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down from 1520: %v", err)
	}
	if err := m.Migrate(1520); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if got := col("workflow_runs", "id", "wr-tax-new"); got != "ag-tax" {
		t.Errorf("after down and up, wr-tax-new = %s, want ag-tax", got)
	}
	// The down is lossy: a refused child's agency is its parent's, which no
	// workflow names, so it comes back Global's.
	if got := col("workflow_runs", "id", "wr-child"); got != "global" {
		t.Errorf("after down and up, wr-child = %s, want global (the down loses a named agency)", got)
	}
}
