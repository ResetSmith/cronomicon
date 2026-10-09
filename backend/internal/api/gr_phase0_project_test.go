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

// An in-app job built on a PROJECT script is not a checkout job. Today's
// behaviour, pinned; no production code changes.
//
// A project is a directory the agent checks out at a commit the server pins
// when the run is queued. Whether a run is a checkout run is decided from the
// JOB's own copy of its script's checkout marker (jobs.project_root). Sync
// copies the marker onto a job that comes from Git. The composer does not copy
// it onto a job built in the app. So a run of such a job is queued with no
// pinned commit, and the agent is handed the project's entry file alone,
// without the tree it sits in: a playbook that needs a role or a vars file
// beside it fails, and one that needs nothing runs, on any agent that has
// Ansible, with no checkout at all.
//
// Whether this is to change is the owner's to decide, because changing it is
// not harmless: the job becomes a real checkout run, which an agent refuses
// unless it was started with -allow-checkout, lists the repository and holds
// a credential for it. A self-contained playbook that runs today would then
// need such an agent. This test is inverted if that is decided.
func TestGR0_AnInAppJobOnAProjectIsNotACheckoutJob(t *testing.T) {
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
	exec(`UPDATE git_repos SET last_sha = ? WHERE id = 'global'`, sha)
	exec(`INSERT INTO scripts(uid, repo_id, name, run_type, script_path, project_root, content_hash, synced_at)
	      VALUES('s-proj','global','proj','ansible','scripts/proj/site.yml','scripts/proj','sha256:p','t')`)
	// The same script used by a job that came from Git, as sync writes it: the
	// marker is on the job.
	exec(`INSERT INTO jobs(uid, name, source, run_type, script_path, project_root, content_hash, synced_at, script_ref, script_uid)
	      VALUES('j-git','proj-git','git','ansible','scripts/proj/site.yml','scripts/proj','sha256:p','t','proj','s-proj')`)

	client, csrf := devLoginWithCSRF(t, ts)
	b, _ := json.Marshal(map[string]any{"name": "proj-app", "scriptRef": "proj", "scope": ""})
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
	// The composed job has the entry and no marker.
	if got := job("cronomicon", "proj-app", "script_path"); got != "scripts/proj/site.yml" {
		t.Fatalf("the composed job's script_path = %q, want the project's entry", got)
	}
	switch got := job("cronomicon", "proj-app", "project_root"); got {
	case "":
		// Today.
	case "scripts/proj":
		t.Errorf("the composed job carries the checkout marker: this has been decided and built, and the test is to be inverted")
	default:
		t.Errorf("the composed job's project_root = %q", got)
	}

	// The consequence, at enqueue: the Git job's run is pinned to the commit,
	// the in-app job's run is not.
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
	if got := pinned(enqueue("cronomicon", "proj-app")); got != "" {
		t.Errorf("a run of the in-app job on the project is pinned to %q: today it is expected to have no commit, and to be handed the entry file alone", got)
	}
}
