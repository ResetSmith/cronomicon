package notices_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An agency's entry that names Global's calendar is reported to the agency
// (§0a, 2.4.0), and a global calendar of Global's is reported to Global while
// there are other agencies (it freezes Global only now).
func TestI1_CalendarsAcrossAgenciesAreReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-tax', 'tax-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-tax', 'ag-tax')`)
	mustExec(t, pool, `INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES
	                   ('cronomicon', 'holidays', 0, 0, 't', 't', 'global'), ('cronomicon', 'freeze', 1, 0, 't', 't', 'global')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at) VALUES ('j-tax', 'tax-job', 'cronomicon', 'bash', 'tax-hosts', 't')`)
	mustExec(t, pool, `INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid, skip_calendars)
	                   VALUES ('cronomicon', 'job', 'tax-job', 'nightly', '0 2 * * *', 0, 'j-tax', '["holidays"]')`)
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindCalendarOtherAgency); len(got) != 1 || got["j-tax:nightly:holidays"].AgencyID != "ag-tax" {
		t.Errorf("calendar_other_agency = %v, want Tax's entry naming Global's holidays", got)
	}
	if got := openOf(t, pool, notices.KindGlobalCalendarScope); len(got) != 1 || got["freeze"].AgencyID != "global" {
		t.Errorf("global_calendar_scope = %v, want Global's freeze calendar", got)
	}
}
