package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// TestNamespaceContractEnforcement covers the W3 store row-name validation and the
// W4 run-env reserved guard end-to-end through the HTTP surface: each rejection
// must surface as 422 (not 500), and a clean write must still succeed.
func TestNamespaceContractEnforcement(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup','bash','echo hi','ssh','sha256:aaa','scripts/backup.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	do := func(method, url string, body any) *http.Response {
		t.Helper()
		var rdr *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, _ := http.NewRequest(method, url, rdr)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		return resp
	}

	cases := []struct {
		name   string
		method string
		url    string
		body   map[string]any
		want   int
	}{
		{"secret with AMADEUS_ key rejected", "POST", ts.URL + "/api/v1/env-secrets",
			map[string]any{"key": "AMADEUS_SECRET_X", "source": "stored", "scope": "", "value": "v"}, http.StatusUnprocessableEntity},
		{"secret named KEK rejected", "POST", ts.URL + "/api/v1/env-secrets",
			map[string]any{"key": "KEK", "source": "stored", "scope": "", "value": "v"}, http.StatusUnprocessableEntity},
		{"secret with dash rejected", "POST", ts.URL + "/api/v1/env-secrets",
			map[string]any{"key": "bad-key", "source": "stored", "scope": "", "value": "v"}, http.StatusUnprocessableEntity},
		{"clean secret accepted", "POST", ts.URL + "/api/v1/env-secrets",
			map[string]any{"key": "GOOD_SECRET", "source": "stored", "scope": "", "value": "v"}, http.StatusCreated},
		{"env var with AMADEUS_ key rejected", "POST", ts.URL + "/api/v1/env-vars",
			map[string]any{"key": "AMADEUS_VAR_X", "value": "v", "scope": ""}, http.StatusUnprocessableEntity},
		{"ssh credential with dash rejected", "POST", ts.URL + "/api/v1/ssh/credentials",
			map[string]any{"label": "bad-label", "source": "vault", "vaultRef": "kv/x"}, http.StatusUnprocessableEntity},
		{"job env with AMADEUS_ key rejected", "POST", ts.URL + "/api/v1/jobs",
			map[string]any{"name": "guardjob", "scriptRef": "backup", "scope": "", "env": map[string]string{"AMADEUS_SECRET_X": "v"}}, http.StatusUnprocessableEntity},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(tc.method, tc.url, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
