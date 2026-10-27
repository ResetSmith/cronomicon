package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// TestAnInAppJobOnAProjectIsACheckoutJob (present defect 24; decided by the
// owner on 2026-10-09, built on 2026-10-12; this was the pin
// TestGR0_AnInAppJobOnAProjectIsNotACheckoutJob, inverted).
//
// Whether a run is a checkout run is decided from the JOB's own copy of its
// script's checkout marker (jobs.project_root). Sync copies the marker onto a
// job written in Git. The composer did not copy it onto a job built in the
// app: such a job's run was queued with no pinned commit and the agent was
// handed the project's entry file alone, without its tree. A playbook that
// needs a role or a vars file beside it failed; one that needs nothing ran on
// any agent that has Ansible.
//
// The composer copies it. A job built in the app on a project is pinned at
// enqueue exactly as the Git job on the same project is.
func TestAnInAppJobOnAProjectIsACheckoutJob(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	str := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRowContext(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("query: %v\n%s", err, q)
		}
		return s
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	// 2.5.3: the built-in agency composes nothing; the jobs are an ordinary
	// agency's, on its repository's project.
	seedWorkAgency(t, pool)
	exec(`UPDATE git_repos SET last_sha = ? WHERE id = ?`, sha, workRepo)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script_path, project_root, content_hash, synced_at)
	      VALUES('s-proj',?,'proj','ansible','scripts/proj/site.yml','scripts/proj','sha256:p','t')`, workRepo)
	// The same script used by a job that came from Git, as sync writes it: the
	// marker is on the job.
	exec(`INSERT INTO jobs(uid, name, source, run_type, script_path, project_root, content_hash, synced_at, script_ref, script_uid, repo_id)
	      VALUES('j-git','proj-git','git','ansible','scripts/proj/site.yml','scripts/proj','sha256:p','t','proj','s-proj',?)`, workRepo)

	client, csrf := devLoginWithCSRF(t, ts)
	workInAgency(t, pool, client)
	b, _ := json.Marshal(map[string]any{"name": "proj-app", "scriptRef": "proj", "scope": workScope})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /jobs = %d: %s", resp.StatusCode, raw)
	}

	job := func(source, name, column string) string {
		return str(`SELECT COALESCE(`+column+`,'') FROM jobs WHERE source=? AND name=?`, source, name)
	}
	// The composed job has the entry and the marker.
	if got := job("cronomicon", "proj-app", "script_path"); got != "scripts/proj/site.yml" {
		t.Fatalf("the composed job's script_path = %q, want the project's entry", got)
	}
	if got := job("cronomicon", "proj-app", "project_root"); got != "scripts/proj" {
		t.Errorf("the composed job's project_root = %q, want the project's root: it is not a checkout job", got)
	}

	// The consequence, at enqueue: both runs are pinned to the commit.
	enqueue := func(source, name string) string {
		t.Helper()
		id, err := scheduler.EnqueueRunWithID(ctx, pool, scheduler.EnqueueParams{
			JobName: name, JobSource: source, JobUID: job(source, name, "uid"),
			RunType: "ansible", TriggerKind: "manual", TriggeredBy: "t@example.com",
		})
		if err != nil {
			t.Fatalf("enqueue %s: %v", name, err)
		}
		return id
	}
	pinned := func(id string) string {
		return str(`SELECT COALESCE(checkout_sha,'') FROM runs WHERE id=?`, id)
	}
	if got := pinned(enqueue("git", "proj-git")); got != sha {
		t.Errorf("a run of the Git job on the project has checkout_sha %q, want the pinned commit", got)
	}
	app := enqueue("cronomicon", "proj-app")
	if got := pinned(app); got != sha {
		t.Errorf("a run of the in-app job on the project has checkout_sha %q, want the pinned commit: it would be handed the entry file alone", got)
	}
	if got := str(`SELECT COALESCE(checkout_entry,'') || ' in ' || COALESCE(checkout_repo,'') FROM runs WHERE id=?`, app); got != "scripts/proj/site.yml in "+workRepo {
		t.Errorf("the run of the in-app job is pinned to %q, want the project's entry in its script's repository", got)
	}

	// Saved again on a script that is no project, it stops being one: the
	// marker follows the script the job is on.
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script_path, content_hash, synced_at)
	      VALUES('s-plain',?,'plain','ansible','scripts/plain.yml','sha256:q','t')`, workRepo)
	b, _ = json.Marshal(map[string]any{"name": "proj-app", "scriptRef": "plain", "scope": workScope})
	req, _ = http.NewRequest(http.MethodPut, ts.URL+"/api/v1/jobs/"+job("cronomicon", "proj-app", "rowid"), bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("PUT /jobs: %v", err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /jobs = %d: %s", resp.StatusCode, raw)
	}
	if got := job("cronomicon", "proj-app", "project_root"); got != "" {
		t.Errorf("saved on a script that is no project, the job's project_root = %q, want none", got)
	}
	if got := pinned(enqueue("cronomicon", "proj-app")); got != "" {
		t.Errorf("a run of the job on a plain script is pinned to %q, want a body run", got)
	}
}
