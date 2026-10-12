package runner

import (
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/gittest"
)

// Two things a job built in the app can be handed that are not its agency's to
// hand it. Found by the independent review of 2.4.0 Phase R5 (2026-10-12),
// reproduced here, and NOT changed: each needs a rule that has not been
// decided. Neither can be met before a second repository can be connected
// (Phase R6), and neither is in a released version: an agency's repository is
// new in 2.4.0.
//
// A job built in the app holds a COPY of its script. For a body kept as a file
// the copy is a path, and dispatch reads the file in the script's repository
// at that repository's last synced commit (GR-22).
func TestGR0_AnInAppJobIsHandedAFileItsAgencyDidNotWrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(root, "job-definitions"))
	globalDir, finDir := filepath.Join(root, "job-definitions"), filepath.Join(root, "repo-fin")
	gSHA := gittest.Commit(t, globalDir, map[string]string{"scripts/run.sh": "echo a file of the installation's own repository\n"})
	fSHA := gittest.Commit(t, finDir, map[string]string{"scripts/run.sh": "echo finance, first\n"})

	svc := newTestService(t)
	as := authSvc(t, svc)
	runnerID, tok := "runner-inapp", "crn_run_inapp"
	insertRunner(t, svc, runnerID, "inapp", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, runnerID)
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := svc.db.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-fin', 'Finance', 't'), ('ag-tax', 'Tax', 't')`)
	exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-fin', 'ag-fin', 'u', 'main')`)
	gittest.Synced(t, svc.db, "global", gSHA)
	gittest.Synced(t, svc.db, "repo-fin", fSHA)
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-fin', 'fin-hosts', 'cronomicon', 't')`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-fin')`)
	exec(`INSERT INTO scripts (uid, repo_id, name, run_type, script_path, content_hash, synced_at) VALUES
	      ('s-fin', 'repo-fin', 'run.sh', 'bash', 'scripts/run.sh', 'h', 't')`)
	// Finance's job, built in the app on a script of Finance's repository.
	exec(`INSERT INTO jobs (uid, name, source, scope, run_type, script_ref, script_uid, script_path, synced_at) VALUES
	      ('j-app', 'app-job', 'cronomicon', 'fin-hosts', 'bash', 'run.sh', 's-fin', 'scripts/run.sh', 't')`)
	body := func() string {
		t.Helper()
		id := db.NewTraceID()
		exec(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, scope, status, runner_id, executor,
		                       triggered_by, trigger_kind, started_at, created_at)
		      VALUES (?, 'app-job', 'cronomicon', 'j-app', 'bash', 'fin-hosts', 'running', ?, 'runner', 'test', 'manual', ?, ?)`,
			id, runnerID, now(), now())
		return getManifest(t, svc, as, id, tok).Body
	}
	if got := body(); got != "echo finance, first\n" {
		t.Fatalf("Finance's job on Finance's scope is handed %q, want its script's file", got)
	}

	// 1. The scope is given to Tax. The job is still joined to Finance's
	// script. Finance's committers change the file, and their repository syncs:
	// the job, which now runs on Tax's hosts, is handed what they wrote. (Sync
	// stops refreshing such a job's copy for exactly this reason, and says "such
	// a job keeps the copy it has": for a file, the copy is only the path.)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-fin'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-fin', 'ag-tax')`)
	gittest.CommitSynced(t, svc.db, "repo-fin", finDir, map[string]string{"scripts/run.sh": "echo finance, written after the scope went to Tax\n"})
	if got := body(); got != "echo finance, written after the scope went to Tax\n" {
		t.Errorf("PRESENT DEFECT CHANGED: a job on what is now Tax's scope, still joined to Finance's script, is handed %q. "+
			"It was handed the file Finance's committers wrote AFTER the scope left them; if that is now refused, invert this test.", got)
	}

	// 2. Finance removes the script. The row's removal clears the job's
	// script_uid; the job keeps the path, has no repository of its own, and the
	// read falls back to Global's repository: the job is handed whatever the
	// installation's own repository holds at a path Finance's committers chose.
	exec(`DELETE FROM scripts WHERE uid = 's-fin'`)
	var joined any
	if err := svc.db.QueryRow(`SELECT script_uid FROM jobs WHERE uid = 'j-app'`).Scan(&joined); err != nil || joined != nil {
		t.Fatalf("after its script was removed the job's script_uid = %v (%v), want none", joined, err)
	}
	if got := body(); got != "echo a file of the installation's own repository\n" {
		t.Errorf("PRESENT DEFECT CHANGED: a job built in the app whose script has gone is handed %q. "+
			"It was handed the file at its path in GLOBAL's repository; if that is now refused, invert this test.", got)
	}
}
