package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A reaction pinned to another agency's definition is reported to its own
// agency (§0a, open point 1), and the notice resolves when it watches one of
// its own. So is one with no upstream identity whose name only another
// agency's definition holds: the reactor refuses it too (2.4.2, present
// defect 60; until then it was not reported). Not reported: a reaction to its
// own agency's definition.
func TestI1_AReactionToAnotherAgencysDefinitionIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	for _, s := range [][3]string{{"sc-fin", "fin-hosts", "ag-fin"}, {"sc-tax", "tax-hosts", "ag-tax"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES (?, ?, 'cronomicon', 't')`, s[0], s[1])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	for _, j := range [][3]string{{"j-after", "after", "fin-hosts"}, {"j-fin", "fin-job", "fin-hosts"}, {"j-tax", "tax-job", "tax-hosts"}} {
		mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES (?, ?, 'cronomicon', 'bash', ?, 't')`, j[0], j[1], j[2])
	}
	react := func(name, onName string, onUID any) {
		mustExec(t, pool, `INSERT INTO reactions (owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_uid, on_outcome,
		                                          delay_seconds, min_interval_seconds, include_workflow_children, enabled, owner_uid)
		                   VALUES ('cronomicon', 'job', 'after', ?, 'cronomicon', 'job', ?, ?, 'success', 0, 0, 0, 1, 'j-after')`, name, onName, onUID)
	}
	react("on-tax", "tax-job", "j-tax")
	react("on-fin", "fin-job", "j-fin")
	react("by-name", "tax-job", nil)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindReactionOtherAgency)
	}
	got := check()
	n, ok := got["j-after:on-tax"]
	_, byName := got["j-after:by-name"]
	if len(got) != 2 || !ok || !byName {
		t.Fatalf("open notices = %v; want the two reactions to Tax's job", got)
	}
	if n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "tax-job") || !strings.Contains(n.Detail, "another agency's") {
		t.Errorf("the notice is filed under %q with %q", n.AgencyID, n.Detail)
	}

	mustExec(t, pool, `UPDATE reactions SET on_name = 'fin-job', on_uid = 'j-fin' WHERE name = 'on-tax'`)
	mustExec(t, pool, `UPDATE reactions SET on_name = 'fin-job' WHERE name = 'by-name'`)
	if got := check(); len(got) != 0 {
		t.Errorf("after the reaction was pointed at its own agency's job: %v", got)
	}
}
