package secrets

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// httpVaultClient is a dependency-free Vault client (KV v2) authenticated via
// AppRole or a static token, talking the Vault HTTP API directly — a
// deliberate "no Vault SDK" dependency decision. It is built but stays disabled unless
// the orchestrator wires it (a Vault addr plus credentials for the configured
// auth method); otherwise the stub remains.
//
// vaultRef format (matches the DB vault_ref contract): "<kvv2-path>#<field>",
// e.g. "secret/data/amadeus/app#DB_PASSWORD". The field after '#' selects one
// key from the secret's data map; if omitted it defaults to "value".
//
// Phase-2 hardening (D4) is layered via VaultOptions, each dormant until set:
// an X-Vault-Namespace header, a private-CA transport, bounded retry/backoff on
// transient failures, and response-wrapped secret_id acceptance.
type httpVaultClient struct {
	addr      string
	roleID    string
	secretID  string
	namespace string // X-Vault-Namespace, sent only when non-empty
	wrapped   bool   // secretID is a response-wrapping token, not the secret_id
	tokenAuth bool   // secretID IS the Vault token; skip the AppRole login entirely
	client    *http.Client

	mu sync.Mutex
	// realSecretID caches the unwrapped secret_id once a wrapping token has been
	// exchanged (a wrapping token is single-use, so it can only be unwrapped once —
	// the cached value is reused across token re-logins). Empty when !wrapped.
	realSecretID string
	token        string
	tokenExp     time.Time
}

// VaultOptions carries the Phase-2 client-hardening knobs (D4). The zero value
// reproduces the original client exactly (no namespace, system CA roots, static
// secret_id). Each field is independently activated by its own config being set.
type VaultOptions struct {
	// AuthMethod selects how the client obtains its Vault token: "approle" (the
	// zero value, and the default) logs in at auth/approle/login with the
	// role_id/secret_id pair; "token" treats the secretID argument as an
	// already-minted Vault token and performs no login at all. Token auth is the
	// simplest way to point Cronomicon at a Vault an operator already has a token
	// for; it does NOT self-renew, so a token that expires or is revoked makes
	// every reveal fail until the operator stores a new one.
	AuthMethod string
	// Namespace, when non-empty, is sent as X-Vault-Namespace on every request
	// (Vault Enterprise / HCP namespaces).
	Namespace string
	// CAFile is a path to a PEM CA bundle; when set the client trusts only it
	// (plus is pinned via a custom transport) instead of the system roots.
	CAFile string
	// SecretIDWrapped marks secretID as a response-wrapping token to be unwrapped
	// via sys/wrapping/unwrap before the first AppRole login.
	SecretIDWrapped bool
	// Egress is the SU-7 SSRF posture for the client transport. The zero value
	// blocks cloud-metadata/loopback/link-local (and, with AllowPrivate=false,
	// private ranges); production wires it from config, tests set AllowLoopback.
	Egress httpx.EgressPolicy
}

// vaultRetryMax bounds transient-failure retries (D4: "bounded retry/backoff on
// transient failures, always on"). L8: this is the TOTAL number of attempts the
// do-loop makes (the initial attempt plus up to vaultRetryMax-1 retries), not
// "first attempt plus this many" — the loop condition is attempt < vaultRetryMax.
const vaultRetryMax = 3

// NewVaultClientWithOptions builds the client with the Phase-2 hardening knobs.
// With opts.AuthMethod == "token", secretID is the Vault token itself and roleID
// is ignored. It returns an error when a configured CA bundle cannot be
// read/parsed — a fail-loud misconfiguration rather than a silent fall-back to
// system roots — or on an unsupported auth/hardening combination.
func NewVaultClientWithOptions(addr, roleID, secretID string, opts VaultOptions) (VaultClient, error) {
	tokenAuth := opts.AuthMethod == "token"
	// Response wrapping unwraps to an AppRole secret_id (the shape resolveSecretID
	// expects). Silently accepting it under token auth would send the single-use
	// WRAPPING token as X-Vault-Token on every request — 403 on every read, with
	// nothing in the error to say why. Refuse the combination instead.
	if tokenAuth && opts.SecretIDWrapped {
		return nil, fmt.Errorf("vault: response-wrapped credentials are AppRole-only, not supported with token auth")
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		// SU-8: never follow redirects. Go's stdlib strips only Authorization/
		// WWW-Authenticate/Cookie on a cross-host redirect, NOT custom headers, so a
		// redirect to an attacker-controlled host would carry X-Vault-Token with it —
		// exactly the exposure we refuse. Surface any 3xx as an error instead. (An HA
		// standby node answers 307 to redirect to the active node; point the Vault addr
		// at the active node / VIP / LB, standard practice.)
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	// SU-7: guard egress against SSRF. The CA path wraps the private-CA transport so
	// pinning and the guard compose; the no-CA path guards a clone of the default
	// transport. Blocks cloud-metadata/loopback/link-local (private per opts.Egress).
	if opts.CAFile != "" {
		tr, err := CATransport(opts.CAFile)
		if err != nil {
			return nil, err
		}
		client.Transport = httpx.SafeTransport(tr, opts.Egress)
	} else {
		client.Transport = httpx.SafeTransport(nil, opts.Egress)
	}
	return &httpVaultClient{
		addr:      strings.TrimRight(addr, "/"),
		roleID:    roleID,
		secretID:  secretID,
		namespace: strings.TrimSpace(opts.Namespace),
		wrapped:   opts.SecretIDWrapped,
		tokenAuth: tokenAuth,
		client:    client,
	}, nil
}

// CATransport builds an http.Transport that trusts only the CA bundle at caFile.
// L6: it CLONES http.DefaultTransport and overrides only TLSClientConfig, so a
// private-CA deployment keeps ProxyFromEnvironment, connection pooling/keep-alives,
// and HTTP/2 — a bare &http.Transport{} silently dropped all of these, notably
// bypassing an egress proxy.
func CATransport(caFile string) (*http.Transport, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("vault CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("vault CA file %q: no valid PEM certificates", caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return tr, nil
}

// Fetch reads the field from the KV v2 secret named by vaultRef.
func (v *httpVaultClient) Fetch(vaultRef string) (string, error) {
	path, field := splitRef(vaultRef)
	token, err := v.ensureToken()
	if err != nil {
		return "", err
	}
	var out struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := v.do(http.MethodGet, "/v1/"+path, token, nil, &out); err != nil {
		return "", err
	}
	val, ok := out.Data.Data[field]
	if !ok {
		return "", fmt.Errorf("vault: field %q not present at %q", field, path)
	}
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("vault: field %q at %q is not a string", field, path)
	}
	return s, nil
}

// Write stores value at the KV v2 secret named by vaultPath (field after '#').
func (v *httpVaultClient) Write(vaultPath, value string) error {
	path, field := splitRef(vaultPath)
	token, err := v.ensureToken()
	if err != nil {
		return err
	}
	body := map[string]any{"data": map[string]string{field: value}}
	return v.do(http.MethodPost, "/v1/"+path, token, body, nil)
}

// ensureToken returns a valid client token, logging in via AppRole when the
// cached token is missing or within the renewal margin of expiry. The lazy
// re-login here IS the token-refresh mechanism (D4): a request-driven client
// refreshes on demand, avoiding a background goroutine's lifecycle.
//
// Under token auth there is nothing to log in to: the operator-supplied token is
// used verbatim, with no expiry tracking and no renewal. Vault answers 403 once
// it lapses, which surfaces as the usual "vault ... status 403" on the reveal —
// the operator's cue to store a fresh token.
func (v *httpVaultClient) ensureToken() (string, error) {
	if v.tokenAuth {
		if v.secretID == "" {
			return "", fmt.Errorf("vault token auth: no token configured")
		}
		return v.secretID, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Now().Before(v.tokenExp) {
		return v.token, nil
	}
	secretID, err := v.resolveSecretID()
	if err != nil {
		return "", err
	}
	var login struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	body := map[string]string{"role_id": v.roleID, "secret_id": secretID}
	if err := v.do(http.MethodPost, "/v1/auth/approle/login", "", body, &login); err != nil {
		return "", fmt.Errorf("vault approle login: %w", err)
	}
	if login.Auth.ClientToken == "" {
		return "", fmt.Errorf("vault approle login returned no token")
	}
	v.token = login.Auth.ClientToken
	// Renew at 80% of the lease to avoid using a token right at expiry.
	ttl := time.Duration(login.Auth.LeaseDuration) * time.Second
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	v.tokenExp = time.Now().Add(ttl * 4 / 5)
	return v.token, nil
}

// resolveSecretID returns the AppRole secret_id, unwrapping a response-wrapping
// token once and caching the result (D4). Must be called with v.mu held.
func (v *httpVaultClient) resolveSecretID() (string, error) {
	if !v.wrapped {
		return v.secretID, nil
	}
	if v.realSecretID != "" {
		return v.realSecretID, nil
	}
	// A wrapping token is single-use: present it as the token to sys/wrapping/unwrap
	// and read the secret_id back out of the wrapped response's data.
	var out struct {
		Data struct {
			SecretID string `json:"secret_id"`
		} `json:"data"`
	}
	if err := v.do(http.MethodPost, "/v1/sys/wrapping/unwrap", v.secretID, nil, &out); err != nil {
		return "", fmt.Errorf("vault unwrap secret_id: %w", err)
	}
	if out.Data.SecretID == "" {
		return "", fmt.Errorf("vault unwrap secret_id: no secret_id in wrapped response")
	}
	v.realSecretID = out.Data.SecretID
	return v.realSecretID, nil
}

// do performs a Vault HTTP request, JSON-encoding reqBody and decoding into
// respOut (both optional). A non-2xx response is returned as an error. Transient
// failures (transport errors, 429, and 5xx) are retried with a bounded backoff.
func (v *httpVaultClient) do(method, path, token string, reqBody, respOut any) error {
	var payload []byte
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		payload = b
	}

	var lastErr error
	for attempt := range vaultRetryMax {
		if attempt > 0 {
			// Bounded exponential backoff: 100ms, 200ms, … capped implicitly by
			// vaultRetryMax. Cheap and safe — every retried request is idempotent
			// (KV reads, AppRole login, KV writes are last-write-wins).
			time.Sleep(time.Duration(1<<uint(attempt-1)) * 100 * time.Millisecond)
		}
		retry, err := v.doOnce(method, path, token, payload, reqBody != nil, respOut)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return lastErr
}

// doOnce issues a single request. retry reports whether err (when non-nil) is a
// transient condition worth retrying.
func (v *httpVaultClient) doOnce(method, path, token string, payload []byte, hasBody bool, respOut any) (retry bool, err error) {
	var rdr io.Reader
	if hasBody {
		rdr = bytes.NewReader(payload)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, v.addr+path, rdr)
	if err != nil {
		return false, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if v.namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.namespace)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := v.client.Do(req)
	if err != nil {
		// Transport-level failures (DNS, connection refused, timeout) are transient.
		return true, fmt.Errorf("vault unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		e := fmt.Errorf("vault %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(snippet)))
		// 429 (rate limited / standby) and 5xx (server/gateway) are transient; 4xx
		// (bad creds, missing path, permission) are not — retrying only wastes time.
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500, e
	}
	if respOut != nil {
		if err := json.NewDecoder(resp.Body).Decode(respOut); err != nil {
			return false, err
		}
	}
	return false, nil
}

// splitRef parses "<path>#<field>"; field defaults to "value" when absent.
func splitRef(ref string) (path, field string) {
	if i := strings.LastIndex(ref, "#"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, "value"
}
