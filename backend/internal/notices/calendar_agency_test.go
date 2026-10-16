package notices_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A global calendar of Global's is reported to Global while there are other
// agencies (§0a, 2.4.0: it freezes Global only now). An agency's entry naming
// a calendar of its own is nothing to report (since I2 each agency has its
// own calendars, and a name is unique within the agency only).
func TestI1_CalendarsAcrossAgenciesAreReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES
	                   ('cronomicon', 'holidays', 0, 0, 't', 't', 'global'), ('cronomicon', 'freeze', 1, 0, 't', 't', 'global'),
	                   ('cronomicon', 'holidays', 0, 0, 't', 't', 'ag-tax'), ('cronomicon', 'freeze', 1, 0, 't', 't', 'ag-tax')`)
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindGlobalCalendarScope); len(got) != 1 || got["freeze"].AgencyID != "global" {
		t.Errorf("global_calendar_scope = %v, want Global's freeze calendar alone (Tax's is Tax's)", got)
	}
}
