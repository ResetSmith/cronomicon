package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// SB — the manual/token half of the producer conformance for a scope bound to
// runners. A job with no executor of its own runs on the bound runners; an
// explicit ssh — from the job or from the request — is 422
// scope_requires_runner and leaves no run row.
func TestRunOnABoundScope(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	exec := mustExec(t, pool)
	// sc:tax is the scope the fixture put in the operator's agency.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('sc:tax','r-dmz','runner-dmz-01','t')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('deploy','git','bash','tax',1)`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, executor, enabled) VALUES ('legacy','git','bash','tax','ssh',1)`)

	executorOf := func(job string) string {
		t.Helper()
		var e string
		if err := pool.QueryRow(`SELECT executor FROM runs WHERE job_name = ? ORDER BY created_at DESC LIMIT 1`, job).Scan(&e); err != nil {
			t.Fatalf("no run row for %s: %v", job, err)
		}
		return e
	}
	refusal := func(rec interface{ Bytes() []byte }) (code, message string) {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(rec.Bytes(), &e)
		return e.Code, e.Message
	}

	// No executor anywhere: the binding decides, not the shell default.
	deploy := runPath(t, pool, "deploy")
	if rec := reqAs(t, h, http.MethodPost, deploy, "sec-operators", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if got := executorOf("deploy"); got != "runner" {
		t.Errorf("executor = %q, want runner — a shell job on a bound scope must not default to ssh", got)
	}

	// The request asks for ssh.
	rec := reqAs(t, h, http.MethodPost, deploy, "sec-operators", `{"executor":"ssh"}`)
	code, msg := refusal(rec.Body)
	if rec.Code != http.StatusUnprocessableEntity || code != execspec.CodeScopeRequiresRunner {
		t.Fatalf("ssh override = %d %q, want 422 %s (%s)", rec.Code, code, execspec.CodeScopeRequiresRunner, rec.Body.String())
	}
	if !containsAll(msg, "tax", "bound to runners", "this run asks") {
		t.Errorf("message %q must name the scope, the reason and who asked", msg)
	}

	// The job itself asks for ssh.
	legacy := runPath(t, pool, "legacy")
	rec = reqAs(t, h, http.MethodPost, legacy, "sec-operators", "")
	code, msg = refusal(rec.Body)
	if rec.Code != http.StatusUnprocessableEntity || code != execspec.CodeScopeRequiresRunner {
		t.Fatalf("job with executor ssh = %d %q, want 422 %s (%s)", rec.Code, code, execspec.CodeScopeRequiresRunner, rec.Body.String())
	}
	if !containsAll(msg, "this job's executor") {
		t.Errorf("message %q must say the job asked", msg)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='legacy'`).Scan(&n)
	if n != 0 {
		t.Errorf("run rows for the refused job = %d, want 0 — a 422 must not leave a row", n)
	}
	// ...and the request can still send it to the runners.
	if rec := reqAs(t, h, http.MethodPost, legacy, "sec-operators", `{"executor":"runner"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("runner override = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
}

// TestRunWithAGlobalSSHDefaultFallsThroughForAToolchainType pins the drift the
// single resolver settled. With the global default executor set to ssh, a
// scheduled Ansible job has always fallen through to the runner — a fleet-wide
// default is not a choice anyone made about that job — but the same job run by
// hand or by token was refused 422, because the manual path's copy of the
// precedence lacked the exception. Both now fall through. An EXPLICIT ssh on a
// toolchain type is still refused.
func TestRunWithAGlobalSSHDefaultFallsThroughForAToolchainType(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	exec := mustExec(t, pool)
	exec(`INSERT INTO settings (key, value) VALUES ('defaultExecutor', 'ssh')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('play','git','ansible','tax',1)`)
	path := runPath(t, pool, "play")

	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("ansible run under a global ssh default = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var executor string
	if err := pool.QueryRow(`SELECT executor FROM runs WHERE job_name='play'`).Scan(&executor); err != nil {
		t.Fatalf("no run row: %v", err)
	}
	if executor != "runner" {
		t.Errorf("executor = %q, want runner", executor)
	}

	rec := reqAs(t, h, http.MethodPost, path, "sec-operators", `{"executor":"ssh"}`)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusUnprocessableEntity || e.Code != execspec.CodeInvalidExecutor {
		t.Errorf("explicit ssh on ansible = %d %q, want 422 %s", rec.Code, e.Code, execspec.CodeInvalidExecutor)
	}
}

// TestRunJudgesTheEffectiveScope — the scope the resolver is asked about is the
// one the run will USE. A job on an open scope, run once against a bound scope
// by a per-run override, is bound-scope work: it goes to the runners, and asking
// for ssh is refused. Resolved against the job's own scope instead, it would
// have left from the server for the bound scope's hosts.
func TestRunJudgesTheEffectiveScope(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-open','open','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-dmz','dmz-web','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-dmz','r-dmz','runner-dmz-01','t')`)
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled) VALUES ('deploy','git','bash','open',1)`)
	path := runPath(t, pool, "deploy")

	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("run on its own scope = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"scope":"dmz-web"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("run overridden onto the bound scope = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	got := map[string]string{}
	rows, err := pool.Query(`SELECT scope, executor FROM runs WHERE job_name='deploy'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope, executor string
		if err := rows.Scan(&scope, &executor); err != nil {
			t.Fatal(err)
		}
		got[scope] = executor
	}
	if got["open"] != "ssh" || got["dmz-web"] != "runner" {
		t.Errorf("executors by scope = %v, want open:ssh and dmz-web:runner", got)
	}

	rec := reqAs(t, h, http.MethodPost, path, "sec-admins", `{"scope":"dmz-web","executor":"ssh"}`)
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusUnprocessableEntity || e.Code != execspec.CodeScopeRequiresRunner {
		t.Errorf("ssh onto the bound scope = %d %q, want 422 %s", rec.Code, e.Code, execspec.CodeScopeRequiresRunner)
	}
}

// TestRunOfASameNamedJobIsItsOwn — the manual half of the identity defect SB
// Phase 0 found on the cron path. Two same-named cronomicon jobs, which R2
// allows across agencies: running one by hand must resolve ITS executor and
// stamp ITS identity on the run, not whichever row a (name, source) lookup
// returns first.
func TestRunOfASameNamedJobIsItsOwn(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, executor, script_ref, enabled)
	      VALUES ('uid-a','twin','cronomicon','bash','scope-a','runner','scripts/a.sh',1)`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, executor, script_ref, enabled)
	      VALUES ('uid-b','twin','cronomicon','bash','scope-b','ssh','scripts/b.sh',1)`)

	for uid, want := range map[string]struct{ scope, executor, script string }{
		"uid-a": {"scope-a", "runner", "scripts/a.sh"},
		"uid-b": {"scope-b", "ssh", "scripts/b.sh"},
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
