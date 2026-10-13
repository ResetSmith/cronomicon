package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A schedule entry that records no owner, under a name more than one
// definition holds, fires for none of them (scheduler.loadJobEntries), and
// this notice is what says so (2.4.0 Phase R6, the owner's yes to question
// 18): under Global, naming the entry, its timing and the agencies of the
// definitions that hold the name. What is not reported: an entry with its
// owner, and one with none whose name ONE definition holds, which is that
// definition's and fires. It clears when one definition is left with the
// name, or the entry is gone, and a dismissal holds while it stands.
func TestAScheduleEntryWithNoOwnerUnderASharedNameIsReported(t *testing.T) {
	pool := open(t)
	ctx := context.Background()
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	for _, s := range [][3]string{{"sc-fin", "fin-hosts", "ag-fin"}, {"sc-tax", "tax-hosts", "ag-tax"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, s[0], s[1])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES
	        ('j-fin', 'load', 'cronomicon', 'bash', 'fin-hosts', 't'),
	        ('j-tax', 'load', 'cronomicon', 'bash', 'tax-hosts', 't'),
	        ('j-one', 'solo', 'cronomicon', 'bash', 'fin-hosts', 't'),
	        ('j-git', 'load', 'git',        'bash', 'fin-hosts', 't')`)
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, owner_agency) VALUES
	        ('w-fin', 'nightly', 'cronomicon', '[]', 't', 'ag-fin'),
	        ('w-glb', 'nightly', 'cronomicon', '[]', 't', 'global'),
	        ('w-solo', 'solo',   'cronomicon', '[]', 't', 'ag-fin')`) // a workflow does not hold a job's name
	mustExec(t, pool, `INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, interval, position, owner_uid) VALUES
	        ('cronomicon', 'job',      'load',    'whose',     '0 2 * * *', NULL,  0, NULL),
	        ('cronomicon', 'job',      'load',    'fins-own',  '0 3 * * *', NULL,  1, 'j-fin'),
	        ('cronomicon', 'job',      'solo',    'the-ones',  '0 4 * * *', NULL,  0, NULL),
	        ('git',        'job',      'load',    'gits',      '0 5 * * *', NULL,  0, NULL),
	        ('cronomicon', 'workflow', 'nightly', 'whose-flow', '',         '15m', 0, NULL)`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(ctx, pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindScheduleEntryNoOwner)
	}
	got := check()
	if len(got) != 2 {
		t.Fatalf("open notices = %d (%v), want the two entries with no owner under a shared name", len(got), got)
	}
	n, ok := got["job:cronomicon:load:whose"]
	if !ok || n.AgencyID != "global" {
		t.Fatalf("the job entry's notice: found %v, agency %q; want it under Global", ok, n.AgencyID)
	}
	for _, want := range []string{`"whose"`, "cron 0 2 * * *", `job "load"`, "2 jobs", "Finance", "Tax", "fires none", "dismiss"} {
		if !strings.Contains(n.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, n.Detail)
		}
	}
	w, ok := got["workflow:cronomicon:nightly:whose-flow"]
	if !ok {
		t.Fatalf("no notice for the workflow's entry: %v", got)
	}
	for _, want := range []string{"every 15m", `workflow "nightly"`, "Finance", "Global"} {
		if !strings.Contains(w.Detail, want) {
			t.Errorf("the workflow entry's detail does not say %q: %s", want, w.Detail)
		}
	}

	// A dismissal holds while the condition stands.
	if changed, err := notices.Dismiss(ctx, pool, n.ID, "root@example.com"); err != nil || !changed {
		t.Fatalf("dismiss: %v %v", changed, err)
	}
	if got = check(); len(got) != 1 {
		t.Errorf("after the dismissal and another check, open notices = %v, want the workflow's alone", got)
	}

	// One definition is left with the name: the entry is that one's, and the
	// notice clears. A job in the recycle bin still holds its name, as the
	// scheduler counts it.
	mustExec(t, pool, `UPDATE workflows SET deleted_at = '2026-01-01T00:00:00Z' WHERE uid = 'w-glb'`)
	if got = check(); len(got) != 1 {
		t.Errorf("a workflow in the recycle bin still holds its name: open notices = %v, want the workflow's entry still", got)
	}
	mustExec(t, pool, `DELETE FROM workflows WHERE uid = 'w-glb'`)
	if got = check(); len(got) != 0 {
		t.Errorf("one workflow is left with the name: open notices = %v, want none", got)
	}
	// And when the entry is gone.
	var resolved int
	mustExec(t, pool, `DELETE FROM definition_schedules WHERE name = 'whose'`)
	check()
	if err := pool.QueryRow(`SELECT COUNT(*) FROM notices WHERE kind = ? AND resolved_at IS NOT NULL`, notices.KindScheduleEntryNoOwner).Scan(&resolved); err != nil || resolved != 2 {
		t.Errorf("resolved notices = %d (%v), want both", resolved, err)
	}
}
