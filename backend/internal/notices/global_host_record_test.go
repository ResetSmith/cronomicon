package notices_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// A host an agency's scope reaches, for which only Global has a record written
// by hand, is reported to the agency (§0a, IN-6; question 33): Global's record
// is Global's runs' only now, and the agency's runs on that host fail. Not
// reported: a host the agency has a record for (by hand, or imported for the
// scope), a Global scope's host, and a host Global has no record of. It clears
// when the agency writes its own.
func TestI1_AHostOnlyGlobalDescribedIsReported(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't')`)
	mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at) VALUES
	                   ('sc-fin', 'fin-prod', 'cronomicon', 't'), ('sc-fin2', 'fin-dr', 'cronomicon', 't'), ('sc-g', 'g-hosts', 'cronomicon', 't')`)
	mustExec(t, pool, `DELETE FROM scope_agencies WHERE scope_id IN ('sc-fin', 'sc-fin2', 'sc-g')`)
	mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin'), ('sc-fin2', 'ag-fin'), ('sc-g', 'global')`)
	for _, sh := range [][2]string{
		{"sc-fin", "db01"}, {"sc-fin2", "db01"}, // one host, two of Finance's scopes: one notice
		{"sc-fin", "web01"},                     // Finance wrote its own
		{"sc-fin", "imp01"},                     // imported for the scope
		{"sc-fin", "nobody01"},                  // Global has no record of it
		{"sc-g", "db01"},                        // a Global scope's: Global's record is its own
	} {
		mustExec(t, pool, `INSERT INTO scope_hosts (scope_id, host) VALUES (?, ?)`, sh[0], sh[1])
	}
	mustExec(t, pool, `INSERT INTO ssh_hosts (id, source, hostname, port, created_at, owner_agency) VALUES
	                   ('h-g1', 'cronomicon', 'db01', 22, 't', 'global'),
	                   ('h-g2', 'cronomicon', 'web01', 22, 't', 'global'),
	                   ('h-g3', 'cronomicon', 'imp01', 22, 't', 'global'),
	                   ('h-f2', 'cronomicon', 'web01', 22, 't', 'ag-fin')`)
	mustExec(t, pool, `INSERT INTO ssh_hosts (id, source, scope_id, hostname, port, created_at) VALUES ('h-i3', 'cronomicon', 'sc-fin', 'imp01', 22, 't')`)

	check := func() map[string]notices.Notice {
		t.Helper()
		if err := notices.RunChecks(context.Background(), pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		return openOf(t, pool, notices.KindGlobalHostRecord)
	}
	got := check()
	if len(got) != 1 {
		t.Fatalf("global_host_record = %v, want Finance's db01 alone", got)
	}
	n, ok := got["ag-fin:db01"]
	if !ok || n.AgencyID != "ag-fin" || !strings.Contains(n.Detail, "fin-dr, fin-prod") || !strings.Contains(n.Detail, "only Global has") {
		t.Errorf("Finance's db01: %+v", n)
	}

	mustExec(t, pool, `INSERT INTO ssh_hosts (id, source, hostname, port, created_at, owner_agency) VALUES ('h-f1', 'cronomicon', 'db01', 22, 't', 'ag-fin')`)
	if got := check(); len(got) != 0 {
		t.Errorf("after Finance wrote its own record for db01: %v", got)
	}
}
