package aws

import (
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
)

const (
	testKeyID  = "AKIDTEST"
	testSecret = "test/secret/key"
	testRegion = "eu-west-1"
)

// fakeSecretsManager answers GetSecretValue for the secrets in store, and
// checks each request's signature again from what arrived on the wire.
func fakeSecretsManager(t *testing.T, store map[string]map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fail := func(code int, typ, msg string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"__type": typ, "message": msg})
		}
		if r.Header.Get("X-Amz-Target") != "secretsmanager.GetSecretValue" {
			fail(400, "UnknownOperationException", r.Header.Get("X-Amz-Target"))
			return
		}
		if !signatureHolds(r, body, testSecret) {
			fail(403, "InvalidSignatureException", "The request signature we calculated does not match")
			return
		}
		var in struct{ SecretId string }
		_ = json.Unmarshal(body, &in)
		sec, ok := store[in.SecretId]
		if !ok {
			fail(400, "ResourceNotFoundException", "Secrets Manager can't find the specified secret.")
			return
		}
		_ = json.NewEncoder(w).Encode(sec)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signatureHolds signs the arrived request again with the server's copy of
// the key and the client's own date, over the headers the client signed.
func signatureHolds(r *http.Request, body []byte, secret string) bool {
	auth := r.Header.Get("Authorization")
	i := strings.Index(auth, "SignedHeaders=")
	j := strings.Index(auth, ", Signature=")
	if i < 0 || j < 0 || !strings.Contains(auth, "Credential="+testKeyID+"/") {
		return false
	}
	signed := strings.Split(auth[i+len("SignedHeaders="):j], ";")
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return false
	}
	re, _ := http.NewRequest(r.Method, "https://"+r.Host+r.URL.RequestURI(), nil)
	for _, h := range signed {
		if h == "host" || h == "x-amz-date" {
			continue
		}
		re.Header.Set(h, r.Header.Get(h))
	}
	re.Header.Set("Host", r.Host)
	sign(re, body, credentials{AccessKeyID: testKeyID, SecretAccessKey: secret, SessionToken: r.Header.Get("X-Amz-Security-Token")}, testRegion, service, at)
	return re.Header.Get("Authorization") == auth
}

func testClient(t *testing.T, srv *httptest.Server, secret string) *client {
	t.Helper()
	c, err := Provider{}.NewClient(secretstore.Connection{
		Settings:   map[string]string{SettingRegion: testRegion, SettingEndpoint: srv.URL},
		Credential: map[string]string{CredAccessKeyID: testKeyID, CredSecretAccessKey: secret},
	}, httpx.EgressPolicy{AllowLoopback: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	cl := c.(*client)
	cl.http.Transport.(*http.Transport).TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	return cl
}

// TestACABundleIsTrusted: an endpoint of one's own is reached with its CA.
func TestACABundleIsTrusted(t *testing.T) {
	srv := fakeSecretsManager(t, map[string]map[string]string{"x": {"SecretString": "v"}})
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	c, err := Provider{}.NewClient(secretstore.Connection{
		Settings:   map[string]string{SettingRegion: testRegion, SettingEndpoint: srv.URL, SettingCAPEM: ca},
		Credential: map[string]string{CredAccessKeyID: testKeyID, CredSecretAccessKey: testSecret},
	}, httpx.EgressPolicy{AllowLoopback: true, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := c.Fetch("x"); err != nil || v != "v" {
		t.Fatalf("Fetch = %q, %v", v, err)
	}
}

func TestFetch(t *testing.T) {
	str := func(s string) map[string]string { return map[string]string{"SecretString": s} }
	srv := fakeSecretsManager(t, map[string]map[string]string{
		"cronomicon/db":   str(`{"password":"s3cr3t","port":5432}`),
		"cronomicon/raw":  str("plain value"),
		"cronomicon/blob": {"SecretBinary": "AAEC"},
		"arn:aws:secretsmanager:eu-west-1:123456789012:secret:cronomicon/db-AbCdEf": str(`{"password":"by-arn"}`),
	})
	c := testClient(t, srv, testSecret)
	for _, tc := range []struct{ ref, want, err string }{
		{"cronomicon/db#password", "s3cr3t", ""},
		{"cronomicon/raw", "plain value", ""},
		{"arn:aws:secretsmanager:eu-west-1:123456789012:secret:cronomicon/db-AbCdEf#password", "by-arn", ""},
		{"cronomicon/db#missing", "", "not present"},
		{"cronomicon/db#port", "", "not a string"},
		{"cronomicon/raw#k", "", "not a JSON object"},
		{"cronomicon/blob", "", "binary"},
		{"cronomicon/nope", "", "ResourceNotFoundException"},
		{"cronomicon/../db", "", "segment"},
	} {
		got, err := c.Fetch(tc.ref)
		switch {
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("%s = %q, %v; want %q", tc.ref, got, err, tc.want)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s = %q, %v; want an error with %q", tc.ref, got, err, tc.err)
		}
	}
}

// A wrong credential is the credential's error, which says the whole store
// is unusable, not that one secret is missing.
func TestAWrongCredentialIsErrCredential(t *testing.T) {
	srv := fakeSecretsManager(t, map[string]map[string]string{"x": {"SecretString": "v"}})
	if _, err := testClient(t, srv, "wrong").Fetch("x"); !errors.Is(err, ErrCredential) {
		t.Fatalf("err = %v, want ErrCredential", err)
	}
}

// SM-4: the metadata address is refused by the transport, whatever the
// endpoint says.
func TestTheMetadataAddressIsRefused(t *testing.T) {
	c, err := Provider{}.NewClient(secretstore.Connection{
		Settings:   map[string]string{SettingRegion: testRegion, SettingEndpoint: "https://169.254.169.254"},
		Credential: map[string]string{CredAccessKeyID: "a", CredSecretAccessKey: "b"},
	}, httpx.EgressPolicy{AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch("x"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Fetch through the metadata address = %v, want refused (ErrUnreachable)", err)
	}
	if err := c.(*client).Health(); err == nil {
		t.Fatal("Health through the metadata address succeeded")
	}
}

func TestHealthIsAHandshake(t *testing.T) {
	srv := fakeSecretsManager(t, nil)
	if err := testClient(t, srv, testSecret).Health(); err != nil {
		t.Fatalf("Health = %v", err)
	}
	srv.Close()
	if err := testClient(t, srv, testSecret).Health(); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Health of a closed endpoint = %v, want ErrUnreachable", err)
	}
}

func TestValidate(t *testing.T) {
	ok := secretstore.Connection{
		Settings:   map[string]string{SettingRegion: "us-east-1"},
		Credential: map[string]string{CredAccessKeyID: "a", CredSecretAccessKey: "b"},
	}
	if err := (Provider{}).Validate(ok); err != nil {
		t.Fatalf("valid = %v", err)
	}
	for name, mut := range map[string]func(c *secretstore.Connection){
		"no region":  func(c *secretstore.Connection) { c.Settings = map[string]string{} },
		"bad region": func(c *secretstore.Connection) { c.Settings = map[string]string{SettingRegion: "Moon"} },
		"http endpoint": func(c *secretstore.Connection) {
			c.Settings = map[string]string{SettingRegion: "us-east-1", SettingEndpoint: "http://x"}
		},
		"endpoint path": func(c *secretstore.Connection) {
			c.Settings = map[string]string{SettingRegion: "us-east-1", SettingEndpoint: "https://x/p"}
		},
		"no secret key": func(c *secretstore.Connection) { c.Credential = map[string]string{CredAccessKeyID: "a"} },
	} {
		c := secretstore.Connection{Settings: ok.Settings, Credential: ok.Credential}
		mut(&c)
		if err := (Provider{}).Validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
