package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// errReaped signals that the server returned 404 for this runner's poll — it
// was reaped or deregistered (D4). The lifecycle loop discards the stored
// identity and re-registers.
var errReaped = fmt.Errorf("runner not found on server (reaped/deregistered)")

// errIdentityRejected signals that the server returned 401 for this runner's
// poll: the per-runner token is no longer accepted (the runner was deleted, or
// the server's token store was reset/rebuilt). Like errReaped this is terminal
// for the current identity, so the loop discards it and re-registers instead of
// polling a dead token forever (which otherwise strands the runner invisibly,
// as only a 404 previously triggered recovery).
var errIdentityRejected = fmt.Errorf("runner token rejected by server (401 — deleted or token store reset)")

// errProtocolTooOld signals that the server returned 426 for this runner's poll
// — this binary speaks an older wire protocol than the server's floor. Unlike
// errReaped and errIdentityRejected this is NOT an identity problem, so the
// recovery is NOT to re-register: a fresh registration declares the same old
// protocol and is refused the same way. The identity file is kept, the agent
// keeps polling (and keeps saying so in its log), and the fix — upgrading this
// binary — resolves it with no operator surgery on the server side.
var errProtocolTooOld = fmt.Errorf("server refused this agent's wire protocol (426 — upgrade the cronomicon-runner binary)")

// errNoWork signals a poll that returned 204 No Content (no assignment, no
// control). Not an error condition — the loop just waits for the next tick.
var errNoWork = fmt.Errorf("no work")

// Client is the agent's HTTP transport to the Cronomicon server. It speaks the
// runnerproto wire types and carries the per-runner bearer once registered.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a Client. caCertPath, if set, is a PEM CA bundle to trust
// (otherwise the system store is used). pollHoldTimeout bounds a single poll
// request (the server long-polls ~30s, so this must exceed that).
func NewClient(baseURL, caCertPath string) (*Client, error) {
	transport := &http.Transport{
		// A fresh dialer per request would defeat keep-alive; defaults are fine.
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if caCertPath != "" {
		pem, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA cert %q: %w", caCertPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA cert %q contained no valid certificates", caCertPath)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		baseURL: baseURL,
		// No global timeout: the poll long-polls and log streams are long-lived;
		// per-request deadlines are applied via context instead.
		http: &http.Client{Transport: transport},
	}, nil
}

// declareBody builds the declared-config JSON shared by Register and Redeclare:
// protocolVersion, the inventory mode (R2.4), the WIDENED capability tokens
// (run-types + detected checkout/vault/collection tokens, RX.7), and the
// toolchain display detail.
func declareBody(cfg Config, caps []string, tc Toolchains) []byte {
	buf, _ := json.Marshal(map[string]any{
		"name":            cfg.Name,
		"os":              cfg.OS,
		"capabilities":    caps,
		"version":         agentVersion,
		"maxConcurrent":   cfg.MaxConcurrent,
		"inventory":       cfg.Inventory,
		"protocolVersion": runnerproto.ProtocolVersion,
		"toolchains":      tc,
	})
	return buf
}

// Register performs POST /api/v1/runners/register with the shared registration
// token and returns the minted identity.
func (c *Client) Register(ctx context.Context, cfg Config, caps []string, tc Toolchains) (Identity, error) {
	buf := declareBody(cfg, caps, tc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/runners/register", bytes.NewReader(buf))
	if err != nil {
		return Identity{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.RegistrationToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("register request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return Identity{}, fmt.Errorf("register: server returned %s: %s",
			resp.Status, readSnippet(resp.Body))
	}
	var out struct {
		Runner struct {
			ID string `json:"id"`
		} `json:"runner"`
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, fmt.Errorf("decode register response: %w", err)
	}
	if out.Runner.ID == "" || out.APIKey == "" {
		return Identity{}, fmt.Errorf("register response missing id/apiKey")
	}
	return Identity{ID: out.Runner.ID, APIKey: out.APIKey}, nil
}

// Redeclare performs POST /api/v1/runners/{id}/redeclare (protocol v4) — the
// id-preserving re-declaration triggered by the "re-register" control op. It
// authenticates with the runner's EXISTING API key, never a registration token
// (registration tokens are first-contact-only); the identity is unchanged.
// A 404 maps to errReaped: the row is gone, and the caller falls back to the
// discard-identity + fresh-register path.
// UploadHostKeys posts the keys the agent scanned for operator approval (Phase
// 5), authenticated with the runner's own key.
// POST /api/v1/runners/{id}/hostkeys
// ReportFileSightings uploads observed file arrivals (ET-D). Mirrors
// UploadHostKeys: the agent reports what it saw and the server decides what
// that means — the agent never triggers a run itself.
func (c *Client) ReportFileSightings(ctx context.Context, id Identity, sightings []sightingOut) error {
	buf, err := json.Marshal(map[string]any{"sightings": sightings})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/runners/"+id.ID+"/file-sightings", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("file-sightings request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("file-sightings: server returned %s: %s", resp.Status, readSnippet(resp.Body))
	}
	return nil
}

func (c *Client) UploadHostKeys(ctx context.Context, id Identity, entries []scannedHostKey) error {
	buf, err := json.Marshal(map[string]any{"entries": entries})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/runners/"+id.ID+"/hostkeys", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hostkeys upload request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hostkeys upload: server returned %s: %s", resp.Status, readSnippet(resp.Body))
	}
	return nil
}

func (c *Client) Redeclare(ctx context.Context, id Identity, cfg Config, caps []string, tc Toolchains) error {
	buf := declareBody(cfg, caps, tc)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/runners/"+id.ID+"/redeclare", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("redeclare request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return errReaped
	default:
		return fmt.Errorf("redeclare: server returned %s: %s",
			resp.Status, readSnippet(resp.Body))
	}
}

// Poll performs GET /api/v1/runners/{id}/poll. The server holds the request
// ~30s. configDigest, when non-empty, rides as a query param so the server
// can detect declared-config drift and request a re-register (Phase 5).
// settingsVersion is the managed-settings version the agent has APPLIED; it
// ALWAYS rides (even 0), which is how the server knows the agent is
// settings-capable (Phase 4) and can safely send it settings.
// Returns:
//   - (resp, nil)         on 200 with a body (may carry an assignment/control)
//   - (nil, errNoWork)    on 204 No Content
//   - (nil, errReaped)    on 404 (runner reaped/deregistered — re-register)
//   - (nil, errProtocolTooOld) on 426 (this binary is below the server's floor)
func (c *Client) Poll(ctx context.Context, id Identity, configDigest string, settingsVersion int) (*runnerproto.PollResponse, error) {
	q := url.Values{}
	if configDigest != "" {
		q.Set("configDigest", configDigest)
	}
	// Always present (even "0") so the server can tell a Phase-4 agent from an
	// older one and only then include managed settings in the response.
	q.Set("settingsVersion", strconv.Itoa(settingsVersion))
	pollURL := c.baseURL + "/api/v1/runners/" + id.ID + "/poll?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("poll request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, errNoWork
	case http.StatusNotFound:
		return nil, errReaped
	case http.StatusUnauthorized:
		return nil, errIdentityRejected
	case http.StatusUpgradeRequired:
		// Carry the server's own sentence: it names both versions, which is the
		// whole diagnostic. Wrapped so the caller can still match the sentinel.
		return nil, fmt.Errorf("%w: %s", errProtocolTooOld, readSnippet(resp.Body))
	case http.StatusOK:
		var pr runnerproto.PollResponse
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			return nil, fmt.Errorf("decode poll response: %w", err)
		}
		return &pr, nil
	default:
		return nil, fmt.Errorf("poll: server returned %s: %s", resp.Status, readSnippet(resp.Body))
	}
}

// CheckHealth GETs /healthz for the doctor preflight. It distinguishes the
// failure modes that stalled real deployments: unreachable/TLS (transport
// error), a non-200, and — the SSO-proxy case — a 200/redirect whose body is an
// HTML login page rather than the app's health response.
func (c *Client) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	snippet := readSnippet(resp.Body)
	looksHTML := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "html") ||
		strings.Contains(strings.ToLower(snippet), "<!doctype") || strings.Contains(strings.ToLower(snippet), "<html")
	if looksHTML {
		return fmt.Errorf("got HTTP %d with an HTML body — a reverse proxy is intercepting runner endpoints (expected a plain health response). Add the runner paths to the proxy auth-bypass; see the runner install guide", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned HTTP %d: %s", resp.StatusCode, snippet)
	}
	return nil
}

// Manifest performs GET /api/v1/runs/{traceId}/manifest. Only the owning runner
// may fetch it (else 404).
func (c *Client) Manifest(ctx context.Context, id Identity, traceID string) (*runnerproto.ManifestResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL+"/api/v1/runs/"+traceID+"/manifest", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manifest request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest %s: server returned %s: %s",
			traceID, resp.Status, readSnippet(resp.Body))
	}
	var m runnerproto.ManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, nil
}

// postLogResult reports the outcome of a single log-POST attempt.
type postLogResult struct {
	// committed is the absolute byte offset the server has persisted after this
	// attempt (the offset to resume from if a later attempt is needed).
	committed int64
	// resumeMismatch is true when the server replied 409 with a persistedOffset
	// that differs from the offset we sent; persistedOffset carries the truth.
	resumeMismatch  bool
	persistedOffset int64
}

// postLog streams a chunk to POST /api/v1/runs/{traceId}/log starting at
// resumeOffset. body is the raw newline-delimited payload for THIS attempt
// (already offset-trimmed by the caller). On a 409 it parses persistedOffset so
// the caller can re-slice and retry from exactly there (R2.3). partial marks a
// mid-run flush (v12 live tailing): the server persists + advances the offset
// but does not treat the chunk's last line as the trailing envelope.
func (c *Client) postLog(ctx context.Context, id Identity, traceID string,
	resumeOffset int64, body io.Reader, partial bool) (postLogResult, error) {

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/api/v1/runs/"+traceID+"/log", body)
	if err != nil {
		return postLogResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+id.APIKey)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	// Always send the offset (even 0) so the server can detect a mismatch and
	// reply 409 with persistedOffset — without it the server blindly appends,
	// which would duplicate bytes if it already has some (R2.3).
	req.Header.Set("X-Resume-Offset", strconv.FormatInt(resumeOffset, 10))
	if partial {
		req.Header.Set("X-Log-Partial", "1")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return postLogResult{}, fmt.Errorf("log post: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return postLogResult{committed: resumeOffset}, nil
	case http.StatusConflict:
		// Resume-offset mismatch: parse persistedOffset and let the caller
		// re-slice from there.
		var c409 struct {
			PersistedOffset int64 `json:"persistedOffset"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&c409)
		return postLogResult{resumeMismatch: true, persistedOffset: c409.PersistedOffset}, nil
	default:
		return postLogResult{}, fmt.Errorf("log post: server returned %s: %s",
			resp.Status, readSnippet(resp.Body))
	}
}

// readSnippet reads a bounded prefix of an error body for diagnostics.
func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	return string(bytes.TrimSpace(b))
}
