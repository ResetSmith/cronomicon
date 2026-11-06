package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/secretstore/aws/fakeaws"
)

// 2.5.5 SM4 — the agency's secret store connection through the API: any
// provider, the AWS test by a read, the refusal to change the kind of store
// while rows name it, and the reference check the forms ask before saving.

func smAWSBody(fake *fakeaws.Server, keyID, example string) string {
	b, _ := json.Marshal(map[string]any{
		"provider":   "aws",
		"settings":   map[string]string{"region": "eu-west-1", "endpoint": fake.URL, "caPem": fake.CAPEM()},
		"credential": map[string]string{"accessKeyId": keyID, "secretAccessKey": "s"},
		"exampleRef": example,
	})
	return string(b)
}

func smAllowHosts(t *testing.T, e *sg6Env, urls ...string) {
	t.Helper()
	var hosts []string
	for _, u := range urls {
		p, _ := url.Parse(u)
		hosts = append(hosts, `"`+p.Host+`"`)
	}
	mustExec(t, e.pool)(`UPDATE settings SET value = ? WHERE key = 'vaultAllowedHosts'`, "["+strings.Join(hosts, ",")+"]")
}

func TestSM4_AnAgencyConnectsAWS(t *testing.T) {
	e := newSG6Env(t)
	fake := fakeaws.New("AKIDTEST", map[string]string{"cronomicon/db": `{"password":"p"}`})
	t.Cleanup(fake.Close)
	smAllowHosts(t, e, fake.URL)

	// No reference to read yet: the save asks for one.
	rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, smAWSBody(fake, "AKIDTEST", ""))
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "example_ref_required" {
		t.Fatalf("an AWS connection with nothing to read = %d %s, want 422 example_ref_required", rec.Code, rec.Body)
	}
	// A refused credential is the store's refusal, whatever the example.
	rec = gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/test", gFinAdmin, smAWSBody(fake, "WRONG", "cronomicon/db#password"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":false`) {
		t.Fatalf("testing a wrong credential = %d %s, want ok false", rec.Code, rec.Body)
	}
	rec = gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/test", gFinAdmin, smAWSBody(fake, "AKIDTEST", "cronomicon/db#password"))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("testing the right one = %d %s, want ok true", rec.Code, rec.Body)
	}
	rec = gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, smAWSBody(fake, "AKIDTEST", "cronomicon/db#password"))
	if rec.Code != http.StatusOK {
		t.Fatalf("connecting AWS = %d %s", rec.Code, rec.Body)
	}
	var got struct {
		Vault struct {
			Provider      string            `json:"provider"`
			Settings      map[string]string `json:"settings"`
			CredentialSet map[string]bool   `json:"credentialSet"`
			CAPresent     bool              `json:"caPresent"`
		} `json:"vault"`
	}
	rec = gateReq(t, e.h, http.MethodGet, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Vault.Provider != "aws" || got.Vault.Settings["region"] != "eu-west-1" || !got.Vault.CredentialSet["secretAccessKey"] || !got.Vault.CAPresent {
		t.Fatalf("GET = %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"s"`) || strings.Contains(rec.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("GET returns the credential or the CA: %s", rec.Body)
	}
	// A re-save with a blank credential keeps the stored one.
	blank, _ := json.Marshal(map[string]any{"provider": "aws",
		"settings": map[string]string{"region": "eu-west-1", "endpoint": fake.URL}, "exampleRef": "cronomicon/db#password"})
	if rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, string(blank)); rec.Code != http.StatusOK {
		t.Fatalf("a re-save keeping the credential and CA = %d %s", rec.Code, rec.Body)
	}

	// The prefixes answer says how to ask for a reference.
	rec = gateReq(t, e.h, http.MethodGet, "/api/v1/agencies/ag:FIN/vault-prefixes", gFinAdmin, "")
	if !strings.Contains(rec.Body.String(), `"provider":"aws"`) || !strings.Contains(rec.Body.String(), "Secret name or ARN") {
		t.Fatalf("vault-prefixes = %s", rec.Body)
	}
	// The form's check: AWS's grammar now.
	for _, c := range []struct{ ref, want string }{
		{"cronomicon/db#password", `"ok":true`},
		{"cronomicon//db", `"code":"validation_failed"`},
	} {
		rec := gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/validate-ref", gFinAdmin, `{"ref":"`+c.ref+`"}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), c.want) {
			t.Errorf("validate-ref %q = %d %s, want %s", c.ref, rec.Code, rec.Body, c.want)
		}
	}
	// Another agency's administrator may not ask.
	if rec := gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/validate-ref", gTaxAdmin, `{"ref":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("TAX asking about FIN's references = %d, want 403", rec.Code)
	}
}

// SM-9: the kind of store does not change while rows name it; the same kind
// with new settings does.
func TestSM4_TheKindOfStoreDoesNotChangeUnderItsRows(t *testing.T) {
	v := newSG6Vault(t, "from-vault")
	e := newSG6Env(t, v)
	e.connect(t, "ag:FIN", gFinAdmin, v)
	fake := fakeaws.New("AKIDTEST", map[string]string{"cronomicon/db": "x"})
	t.Cleanup(fake.Close)
	smAllowHosts(t, e, v.URL, fake.URL)
	e.vaultSecret(t, "FIN_DB", "secret/data/fin/db#value", "ag:FIN")

	rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, smAWSBody(fake, "AKIDTEST", "cronomicon/db"))
	if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "vault_in_use" || !strings.Contains(rec.Body.String(), "(1)") {
		t.Fatalf("changing to AWS with a row in Vault = %d %s, want 409 vault_in_use (1)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, e.connectBody(v, "")); rec.Code != http.StatusOK {
		t.Fatalf("re-saving the same Vault = %d %s", rec.Code, rec.Body)
	}
	mustExec(t, e.pool)(`DELETE FROM secrets WHERE owner_agency = 'ag:FIN'`)
	if rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, smAWSBody(fake, "AKIDTEST", "cronomicon/db")); rec.Code != http.StatusOK {
		t.Fatalf("changing to AWS with no rows = %d %s", rec.Code, rec.Body)
	}
}
