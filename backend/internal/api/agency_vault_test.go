package api_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// The value every read of the fake agency Vault answers. No answer of the API
// may carry it, nor the AppRole credentials.
const (
	sg3Value    = "vault-value-zz9"
	sg3RoleID   = "role-id-zz9"
	sg3SecretID = "secret-id-zz9"
)

// sg3Vault is an https fake Vault: AppRole login, token lookup, and a KV read
// that answers sg3Value, or 404 for a path containing "missing".
func sg3Vault(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role_id"] != sg3RoleID || body["secret_id"] != sg3SecretID {
				http.Error(w, `{"errors":["invalid role or secret id"]}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok", "lease_duration": 3600}})
		case strings.HasSuffix(r.URL.Path, "/auth/token/lookup-self"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		case strings.Contains(r.URL.Path, "missing"):
			http.Error(w, `{"errors":[]}`, http.StatusNotFound)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": sg3Value}}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSG3_AgencyVaultRoutes walks an agency's own Vault through the API with
// the two-agency cast: who may read and change it, the preview and the
// confirmation, the credentials and values never answered, the prefixes that
// become the agency's, and the disconnect refused while in use.
func TestSG3_AgencyVaultRoutes(t *testing.T) {
	var cfg *config.Config
	h, pool := gateServerWith(t, func(c *config.Config) {
		c.OutboundAllowPrivate, c.OutboundAllowLoopback = true, true
		cfg = c
	})
	ctx := context.Background()
	vault := sg3Vault(t)
	u, _ := url.Parse(vault.URL)
	if _, err := pool.ExecContext(ctx, `INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES('vaultAllowedHosts', ?, 't', 't')`,
		`["`+u.Host+`"]`); err != nil {
		t.Fatal(err)
	}
	sec := secrets.New(pool, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, p := range []string{"secret/data/ok#value", "secret/data/missing#value"} {
		if _, err := sec.Create(ctx, secrets.CreateInput{Key: "K" + p[12:14], Source: "vault", VaultPath: p, OwnerAgency: "ag:FIN"}, "t"); err != nil {
			t.Fatal(err)
		}
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: vault.Certificate().Raw}))
	caJSON, _ := json.Marshal(ca)
	body := func(confirm bool) string {
		b := `{"addr":"` + vault.URL + `","roleId":"` + sg3RoleID + `","secretId":"` + sg3SecretID + `","caPem":` + string(caJSON)
		if confirm {
			b += `,"confirmUnresolved":true`
		}
		return b + `}`
	}
	never := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		for _, s := range []string{sg3Value, sg3RoleID, sg3SecretID} {
			if strings.Contains(rec.Body.String(), s) {
				t.Errorf("%s answered %q: %s", what, s, rec.Body.String())
			}
		}
	}
	const fin = "/api/v1/agencies/ag:FIN/vault"

	// Who may read and change FIN's connection.
	for _, c := range []struct {
		group  string
		method string
		path   string
		body   string
		want   int
	}{
		{gFinAdmin, http.MethodGet, fin, "", http.StatusOK},
		{gRoot, http.MethodGet, fin, "", http.StatusOK},
		{gTaxAdmin, http.MethodGet, fin, "", http.StatusForbidden},
		{gFinViewer, http.MethodGet, fin, "", http.StatusForbidden},
		{gTaxAdmin, http.MethodPut, fin, body(true), http.StatusForbidden},
		{gTaxAdmin, http.MethodPost, fin + "/preview", body(false), http.StatusForbidden},
		{gMixed, http.MethodPut, "/api/v1/agencies/ag:TAX/vault", body(true), http.StatusForbidden},
		{gRoot, http.MethodPut, "/api/v1/agencies/global/vault", body(true), http.StatusUnprocessableEntity},
		{gFinAdmin, http.MethodPost, "/api/v1/settings/vault/test", "", http.StatusForbidden},
		// Without a Vault of its own, FIN's prefixes are a global administrator's.
		{gFinAdmin, http.MethodPut, "/api/v1/agencies/ag:FIN/vault-prefixes", `{"prefixes":["secret/data"]}`, http.StatusForbidden},
	} {
		if rec := gateReq(t, h, c.method, c.path, c.group, c.body); rec.Code != c.want {
			t.Errorf("%s %s %s = %d, want %d: %s", c.group, c.method, c.path, rec.Code, c.want, rec.Body)
		}
	}

	// The preview: one reference resolves, one does not, and no value is answered.
	rec := gateReq(t, h, http.MethodPost, fin+"/preview", gFinAdmin, body(false))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d: %s", rec.Code, rec.Body)
	}
	never("preview", rec)
	var prev struct {
		References []struct {
			Path     string `json:"path"`
			Resolves bool   `json:"resolves"`
		} `json:"references"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &prev)
	if len(prev.References) != 2 {
		t.Fatalf("preview references = %+v", prev.References)
	}
	for _, r := range prev.References {
		if r.Resolves == strings.Contains(r.Path, "missing") {
			t.Errorf("preview says %s resolves=%v", r.Path, r.Resolves)
		}
	}
	never("test", gateReq(t, h, http.MethodPost, fin+"/test", gFinAdmin, body(false)))

	// A save with an unresolved reference needs the confirmation.
	rec = gateReq(t, h, http.MethodPut, fin, gFinAdmin, body(false))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "references_unresolved") {
		t.Fatalf("save unconfirmed = %d: %s", rec.Code, rec.Body)
	}
	never("refused save", rec)
	if rec = gateReq(t, h, http.MethodPut, fin, gFinAdmin, body(true)); rec.Code != http.StatusOK {
		t.Fatalf("save confirmed = %d: %s", rec.Code, rec.Body)
	}
	never("save", rec)
	rec = gateReq(t, h, http.MethodGet, fin, gFinAdmin, "")
	never("read", rec)
	if !strings.Contains(rec.Body.String(), `"connected":true`) {
		t.Errorf("read after save: %s", rec.Body)
	}

	// With a Vault of its own and no prefixes, FIN names any path in it (SG-26).
	if rec = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_ANY","source":"vault","scope":"fin-hosts","vaultPath":"kv/anything/at/all#k"}`); rec.Code != http.StatusCreated {
		t.Errorf("FIN's Vault secret at any path, own Vault, no prefixes = %d: %s", rec.Code, rec.Body)
	}

	// With a Vault of its own, FIN sets its own prefixes; TAX still may not.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault-prefixes", gFinAdmin, `{"prefixes":["secret/data"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ownVault":true`) {
		t.Errorf("FIN's prefixes with its own Vault = %d: %s", rec.Code, rec.Body)
	}
	if rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault-prefixes", gTaxAdmin, `{"prefixes":[]}`); rec.Code != http.StatusForbidden {
		t.Errorf("TAX setting FIN's prefixes = %d", rec.Code)
	}

	// Once it has prefixes, they limit it.
	if rec = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_OUT","source":"vault","scope":"fin-hosts","vaultPath":"kv/elsewhere#k"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("FIN's Vault secret outside its own prefixes = %d: %s", rec.Code, rec.Body)
	}

	// Disconnect refused while FIN holds Vault-backed rows.
	if rec = gateReq(t, h, http.MethodDelete, fin, gFinAdmin, ""); rec.Code != http.StatusConflict {
		t.Errorf("disconnect in use = %d: %s", rec.Code, rec.Body)
	}
}
