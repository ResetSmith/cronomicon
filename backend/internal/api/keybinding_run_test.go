package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// LR-47 — the manual/token half of the producer conformance: a shell job that
// binds an SSH key is 422 key_binding_requires_runner while no agent serves its
// scope (the local runner cannot deliver a key file), and is accepted once one
// does. The per-run `executor` no longer decides anything.
func TestRunOfKeyBoundJobWithNoAgentIsRefused(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "tax"})
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs (name, source, run_type, scope, executor, enabled) VALUES ('deploy','git','bash','tax','ssh',1)`)
	if err := runref.ReplaceBindings(context.Background(), pool,
		runref.Owner{Kind: "job", Source: "git", Name: "deploy"},
		[]runref.Binding{{Kind: runref.KindKey, Name: "deploy_key"}}, "t"); err != nil {
		t.Fatal(err)
	}
	path := runPath(t, pool, "deploy")

	rec := reqAs(t, h, http.MethodPost, path, "sec-operators", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a key-bound run with no agent = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != runref.CodeKeyBindingNeedsAgent {
		t.Errorf("code = %q, want %q", errBody.Code, runref.CodeKeyBindingNeedsAgent)
	}
	if !containsAll(errBody.Message, "CRONOMICON_KEY_deploy_key", "agent", "Secret") {
		t.Errorf("message %q must name the key and both ways out", errBody.Message)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy'`).Scan(&n)
	if n != 0 {
		t.Errorf("run rows = %d, want 0 — a 422 must not leave a row", n)
	}

	// Asking for the runner executor does not change the verdict: it is what
	// every run gets, and there is still nobody to deliver the key.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", `{"executor":"runner"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("the same run asking for the runner executor = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	// An agent of the scope's agency is what changes it — registered is enough;
	// offline is an ordinary wait.
	seed(`INSERT OR IGNORE INTO scopes (id, name, source, created_at) VALUES ('sc-tax-kb', 'tax', 'cronomicon', 't')`)
	seed(`INSERT INTO runners (id, name, status, registered_at, created_at, owner_agency)
	      VALUES ('agent-kb', 'agent-kb', 'offline', 't', 't',
	              COALESCE((SELECT sa.agency_id FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
	                         WHERE sc.name = 'tax' LIMIT 1), 'global'))`)
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("with an agent serving the scope = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
}
