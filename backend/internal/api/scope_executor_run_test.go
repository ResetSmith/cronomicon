package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// LR-42 — the manual/token half of the producer conformance for the one
// executor. On a scope bound to runners, as anywhere else, a run is written for
// the runner executor whoever asks for what: the job's own `executor` is not
// read and the request's is ignored. (Until 2.3.0 an explicit ssh on a bound
// scope was 422 scope_requires_runner.)
func TestRunOnABoundScopeIgnoresEveryExecutorChoice(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	exec := mustExec(t, pool)
	// sc:tax is the scope the fixture put in the operator's agency.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('sc:tax','r-dmz','runner-dmz-01','t')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('deploy','git','bash','tax',1)`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, executor, enabled) VALUES ('legacy','git','bash','tax','ssh',1)`)
	exec(`INSERT INTO settings (key, value) VALUES ('defaultExecutor', 'ssh')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('play','git','ansible','tax',1)`)

	for _, tc := range []struct{ what, job, body string }{
		{"a shell job, nothing asked", "deploy", ""},
		{"a shell job, the request asks for ssh", "deploy", `{"executor":"ssh"}`},
		{"a shell job, the request asks for the runner", "deploy", `{"executor":"runner"}`},
		{"a job whose own executor says ssh", "legacy", ""},
		{"an ansible job under a global default of ssh", "play", ""},
		{"an ansible job, the request asks for ssh", "play", `{"executor":"ssh"}`},
	} {
		exec(`DELETE FROM runs`)
		rec := reqAs(t, h, http.MethodPost, runPath(t, pool, tc.job), "sec-operators", tc.body)
		if rec.Code != http.StatusAccepted {
			t.Errorf("%s = %d, want 202 (%s)", tc.what, rec.Code, rec.Body.String())
			continue
		}
		var executor string
		if err := pool.QueryRow(`SELECT executor FROM runs WHERE job_name = ?`, tc.job).Scan(&executor); err != nil {
			t.Fatalf("%s: no run row: %v", tc.what, err)
		}
		if executor != "runner" {
			t.Errorf("%s: executor = %q, want runner", tc.what, executor)
		}
	}
}

// TestRunJudgesTheEffectiveScope — what is decided about a run is decided about
// the scope the run will USE. A key-bound job on a scope no agent serves, run
// once against a scope an agent is bound to by a per-run override, is that
// second scope's work: there is an agent to deliver the key, so it is accepted.
// Judged against the job's own scope instead, it would have been refused — and
// the other way round, a run overridden onto the agentless scope would have
// been queued for nobody.
func TestRunJudgesTheEffectiveScope(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-open','open','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-dmz','dmz-web','cronomicon','2026-01-01T00:00:00Z')`)
	// One agent, Global's like both scopes. dmz-web is bound to it; open is
	// bound to a runner that is gone, so nothing serves open.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('r-dmz','runner-dmz-01','online','t','t')`)
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-dmz','r-dmz','runner-dmz-01','t')`)
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-open','r-gone','runner-gone','t')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('deploy','git','bash','open',1)`)
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "git", Name: "deploy"},
		[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "t"); err != nil {
		t.Fatal(err)
	}
	path := runPath(t, pool, "deploy")
	code := func(rec *httptest.ResponseRecorder) string {
		var e struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &e)
		return e.Code
	}

	rec := reqAs(t, h, http.MethodPost, path, "sec-admins", "")
	if rec.Code != http.StatusUnprocessableEntity || code(rec) != runref.CodeKeyBindingNeedsAgent {
		t.Errorf("run on its own scope, which no agent serves = %d %q, want 422 %s (%s)",
			rec.Code, code(rec), runref.CodeKeyBindingNeedsAgent, rec.Body.String())
	}
	rec = reqAs(t, h, http.MethodPost, path, "sec-admins", `{"scope":"dmz-web"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run overridden onto the scope an agent is bound to = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var scope, executor string
	if err := pool.QueryRow(`SELECT scope, executor FROM runs WHERE job_name='deploy'`).Scan(&scope, &executor); err != nil {
		t.Fatalf("exactly one run row was expected: %v", err)
	}
	if scope != "dmz-web" || executor != "runner" {
		t.Errorf("the run is on %q for %q, want dmz-web / runner", scope, executor)
	}
}

// TestRunOfASameNamedJobIsItsOwn — the manual half of the identity defect SB
// Phase 0 found on the cron path. Two same-named cronomicon jobs, which R2
// allows across agencies: running one by hand must stamp ITS identity and ITS
// script on the run, not whichever row a (name, source) lookup returns first.
// (Each twin declares a different executor; neither is read since 2.3.0.)
func TestRunOfASameNamedJobIsItsOwn(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, executor, script_ref, enabled)
	      VALUES ('uid-a','twin','cronomicon','bash','scope-a','runner','scripts/a.sh',1)`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, executor, script_ref, enabled)
	      VALUES ('uid-b','twin','cronomicon','bash','scope-b','ssh','scripts/b.sh',1)`)

	for uid, want := range map[string]struct{ scope, executor, script string }{
		"uid-a": {"scope-a", "runner", "scripts/a.sh"},
		"uid-b": {"scope-b", "runner", "scripts/b.sh"},
	} {
		var rowid int64
		if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE uid = ?`, uid).Scan(&rowid); err != nil {
			t.Fatal(err)
		}
		path := "/api/v1/jobs/" + jsonInt(rowid) + "/run"
		if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code != http.StatusAccepted {
			t.Fatalf("run of %s = %d, want 202 (%s)", uid, rec.Code, rec.Body.String())
		}
		var gotUID, executor, script string
		if err := pool.QueryRow(`SELECT COALESCE(job_uid,''), executor, COALESCE(script_ref,'') FROM runs WHERE scope = ?`,
			want.scope).Scan(&gotUID, &executor, &script); err != nil {
			t.Fatalf("no run row on %s: %v", want.scope, err)
		}
		if gotUID != uid || executor != want.executor || script != want.script {
			t.Errorf("run of %s = uid %q executor %q script %q, want %q %q %q",
				uid, gotUID, executor, script, uid, want.executor, want.script)
		}
	}
}
