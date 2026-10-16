package notices_test

import (
	"context"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// Present defect 59 (found by the review of 2.4.x): SpanningWorkflows found a
// step's job by the lowest rowid of its name, across every repository, while
// the engine finds a Git workflow's job in its own repository. A workflow of
// B's, through its own repository's child, was said to span (and its save
// refused) because C's repository's `deploy` was the older row.
func TestRV_SpanningWorkflowsResolvesAStepAsTheEngineDoes(t *testing.T) {
	pool := open(t)
	mustExec(t, pool, `INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't'), ('ag-c', 'C', 't')`)
	mustExec(t, pool, `INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', 'u', 'main'), ('repo-c', 'ag-c', 'u', 'main')`)
	for _, s := range [][3]string{{"sc-b", "b-hosts", "ag-b"}, {"sc-c", "c-hosts", "ag-c"}} {
		mustExec(t, pool, `INSERT INTO scopes (id, name, source, created_at, owner_agency) VALUES (?, ?, 'cronomicon', 't', ?)`, s[0], s[1], s[2])
		mustExec(t, pool, `DELETE FROM scope_agencies WHERE scope_id = ?`, s[0])
		mustExec(t, pool, `INSERT INTO scope_agencies (scope_id, agency_id) VALUES (?, ?)`, s[0], s[2])
	}
	// C's deploy first: the older row.
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, repo_id) VALUES ('j-c', 'deploy', 'git', 'bash', 'c-hosts', 't', 'repo-c')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, repo_id) VALUES ('j-b', 'deploy', 'git', 'bash', 'b-hosts', 't', 'repo-b')`)
	mustExec(t, pool, `INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, owner_agency) VALUES ('j-b2', 'prep', 'cronomicon', 'bash', 'b-hosts', 't', 'ag-b')`)
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, repo_id) VALUES ('w-flow', 'flow', 'git', '[{"type":"job","name":"deploy"}]', 't', 'repo-b')`)
	mustExec(t, pool, `INSERT INTO workflows (uid, name, source, steps, synced_at, owner_agency) VALUES ('w-b', 'b-parent', 'cronomicon',
	        '[{"type":"job","name":"prep","jobUid":"j-b2"},{"type":"workflow","name":"go","workflow":"flow","workflowUid":"w-flow"}]', 't', 'ag-b')`)
	spanning, err := notices.SpanningWorkflows(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range spanning {
		if w.Name == "b-parent" {
			t.Errorf("B's workflow through its own repository's child is said to span %s (present defect 59)", w.Agencies)
		}
	}
}
