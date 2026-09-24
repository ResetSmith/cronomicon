package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestSendTestNotificationEndpoint covers K-5's HTTP surface.
//
// The interesting properties are not "it sends" — internal/notify pins that —
// but the contract around it: that a transport refusing is a 200 with a report
// rather than a 500, that the request carries no recipient anyone could inject,
// and that the send is audited because it reaches real inboxes.
func TestSendTestNotificationEndpoint(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	post := func(t *testing.T, withCSRF bool) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/settings/notifications/test", bytes.NewReader(nil))
		req.Header.Set("Content-Type", "application/json")
		if withCSRF {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(raw)
	}

	t.Run("CSRF is required", func(t *testing.T) {
		// It causes an outbound send, so it must not be triggerable cross-site.
		if code, body := post(t, false); code != http.StatusForbidden {
			t.Fatalf("without a CSRF token = %d, want 403 — %s", code, body)
		}
	})

	t.Run("an unconfigured install reports skips, not an error", func(t *testing.T) {
		code, body := post(t, true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — %s", code, body)
		}
		var report struct {
			Results []struct {
				Transport string `json:"transport"`
				Sent      bool   `json:"sent"`
				Skipped   bool   `json:"skipped"`
				Detail    string `json:"detail"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(body), &report); err != nil {
			t.Fatalf("decode: %v — %s", err, body)
		}
		if len(report.Results) != 2 {
			t.Fatalf("got %d transport results, want 2 (email + apprise) — %s", len(report.Results), body)
		}
		for _, res := range report.Results {
			if res.Sent {
				t.Errorf("%s claims to have sent on a test server with no mail config", res.Transport)
			}
			if !res.Skipped {
				t.Errorf("%s = %+v, want skipped on an unconfigured install", res.Transport, res)
			}
			if res.Detail == "" {
				t.Errorf("%s was skipped without saying why", res.Transport)
			}
		}
	})

	t.Run("the send is audited", func(t *testing.T) {
		if code, body := post(t, true); code != http.StatusOK {
			t.Fatalf("status = %d — %s", code, body)
		}
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/change-log?pageSize=50", nil)
		r, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET change-log: %v", err)
		}
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("Notifications")) || !bytes.Contains(raw, []byte("test")) {
			t.Errorf("no Notifications/test row in the change log — a send to real inboxes must be attributable: %s", raw)
		}
	})
}
