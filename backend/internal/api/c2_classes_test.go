package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 2.5.0 C2: the routes whose class the owner settled (questions 68 to 71, and
// /rbac-preflight). Each answered every session in full before.
func TestC2_TheSettledRoutesAnswerTheirClass(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	now := time.Now().UTC().Format(time.RFC3339)

	// Questions 71 and /rbac-preflight: a global administrator's.
	for _, path := range []string{"/api/v1/settings/log-storage", "/api/v1/settings/observability", "/api/v1/rbac-preflight"} {
		if code := gateReq(t, h, http.MethodGet, path, gFinAdmin, "").Code; code != http.StatusForbidden {
			t.Errorf("FIN's administrator reading %s = %d, want 403", path, code)
		}
		if code := gateReq(t, h, http.MethodGet, path, gRoot, "").Code; code != http.StatusOK {
			t.Errorf("a global administrator reading %s = %d, want 200", path, code)
		}
	}

	// Question 69: every agency's name, and an unread agency's nothing more.
	exec(`UPDATE agencies SET description = 'c2-fin-desc', created_by = 'c2-fin-maker@example' WHERE id = 'ag:FIN'`)
	body := gateReq(t, h, http.MethodGet, "/api/v1/agencies", gTaxAdmin, "").Body.String()
	if !strings.Contains(body, `"FIN"`) || strings.Contains(body, "c2-fin-desc") || strings.Contains(body, "c2-fin-maker") {
		t.Errorf("TAX's administrator listing agencies: FIN named %v, its description %v, its maker %v; want named only",
			strings.Contains(body, `"FIN"`), strings.Contains(body, "c2-fin-desc"), strings.Contains(body, "c2-fin-maker"))
	}
	if body := gateReq(t, h, http.MethodGet, "/api/v1/agencies", gFinAdmin, "").Body.String(); !strings.Contains(body, "c2-fin-desc") {
		t.Errorf("FIN's administrator does not see FIN's description: %s", body)
	}

	// Question 70: the relay's address, account and recipients are masked.
	exec(`INSERT OR REPLACE INTO notification_config (id, smtp_host, smtp_port, smtp_from, smtp_username, smtp_encryption, smtp_recipients, apprise_enabled, apprise_targets, last_modified_at)
	      VALUES (1, 'c2-relay.example', 587, 'cronomicon@example.com', 'c2-relay-user', 'starttls', '["c2-ops@example.com"]', 0, '[]', ?)`, now)
	for who, want := range map[string]bool{gFinAdmin: false, gRoot: true} {
		b := gateReq(t, h, http.MethodGet, "/api/v1/settings/notifications", who, "").Body.String()
		for _, m := range []string{"c2-relay.example", "c2-relay-user", "c2-ops@example.com"} {
			if strings.Contains(b, m) != want {
				t.Errorf("%s reading the notification settings: %q shown %v, want %v", who, m, !want, want)
			}
		}
	}

	// Question 68: who signed in, per agency, to an administrator of it.
	exec(`INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at) VALUES
	        ('c2-fin@example',  'Fin',  '["fin-admins"]',               ?, ?),
	        ('c2-tax@example',  'Tax',  '["tax-admins"]',               ?, ?),
	        ('c2-both@example', 'Both', '["fin-admins","tax-admins"]',  ?, ?),
	        ('c2-root@example', 'Root', '["root-admins"]',              ?, ?)`, now, now, now, now, now, now, now, now)
	type login struct {
		Email  string   `json:"email"`
		Groups []string `json:"adGroups"`
		Grants []struct {
			AgencyName string `json:"agencyName"`
			AllScopes  bool   `json:"allScopes"`
		} `json:"grants"`
	}
	logins := func(who string) (int, map[string]login) {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/recent-logins", who, "")
		var env struct {
			Items []login `json:"items"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &env)
		out := map[string]login{}
		for _, l := range env.Items {
			out[l.Email] = l
		}
		return rec.Code, out
	}
	_, all := logins(gRoot)
	for _, e := range []string{"c2-fin@example", "c2-tax@example", "c2-both@example", "c2-root@example"} {
		if _, ok := all[e]; !ok {
			t.Errorf("a global administrator does not see %s", e)
		}
	}
	code, fin := logins(gFinAdmin)
	if code != http.StatusOK {
		t.Fatalf("FIN's administrator reading recent logins = %d", code)
	}
	if _, ok := fin["c2-fin@example"]; !ok {
		t.Errorf("FIN's administrator does not see FIN's person")
	}
	for _, e := range []string{"c2-tax@example", "c2-root@example"} {
		if _, ok := fin[e]; ok {
			t.Errorf("FIN's administrator sees %s, who holds no grant on FIN", e)
		}
	}
	if b, ok := fin["c2-both@example"]; !ok || len(b.Groups) != 0 || len(b.Grants) != 1 || b.Grants[0].AgencyName != "FIN" {
		t.Errorf("FIN's administrator sees the person of both agencies as %+v; want FIN's grant only and no groups", b)
	}
	if code, _ := logins(gFinViewer); code != http.StatusForbidden {
		t.Errorf("a viewer reading recent logins = %d, want 403 (manageRoles)", code)
	}
}
