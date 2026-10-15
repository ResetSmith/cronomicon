package runner

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// Present defect 44, the file-arrival half: an arrival for a project job with
// no registered agent that allows checkout to wait for is refused, with the
// reason on the sighting, and no run is enqueued. With one registered it fires.
func TestArrivalForAProjectJobWithNoCheckoutAgentIsRefused(t *testing.T) {
	svc := newTestService(t)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// The agent that watches the directory has no checkout.
	insertRunner(t, svc, "r1", "one", "online", []string{"ansible", "bash"})
	insertAgencyRow(t, svc, "ag-fin", "Finance")
	addRunnerAgency(t, svc, "r1", "ag-fin")
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	exec(`UPDATE git_repos SET last_sha = '1111111111111111111111111111111111111111' WHERE id = 'global'`)
	// Finance's job, from Finance's repository: a job finds its scope in its own
	// agency (I2), and a job of Global's repository on Finance's scope is
	// stopped since §0a (I1).
	exec(`INSERT INTO git_repos (id, agency_id, url, branch, last_sha) VALUES ('repo-fin', 'ag-fin', 'u', 'main', '1111111111111111111111111111111111111111')`)
	seedWatchJob(t, svc, "git", "ingest", "fin-hosts", `[{"path":"/srv/incoming/*.csv"}]`)
	exec(`UPDATE jobs SET run_type = 'ansible', script_path = 'scripts/proj/site.yml', project_root = 'scripts/proj', repo_id = 'repo-fin' WHERE name = 'ingest'`)
	arrival := func(path string) sightingIn {
		return sightingIn{JobSource: "git", JobName: "ingest", Path: path, SizeBytes: 42, MTime: "2026-08-11T00:00:00Z"}
	}

	before := runCountFor(t, svc, "ingest")
	sg := arrival("/srv/incoming/a.csv")
	if ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg)); ok || err != nil {
		t.Fatalf("an arrival for a project job that no agent can take fired (%v, %v)", ok, err)
	}
	if got := sightingReason(t, svc, sg.Path); got != runref.ReasonCheckoutNeedsAgent {
		t.Errorf("refused_reason = %q, want %q", got, runref.ReasonCheckoutNeedsAgent)
	}
	if n := runCountFor(t, svc, "ingest"); n != before {
		t.Errorf("the refused arrival left %d run rows, want %d", n, before)
	}

	// An agent of that agency that allows checkout is registered.
	insertRunner(t, svc, "r2", "two", "offline", []string{"ansible", "checkout"})
	addRunnerAgency(t, svc, "r2", "ag-fin")
	sg = arrival("/srv/incoming/b.csv")
	if ok, err := svc.recordAndFireSighting(context.Background(), "r1", sg, specFor(sg)); !ok || err != nil {
		t.Fatalf("an arrival with an agent to wait for did not fire: %v %v (reason %q)", ok, err, sightingReason(t, svc, sg.Path))
	}
}
