package runner

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// After the second independent review of 2.4.0 Phase R5 (of the four fixes of
// 2026-10-12).

// TestManifestRefusesARunPinnedToARepositoryItsScopeLeft: the dispatch-time
// confinement asked about the job as it is NOW, and a checkout run is sent to
// the repository it was PINNED to when it was queued. Such a run can wait a
// long time for its agent. Its scope is given to Tax; then the job is joined
// to another script (Finance removes its script, and a sync of Global's finds
// the name again there). The job is no longer Finance's script's, so the
// job's check passes, and the agent was sent to Finance's repository at
// Finance's commit to run it on what are now Tax's hosts.
func TestManifestRefusesARunPinnedToARepositoryItsScopeLeft(t *testing.T) {
	svc, _, _ := inAppOnFinance(t)
	exec := func(q string) {
		t.Helper()
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	// The script is a project, and so is the job built on it.
	exec(`UPDATE scripts SET project_root = 'scripts' WHERE uid = 's-fin'`)
	exec(`UPDATE jobs SET project_root = 'scripts' WHERE uid = 'j-app'`)
	queue := func() string {
		t.Helper()
		trace, err := scheduler.EnqueueRunWithID(context.Background(), svc.db, scheduler.EnqueueParams{
			JobName: "app-job", JobSource: "cronomicon", JobUID: "j-app", RunType: "bash", Scope: "fin-hosts",
			TriggerKind: "manual", TriggeredBy: "t",
		})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		return trace
	}
	claim := func(trace string) {
		t.Helper()
		if _, err := svc.db.Exec(`UPDATE runs SET status = 'running', runner_id = 'runner-inapp', started_at = ? WHERE id = ?`, now(), trace); err != nil {
			t.Fatal(err)
		}
	}
	waiting, onItsOwnScope := queue(), queue()
	var repo sql.NullString
	if err := svc.db.QueryRow(`SELECT checkout_repo FROM runs WHERE id = ?`, waiting).Scan(&repo); err != nil || repo.String != "repo-fin" {
		t.Fatalf("the run is pinned to %q (%v), want Finance's repository", repo.String, err)
	}

	// While the scope is Finance's, a run pinned to Finance's repository is
	// handed out.
	claim(onItsOwnScope)
	if code, answer := fetchManifestOf(t, svc, onItsOwnScope, "crn_run_inapp"); code != http.StatusOK || !strings.Contains(answer, `"checkout"`) {
		t.Fatalf("on Finance's scope the pinned run was answered %d: %s", code, answer)
	}

	// The scope goes to Tax, and the job's script leaves Finance's repository:
	// the job keeps an inline copy and is joined to no script, so its own check
	// passes and what refuses the run is the pin. (Until §0a this joined the
	// job to Global's script of the name; since then Global's script on Tax's
	// scope is refused by the job's own check, question 31.)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	exec(`DELETE FROM scripts WHERE uid = 's-fin'`)
	exec(`UPDATE jobs SET script_uid = NULL, script = 'echo inline', script_path = NULL, project_root = NULL WHERE uid = 'j-app'`)
	if stranded, err := runref.RepoScopeMismatch(context.Background(), svc.db, "j-app", "cronomicon", "app-job", "fin-hosts"); err != nil || stranded {
		t.Fatalf("the job as it is now is refused (%v, %v): the fixture does not reach the pin's check", stranded, err)
	}

	claim(waiting)
	code, answer := fetchManifestOf(t, svc, waiting, "crn_run_inapp")
	if code != http.StatusConflict || !strings.Contains(answer, runref.CodeRepoScopeMismatch) {
		t.Fatalf("a run pinned to Finance's repository, on what is now Tax's scope, was answered %d: %s; want it refused", code, answer)
	}
	if strings.Contains(answer, `"checkout"`) || strings.Contains(answer, "echo finance") {
		t.Errorf("the refusal carries the pin or a body of Finance's: %s", answer)
	}
}
