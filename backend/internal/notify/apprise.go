package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Apprise sends pushes through an Apprise API gateway (the Phase B sidecar).
// Gateway is the base URL (e.g. http://apprise:8000); the stateless /notify
// endpoint takes the comma-joined Apprise service URLs plus a title and body.
type Apprise struct {
	Gateway string
	Client  *http.Client
}

// Send posts a notification to the gateway. targets are Apprise service URLs
// (e.g. "slack://...", "mailto://..."). Returns an error on non-2xx or transport
// failure so the dispatcher can log it.
func (a Apprise) Send(ctx context.Context, targets []string, title, body string) error {
	if a.Gateway == "" {
		return fmt.Errorf("apprise gateway not configured")
	}
	if len(targets) == 0 {
		return fmt.Errorf("no apprise targets")
	}
	payload, _ := json.Marshal(map[string]string{
		"urls":  strings.Join(targets, ","),
		"title": title,
		"body":  body,
		"type":  "warning",
	})
	url := strings.TrimRight(a.Gateway, "/") + "/notify"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("apprise request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("apprise post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("apprise gateway returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}
