package api_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"testing"
)

func credKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func credDoJSON(t *testing.T, client *http.Client, method, url, csrf string, body any) (*http.Response, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

// TestSshCredentialsAPI exercises the SK.8 contract: create derives + returns
// metadata but never the private material, garbage is a 422, and delete is
// guarded (409 with usage) until forced.
func TestSshCredentialsAPI(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLogin(t, ts.URL)

	// Create (201): metadata with derived fingerprint, no private material echoed.
	resp, body := credDoJSON(t, client, http.MethodPost, ts.URL+"/api/v1/ssh/credentials", csrf,
		map[string]any{"label": "prod_deploy", "source": "stored", "material": credKeyPEM(t)})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body=%s", resp.StatusCode, body)
	}
	var created map[string]any
	_ = json.Unmarshal(body, &created)
	if created["fingerprint"] == nil || created["keyType"] != "ssh-ed25519" {
		t.Errorf("missing derived metadata: %s", body)
	}
	if _, leaked := created["material"]; leaked {
		t.Errorf("response leaked private material: %s", body)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("no id in create response")
	}

	// Garbage material → 422 (validate-on-save).
	resp, _ = credDoJSON(t, client, http.MethodPost, ts.URL+"/api/v1/ssh/credentials", csrf,
		map[string]any{"label": "bad", "source": "stored", "material": "not a key"})
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("garbage create status = %d, want 422", resp.StatusCode)
	}

	// List includes it.
	resp, body = credDoJSON(t, client, http.MethodGet, ts.URL+"/api/v1/ssh/credentials", "", nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("prod_deploy")) {
		t.Errorf("list status=%d body=%s", resp.StatusCode, body)
	}

	// Reference it from a host → delete blocked (409 + usage), then forced (204).
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, port, created_at, source, auth_credential_id)
	      VALUES('h1','web1',22,'t','cronomicon',?)`, id); err != nil {
		t.Fatal(err)
	}
	resp, body = credDoJSON(t, client, http.MethodDelete, ts.URL+"/api/v1/ssh/credentials/"+id, csrf, nil)
	if resp.StatusCode != http.StatusConflict || !bytes.Contains(body, []byte("web1")) {
		t.Errorf("delete-in-use status=%d body=%s, want 409 with usage", resp.StatusCode, body)
	}
	resp, _ = credDoJSON(t, client, http.MethodDelete, ts.URL+"/api/v1/ssh/credentials/"+id+"?force=true", csrf, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("force delete status=%d, want 204", resp.StatusCode)
	}
	var cred *string
	_ = pool.QueryRow(`SELECT auth_credential_id FROM ssh_hosts WHERE id='h1'`).Scan(&cred)
	if cred != nil {
		t.Errorf("host ref not cleared after force delete: %v", *cred)
	}
}
