package runner

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
	"github.com/ResetSmith/cronomicon/internal/runref"
	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// A job built in the app holds a COPY of its script. For a body kept as a file
// the copy is a path, and dispatch reads the file in the script's repository
// at that repository's last synced commit (GR-22). Two things followed that
// the independent review of 2.4.0 Phase R5 found (2026-10-12), in code that is
// in no released version: an agency's repository is new in 2.4.0.

// inAppOnFinance seeds Finance's repository with scripts/run.sh, Global's with
// a file at the same path, and a job built in the app on Finance's script, on
// Finance's scope. fetch asks the manifest for a new run of the job.
func inAppOnFinance(t *testing.T) (svc *Service, finDir string, fetch func() *httptest.ResponseRecorder) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(root, "job-definitions"))
	globalDir, finDir := filepath.Join(root, "job-definitions"), filepath.Join(root, "repo-fin")
	gSHA := gittest.Commit(t, globalDir, map[string]string{"scripts/run.sh": "echo a file of the installation's own repository\n"})
	fSHA := gittest.Commit(t, finDir, map[string]string{"scripts/run.sh": "echo finance, first\n"})

	svc = newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-inapp", "crn_run_inapp"
	insertRunner(t, svc, runnerID, "inapp", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	for _, q := range []string{
		`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`,
		`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`,
		`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`,
		`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`,
		`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES
		   ('s-fin', 'repo-fin', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't')`,
		`INSERT INTO jobs (uid, name, source, scope, run_type, script_ref, script_uid, script_path, synced_at) VALUES
		   ('j-app', 'app-job', 'cronomicon', 'fin-hosts', 'bash', 'run.sh', 's-fin', 'scripts/run.sh', 't')`,
	} {
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	gittest.Synced(t, svc.db, "global", gSHA)
	gittest.Synced(t, svc.db, "repo-fin", fSHA)
	fetch = func() *httptest.ResponseRecorder {
		t.Helper()
		id := db.NewTraceID()
		if _, err := svc.db.Exec(`
			INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
			                 triggered_by, trigger_kind, started_at, created_at)
			VALUES (?, 'app-job', 'cronomicon', 'j-app', 'bash', 'fin-hosts', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
			id, runnerID, now(), now()); err != nil {
			t.Fatalf("insert a run: %v", err)
		}
		h := as.RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+id+"/manifest", nil)
		req.SetPathValue("traceId", id)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := fetch(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "echo finance, first") {
		t.Fatalf("Finance's job on Finance's scope was answered %d: %s", rec.Code, rec.Body.String())
	}
	return svc, finDir, fetch
}

// TestAnInAppJobOnAnAgencysScriptIsConfinedToThatAgency (present defect 40;
// this was the first half of the pin
// TestGR0_AnInAppJobIsHandedAFileItsAgencyDidNotWrite, inverted on the owner's
// decision of 2026-10-12).
//
// The job's scope is given to Tax. The job is still joined to Finance's
// script, and Finance's committers change the file. It was handed what they
// wrote and ran it on what are now Tax's hosts: sync stops refreshing such a
// job's copy for exactly this reason, but for a file the copy is only the
// path. The run is refused, as a job that comes from Finance's repository is.
func TestAnInAppJobOnAnAgencysScriptIsConfinedToThatAgency(t *testing.T) {
	svc, finDir, fetch := inAppOnFinance(t)
	exec := func(q string) {
		t.Helper()
		if _, err := svc.db.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	gittest.CommitSynced(t, svc.db, "repo-fin", finDir, map[string]string{"scripts/run.sh": "echo finance, written after the scope went to Tax\n"})

	rec := fetch()
	answer := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(answer, runref.CodeRepoScopeMismatch) ||
		!strings.Contains(answer, runref.ReasonRepoScopeMismatch) {
		t.Fatalf("a job on what is now Tax's scope, still joined to Finance's script, was answered %d: %s; want it refused", rec.Code, answer)
	}
	if strings.Contains(answer, "echo finance") {
		t.Errorf("the refusal carries a body of Finance's: %s", answer)
	}

	// The scope comes back to Finance: the job runs again, with the file as
	// Finance's repository now has it.
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	if rec := fetch(); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "written after the scope went to Tax") {
		t.Errorf("back on Finance's scope the job was answered %d: %s", rec.Code, rec.Body.String())
	}
}

// TestAnInAppJobWhoseScriptHasGoneIsRefused (present defect 41; this was the
// second half of the pin TestGR0_AnInAppJobIsHandedAFileItsAgencyDidNotWrite,
// inverted on the owner's decision of 2026-10-12).
//
// Finance removes the script. The row's removal clears the job's script_uid;
// the job keeps the path and has no repository of its own. The read fell back
// to Global's repository, and the job was handed whatever the installation's
// own repository holds at a path Finance's committers chose. It is refused,
// and told why.
func TestAnInAppJobWhoseScriptHasGoneIsRefused(t *testing.T) {
	svc, _, fetch := inAppOnFinance(t)
	if _, err := svc.db.Exec(`DELETE FROM scripts WHERE uid = 's-fin'`); err != nil {
		t.Fatal(err)
	}
	var joined any
	if err := svc.db.QueryRow(`SELECT script_uid FROM jobs WHERE uid = 'j-app'`).Scan(&joined); err != nil || joined != nil {
		t.Fatalf("after its script was removed the job's script_uid = %v (%v), want none", joined, err)
	}
	rec := fetch()
	answer := rec.Body.String()
	if rec.Code != http.StatusConflict || !strings.Contains(answer, "script_gone") || !strings.Contains(answer, "has been removed") {
		t.Fatalf("a job built in the app whose script has gone was answered %d: %s; want 409 script_gone and the reason", rec.Code, answer)
	}
	if strings.Contains(answer, "a file of the installation's own repository") {
		t.Errorf("the refusal carries the file of Global's repository: %s", answer)
	}

	// The same for a PROJECT built in the app: its run is pinned to nothing
	// (the run-row writer no longer pins it to Global's), and it is told its
	// script has gone, not that its repository had not been synced.
	if _, err := svc.db.Exec(`UPDATE jobs SET project_root = 'scripts', run_type = 'bash' WHERE uid = 'j-app'`); err != nil {
		t.Fatal(err)
	}
	trace, err := scheduler.EnqueueRunWithID(context.Background(), svc.db, scheduler.EnqueueParams{
		JobName: "app-job", JobSource: "cronomicon", JobUID: "j-app", RunType: "bash", Scope: "fin-hosts",
		TriggerKind: "manual", TriggeredBy: "t",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var repo, sha, entry sql.NullString
	if err := svc.db.QueryRow(`SELECT checkout_repo, checkout_sha, checkout_entry FROM runs WHERE id = ?`, trace).Scan(&repo, &sha, &entry); err != nil {
		t.Fatal(err)
	}
	if repo.String != "" || sha.String != "" || entry.String != "scripts/run.sh" {
		t.Errorf("the project run of a job whose script has gone is pinned to (%q, %q) with the entry %q; want no repository, no commit, and its entry",
			repo.String, sha.String, entry.String)
	}
	if _, err := svc.db.Exec(`UPDATE runs SET status = 'running', runner_id = 'runner-inapp', started_at = ? WHERE id = ?`, now(), trace); err != nil {
		t.Fatal(err)
	}
	if code, answer := fetchManifestOf(t, svc, trace, "crn_run_inapp"); code != http.StatusConflict || !strings.Contains(answer, "script_gone") {
		t.Errorf("the project run of a job whose script has gone was answered %d: %s; want 409 script_gone", code, answer)
	}
}

// fetchManifestOf asks the manifest for an existing run.
func fetchManifestOf(t *testing.T, svc *Service, traceID, token string) (int, string) {
	t.Helper()
	h := authSvc(t, svc).RequireRunner(http.HandlerFunc(svc.HandleGetManifest))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+traceID+"/manifest", nil)
	req.SetPathValue("traceId", traceID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}
