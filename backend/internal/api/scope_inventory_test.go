package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestApiScopeInventoryAuthoring exercises the M5 HTTP surface: PUT inventory
// (200 valid, 422 inventory_secret_rejected with the {code,message,errors[]}
// envelope the frontend parses, 409 git) and import-hosts (200).
func TestApiScopeInventoryAuthoring(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	amID := db.NewID()
	exec(`INSERT INTO scopes(id,name,source,created_at,supported_types) VALUES(?,'edge','amadeus','t','["bash"]')`, amID)
	gitID := db.NewID()
	exec(`INSERT INTO scopes(id,name,source,created_at,supported_types) VALUES(?,'gitscope','git','t','["bash"]')`, gitID)

	client, csrf := devLoginWithCSRF(t, ts)
	req := func(method, path string, body map[string]any) (int, string) {
		t.Helper()
		reqBody, _ := json.Marshal(body)
		r, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(reqBody))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	inv := fmt.Sprintf("/api/v1/scopes/%s/inventory", amID)

	// Valid PUT → 200.
	if code, body := req(http.MethodPut, inv, map[string]any{"raw": "[web]\nweb1 ansible_host=10.0.0.1\n", "format": "ini"}); code != http.StatusOK {
		t.Fatalf("valid PUT = %d (%s), want 200", code, body)
	}
	// Secret-bearing PUT → 422 with the inventory_secret_rejected envelope.
	code, body := req(http.MethodPut, inv, map[string]any{"raw": "[web]\nweb1 ansible_become_pass=hunter2\n", "format": "ini"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "inventory_secret_rejected") || !strings.Contains(body, "\"errors\"") {
		t.Errorf("secret PUT = %d (%s), want 422 with inventory_secret_rejected + errors[]", code, body)
	}
	// Git scope PUT → 409.
	if code, _ := req(http.MethodPut, fmt.Sprintf("/api/v1/scopes/%s/inventory", gitID), map[string]any{"raw": "[web]\nweb1\n"}); code != http.StatusConflict {
		t.Errorf("git PUT = %d, want 409", code)
	}
	// import-hosts on the (now valid) amadeus inventory → 200.
	if code, body := req(http.MethodPost, inv+"/import-hosts", map[string]any{"overwrite": true}); code != http.StatusOK {
		t.Errorf("import-hosts = %d (%s), want 200", code, body)
	}
}
