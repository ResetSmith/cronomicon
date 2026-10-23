package notices_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A global calendar of Global's was reported to Global while there were other
// agencies (§0a, 2.4.0: it froze Global only). Retired in 2.5.3 (CA-18):
// Central's calendars are the catalogue agencies take copies of (CA-25), and
// the notice is raised no more. An agency's own global calendar never was.
func TestI1_CalendarsAcrossAgenciesAreReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-tax', 'Tax', 't')`)
	mustExec(t, pool, `INSERT INTO calendars (source, name, global, record_suppressed, created_by, created_at, owner_agency) VALUES
	                   ('cronomicon', 'holidays', 0, 0, 't', 't', 'global'), ('cronomicon', 'freeze', 1, 0, 't', 't', 'global'),
	                   ('cronomicon', 'holidays', 0, 0, 't', 't', 'ag-tax'), ('cronomicon', 'freeze', 1, 0, 't', 't', 'ag-tax')`)
	if err := notices.RunChecks(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	if got := openOf(t, pool, notices.KindGlobalCalendarScope); len(got) != 0 {
		t.Errorf("global_calendar_scope = %v, want none: the notice is retired", got)
	}
}
