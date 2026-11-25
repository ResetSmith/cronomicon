package aws

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore"
)

const (
	service        = "secretsmanager"
	requestTimeout = 15 * time.Second
	maxAnswerBytes = 1 << 20 // a secret is at most 64 KiB; the envelope is small
)

// client reads secrets with GetSecretValue. It is safe for concurrent use.
type client struct {
	endpoint string
	region   string
	cred     credentials
	http     *http.Client
	egress   httpx.EgressPolicy
	now      func() time.Time
}

func newClient(endpoint, region string, cred credentials, egress httpx.EgressPolicy) *client {
	return &client{
		endpoint: endpoint, region: region, cred: cred, egress: egress, now: time.Now,
		http: &http.Client{
			Timeout:   requestTimeout,
			Transport: httpx.SafeTransport(nil, egress),
			// A signed request is not followed anywhere else (SU-8).
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// awsError is the JSON error body Secrets Manager answers with.
type awsError struct {
	Type    string `json:"__type"`
	Message string `json:"message"`
	Msg     string `json:"Message"`
}

// authFailures are the error types that say the CREDENTIAL is wrong, not the
// secret: the store is unusable for every reference.
var authFailures = map[string]bool{
	"UnrecognizedClientException": true, "InvalidSignatureException": true,
	"ExpiredTokenException": true, "IncompleteSignature": true, "InvalidClientTokenId": true,
}

// ErrCredential wraps a refusal of the connection's credential.
var ErrCredential = fmt.Errorf("%w: AWS refused the connection's credential", secretstore.ErrStoreUnusable)

// ErrUnreachable wraps a failure to reach the endpoint at all.
var ErrUnreachable = fmt.Errorf("%w: AWS Secrets Manager did not answer", secretstore.ErrStoreUnusable)

// Fetch reads a reference: "<name-or-ARN>" or "<name-or-ARN>#<json-key>".
func (c *client) Fetch(ref string) (string, error) {
	if err := (Provider{}).ValidateRef(ref); err != nil {
		return "", err
	}
	id, key := split(ref)
	body, _ := json.Marshal(map[string]string{"SecretId": id})
	req, err := http.NewRequest(http.MethodPost, c.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "secretsmanager.GetSecretValue")
	sign(req, body, c.cred, c.region, service, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e awsError
		_ = json.Unmarshal(raw, &e)
		typ := e.Type
		if i := strings.LastIndex(typ, "#"); i >= 0 {
			typ = typ[i+1:]
		}
		msg := e.Message
		if msg == "" {
			msg = e.Msg
		}
		if typ == "" {
			typ = resp.Status
		}
		if authFailures[typ] {
			return "", fmt.Errorf("%w: %s: %s", ErrCredential, typ, msg)
		}
		if resp.StatusCode >= 500 {
			return "", fmt.Errorf("%w: %s: %s", ErrUnreachable, typ, msg)
		}
		return "", fmt.Errorf("aws secrets manager: %s: %s (%s)", typ, msg, id)
	}
	var out struct {
		SecretString *string `json:"SecretString"`
		SecretBinary *string `json:"SecretBinary"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("aws secrets manager: an answer that is not JSON for %s", id)
	}
	if out.SecretString == nil {
		if out.SecretBinary != nil {
			return "", fmt.Errorf("%w (%s)", ErrBinary, id)
		}
		return "", fmt.Errorf("aws secrets manager: no value for %s", id)
	}
	if key == "" {
		return *out.SecretString, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(*out.SecretString), &obj); err != nil {
		return "", fmt.Errorf("aws secrets manager: %s is not a JSON object, so it has no key %q", id, key)
	}
	v, ok := obj[key]
	if !ok {
		return "", fmt.Errorf("aws secrets manager: key %q not present in %s", key, id)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("aws secrets manager: key %q in %s is not a string", key, id)
	}
	return s, nil
}

// Configured is always true for a built client.
func (c *client) Configured() bool { return true }

// Health asks whether the endpoint answers a TLS handshake, under the same
// egress rules as a read, and sends no request: AWS has no unauthenticated
// health path, and the check runs twice a minute (SM-6, question 224).
func (c *client) Health() error {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return err
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "443")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		return fmt.Errorf("%w: no dialer", ErrUnreachable)
	}
	conn, err := tr.DialContext(ctx, "tcp", host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer conn.Close()
	cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if tr.TLSClientConfig != nil {
		cfg = tr.TLSClientConfig.Clone()
		cfg.ServerName = u.Hostname()
	}
	tc := tls.Client(conn, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	return nil
}

var _ secrets.VaultClient = (*client)(nil)
