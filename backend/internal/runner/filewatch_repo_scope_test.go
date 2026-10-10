package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// GR-15 (2.4.0), the file-arrival half: an arrival for a job of an agency's
// repository whose scope is not that agency's is refused, with the reason on
// the sighting, and no run is enqueued. On its agency's scope it fires.
func TestArrivalForAJobWhoseScopeIsNotItsRepositorysAgencysIsRefused(t *testing.T) {
	svc := newTestService(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	insertRunner(t, svc, "r1", "one", "online", []string{"bash"})
	insertAgencyRow(t, svc, "ag-fin", "Finance")
	insertAgencyRow(t, svc, "ag-tax", "Tax")
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	seedWatchJob(t, svc, "git", "ingest", "fin-hosts", `[{"path":"/srv/incoming/*.csv"}]`)
	exec(`UPDATE jobs SET repo_id = 'repo-fin' WHERE name = 'ingest'`)
	arrival := func(path string) sightingIn {
		return sightingIn{JobSource: "git", JobName: "ingest", Path: path, SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
	}

	sg := arrival("/srv/incoming/a.csv")
	if ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg)); !ok || err != nil {
		t.Fatalf("an arrival for the job on its agency's scope did not fire: %v %v (reason %q)", ok, err, sightingReason(t, svc, sg.Path))
	}

	// The scope is given to another agency.
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	before := runCountFor(t, svc, "ingest")
	sg = arrival("/srv/incoming/b.csv")
	if ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg)); ok || err != nil {
		t.Fatalf("an arrival for a job whose scope is another agency's fired (%v, %v)", ok, err)
	}
	if got := sightingReason(t, svc, sg.Path); got != runref.ReasonRepoScopeMismatch {
		t.Errorf("refused_reason = %q, want %q", got, runref.ReasonRepoScopeMismatch)
	}
	if n := runCountFor(t, svc, "ingest"); n != before {
		t.Errorf("a refused arrival enqueued %d run(s)", n-before)
	}
}
