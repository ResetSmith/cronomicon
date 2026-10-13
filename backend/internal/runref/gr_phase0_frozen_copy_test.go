package runref

import (
	"context"
	"reflect"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// PRESENT DEFECT 46, reproduced and not changed (found by the review of 2.4.0
// Phase R5's last part; on the branch only, a consequence of 94f1ae9).
//
// A job built in the app holds a copy of its script, which sync refreshes: the
// run type, the body, the project marker. Since the review of Phase R4
// (eabcd53) sync does NOT refresh the copy of a job whose scope has been given
// to another agency than the script's repository's: "such a job keeps the copy
// it has". Since 94f1ae9 dispatch reads the BODY from the script's record and
// not from the copy. The run's type is still the copy's: every producer writes
// it on the run's row from the job's row.
//
// For every job sync keeps in step the two cannot differ. For the job it does
// not, they can: its script's run type is changed in Git, and the job is run
// with its scope overridden to one of the script's agency's (its own scope is
// refused, by RepoScopeMismatch). That run is handed the script's new body
// under the copy's old interpreter. Before 94f1ae9 it ran the copy's body
// under the copy's interpreter.
//
// It takes a scope given to another agency, a script whose run type changes,
// and a manual run on another scope, together. Not changed: both ways of
// putting it right change what the owner decided in Phase R4 (question 16).
// When it is decided, this test is inverted.
func TestGR0_AJobWhoseCopyIsNotRefreshedRunsItsScriptsBodyUnderItsOldRunType(t *testing.T) {
	f := newOwnerFixture(t) // agencies ag-a and ag-b
	ctx := context.Background()
	f.exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-a', 'ag-a', 'u', 'main')`)
	f.exec(`INSERT INTO scopes (id, name, source, created_at) VALUES
	        ('s-a', 'a-hosts', 'cronomicon', 't'), ('s-b', 'b-hosts', 'cronomicon', 't')`)
	f.exec(`DELETE FROM scope_agencies WHERE scope_id IN ('s-a', 's-b')`)
	f.exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('s-a', 'ag-a'), ('s-b', 'ag-b')`)
	// The script as agency A's repository has it now: python.
	f.exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script, content_hash, synced_at) VALUES
	        ('sc-a', 'repo-a', 'tool', 'python', 'print("as synced")', 'h', 't')`)
	// The job's copy as it was when its scope was given to agency B: bash.
	f.exec(`INSERT INTO jobs (uid, name, source, run_type, scope, synced_at, script_ref, script_uid, script) VALUES
	        ('j-app', 'built', 'cronomicon', 'bash', 'b-hosts', 't', 'tool', 'sc-a', 'echo the copy it keeps')`)
	// A manual run on one of A's scopes: its type is the job row's.
	f.exec(`INSERT INTO runs (id, job_name, job_source, job_uid, run_type, scope, status, triggered_by, trigger_kind, created_at)
	        VALUES ('r1', 'built', 'cronomicon', 'j-app', 'bash', 'a-hosts', 'running', 'u', 'manual', 't')`)

	// On its own scope the job is refused, where it is produced.
	if stranded, err := RepoScopeMismatch(ctx, f.pool, "j-app", "cronomicon", "built", "b-hosts"); err != nil || !stranded {
		t.Fatalf("on its own scope, which is not its script's agency's: refused = %v (%v), want true", stranded, err)
	}
	// On A's scope nothing refuses it, at production or at dispatch.
	if stranded, err := RepoScopeMismatch(ctx, f.pool, "j-app", "cronomicon", "built", "a-hosts"); err != nil || stranded {
		t.Fatalf("on a scope of its script's agency: refused = %v (%v), want false", stranded, err)
	}
	if refusal, err := RefuseDispatch(ctx, f.pool, "r1"); err != nil || refusal != nil {
		t.Fatalf("the dispatch decision: %+v (%v), want the run to go ahead", refusal, err)
	}
	interp, body, err := execspec.ResolveCommand(ctx, f.pool, nil, execspec.JobRef{UID: "j-app"}, execspec.Pin{}, "bash")
	if err != nil {
		t.Fatal(err)
	}
	// THE DEFECT: python text, handed to bash.
	if !reflect.DeepEqual(interp, []string{"bash", "-c"}) || body != `print("as synced")` {
		t.Fatalf("the run is handed %v with body %q; the defect as pinned is [bash -c] with the script's python body. "+
			"If this changed on purpose, invert this test", interp, body)
	}
}
