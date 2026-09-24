package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runref"
)

// KB — the manual/token half of the producer conformance: a key-bound job whose
// run RESOLVES to the ssh executor is 422 key_binding_requires_runner, and the
// same request overridden to the runner executor is accepted. Tests the
// resolved executor, so the per-run override is what flips the verdict.
func TestRunOfKeyBoundJobOnSSHIsRefused(t *testing.T) {
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
		t.Fatalf("ssh run = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errBody)
	if errBody.Code != runref.CodeKeyBindingOnSSH {
		t.Errorf("code = %q, want %q", errBody.Code, runref.CodeKeyBindingOnSSH)
	}
	if !containsAll(errBody.Message, "CRONOMICON_KEY_deploy_key", "runner", "Secret") {
		t.Errorf("message %q must name the key and both ways out", errBody.Message)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_name='deploy'`).Scan(&n)
	if n != 0 {
		t.Errorf("run rows = %d, want 0 — a 422 must not leave a row", n)
	}

	// The per-run override flips the resolved executor, and with it the verdict.
	if rec := reqAs(t, h, http.MethodPost, path, "sec-operators", `{"executor":"runner"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("runner-override run = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
}
