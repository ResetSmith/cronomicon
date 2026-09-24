package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAlertRuleValidationRefusesInertValues covers J-3 (VF-15).
//
// Narrowing the UI is not enough: POST /alerts accepted `tag` targeting, an
// `n-failures-window` trigger and slack/webhook/in-app channels, and the
// dispatcher had a branch for none of them. Any client — a script, a curl, the
// old bundle in someone's cached tab — could still create a rule that persisted,
// listed, and produced nothing. Each rejected case below used to return 201 and
// then never fire.
func TestAlertRuleValidationRefusesInertValues(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	valid := func() map[string]any {
		return map[string]any{
			"targetMode": "all",
			"trigger":    "failure",
			"channels":   []string{"email"},
			"recipients": "ops@example.com",
			"enabled":    true,
		}
	}

	// Positive control first — if this fails, the rejections below prove nothing.
	if code, body := postAlert(t, client, csrf, ts, valid()); code != http.StatusCreated {
		t.Fatalf("a valid rule was rejected: %d — %s", code, body)
	}

	rejected := []struct {
		name     string
		mutate   func(map[string]any)
		wantCode string
	}{
		{"tag targeting", func(m map[string]any) {
			m["targetMode"] = "tag"
			m["jobTags"] = []string{"prod"}
		}, "invalid_target_mode"},
		{"scope targeting (what the demo data seeded)", func(m map[string]any) {
			m["targetMode"] = "scope"
		}, "invalid_target_mode"},
		{"n-failures-window trigger", func(m map[string]any) {
			m["trigger"] = "n-failures-window"
			m["triggerCount"] = 3
			m["triggerWindow"] = "1h"
		}, "invalid_trigger"},
		{"a non-terminal status as trigger", func(m map[string]any) {
			m["trigger"] = "running"
		}, "invalid_trigger"},
		{"slack channel", func(m map[string]any) {
			m["channels"] = []string{"slack"}
		}, "invalid_channel"},
		{"webhook channel", func(m map[string]any) {
			m["channels"] = []string{"webhook"}
		}, "invalid_channel"},
		{"in-app channel", func(m map[string]any) {
			m["channels"] = []string{"in-app"}
		}, "invalid_channel"},
		{"one good channel does not excuse a dead one", func(m map[string]any) {
			m["channels"] = []string{"email", "slack"}
		}, "invalid_channel"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			body := valid()
			tc.mutate(body)
			code, raw := postAlert(t, client, csrf, ts, body)
			if code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 — %s", code, raw)
			}
			if !strings.Contains(raw, tc.wantCode) {
				t.Errorf("error code missing %q: %s", tc.wantCode, raw)
			}
		})
	}

	// The values that DO work, including the two the UI could never reach before:
	// `apprise` — the one working non-email transport — and a `warning` trigger,
	// which the seeded backup-warnings rule proves fires.
	accepted := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"apprise channel", func(m map[string]any) { m["channels"] = []string{"apprise"} }},
		{"warning trigger", func(m map[string]any) { m["trigger"] = "warning" }},
		{"killed trigger", func(m map[string]any) { m["trigger"] = "killed" }},
		{"any trigger", func(m map[string]any) { m["trigger"] = "any" }},
		{"job targeting", func(m map[string]any) {
			m["targetMode"] = "job"
			m["jobName"] = "nightly-db-backup"
		}},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			body := valid()
			tc.mutate(body)
			if code, raw := postAlert(t, client, csrf, ts, body); code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 — %s", code, raw)
			}
		})
	}
}

// TestAlertRuleUpdateValidatesToo — the same gate on PUT. Without it a rule can
// be created clean and then edited into an inert state, which is the likelier
// path in practice: the edit form is where an operator spends their time.
func TestAlertRuleUpdateValidatesToo(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	code, raw := postAlert(t, client, csrf, ts, map[string]any{
		"targetMode": "all", "trigger": "failure",
		"channels": []string{"email"}, "recipients": "ops@example.com", "enabled": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d — %s", code, raw)
	}
	// NB: the id is a UUID string (settings.AlertRule.ID), even though
	// openapi.yaml still declares `id: { type: integer }`. Recorded as VF-17 —
	// harmless at runtime, but the spec is wrong and the frontend's editId is
	// typed `number | null` on the strength of it.
	var made struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &made); err != nil {
		t.Fatalf("decode created rule: %v — %s", err, raw)
	}
	if made.ID == "" {
		t.Fatalf("create returned no id: %s", raw)
	}

	bad, _ := json.Marshal(map[string]any{
		"targetMode": "all", "trigger": "failure",
		"channels": []string{"in-app"}, "recipients": "ops@example.com", "enabled": true,
	})
	req, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/v1/alerts/%s", ts.URL, made.ID), bytes.NewReader(bad))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with an in-app channel = %d, want 422 — %s", r.StatusCode, body)
	}
	if !strings.Contains(string(body), "invalid_channel") {
		t.Errorf("error code missing invalid_channel: %s", body)
	}
}

func postAlert(t *testing.T, client *http.Client, csrf string, ts *httptest.Server, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/alerts", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /alerts: %v", err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	return r.StatusCode, string(raw)
}
