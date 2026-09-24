package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRecentLoginsResponseShape covers bug LB4: the RecentLogins handler must
// serialize fields under the names the OpenAPI RecentLogin schema / SPA expect
// (name, adGroups, lastSeenAt) and surface the single highest-privilege role
// resolved from the row's AD groups (admin > operator > viewer).
func TestRecentLoginsResponseShape(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	// Two groups that resolve to two roles; the handler must pick the highest.
	seedGroupRole(t, s, "ops-team", "operator")
	seedGroupRole(t, s, "admins", "admin")

	now := time.Now().UTC().Format(time.RFC3339)
	groupsJSON, _ := json.Marshal([]string{"ops-team", "admins"})
	if _, err := s.db.Exec(`
		INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		VALUES (?, ?, ?, ?, ?)`,
		"jdoe@example.com", "Jane Doe", string(groupsJSON), now, now); err != nil {
		t.Fatalf("seed recent_logins: %v", err)
	}

	rec := httptest.NewRecorder()
	s.RecentLogins(rec, httptest.NewRequest(http.MethodGet, "/api/v1/access/recent-logins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	// Assert the raw JSON uses the spec field names — a struct decode would mask
	// wrong tags, so check the keys are literally present in the payload.
	raw := rec.Body.String()
	for _, key := range []string{`"name"`, `"adGroups"`, `"lastSeenAt"`, `"firstSeenAt"`, `"grants"`} {
		if !contains(raw, key) {
			t.Errorf("response missing JSON key %s; body: %s", key, raw)
		}
	}
	// And assert the old (buggy) tags are gone. `resolvedRole` joins them in
	// v0.57.8: RB-21 replaced the single lossy name with the grant list, and a
	// client still reading that key would silently show every user as roleless.
	for _, key := range []string{`"displayName"`, `"lastLoginAt"`, `"groups"`, `"resolvedRole"`} {
		if contains(raw, key) {
			t.Errorf("response still has legacy JSON key %s (LB4 regression); body: %s", key, raw)
		}
	}

	var resp struct {
		Items []struct {
			Email       string           `json:"email"`
			Name        string           `json:"name"`
			AdGroups    []string         `json:"adGroups"`
			Grants      []map[string]any `json:"grants"`
			FirstSeenAt string           `json:"firstSeenAt"`
			LastSeenAt  string           `json:"lastSeenAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	it := resp.Items[0]
	if it.Name != "Jane Doe" {
		t.Errorf("name = %q, want Jane Doe", it.Name)
	}
	if len(it.AdGroups) != 2 {
		t.Errorf("adGroups = %v, want 2 entries", it.AdGroups)
	}
	if it.LastSeenAt == "" {
		t.Error("lastSeenAt is blank (LB4)")
	}
	if it.FirstSeenAt == "" {
		t.Error("firstSeenAt is blank")
	}
	// Both grants survive as separate entries rather than collapsing to the
	// highest role — the RB-21 property, asserted here on the wire shape too.
	if len(it.Grants) != 2 {
		t.Errorf("grants = %+v, want both the admin and operator grants", it.Grants)
	}
}

// TestRecentLoginsNoRoles verifies resolvedRole is empty when a row's groups map
// to no role (honest default), without erroring.
func TestRecentLoginsNoRoles(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")

	now := time.Now().UTC().Format(time.RFC3339)
	groupsJSON, _ := json.Marshal([]string{"unmapped-group"})
	if _, err := s.db.Exec(`
		INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		VALUES (?, ?, ?, ?, ?)`,
		"nobody@example.com", "No Body", string(groupsJSON), now, now); err != nil {
		t.Fatalf("seed recent_logins: %v", err)
	}

	rec := httptest.NewRecorder()
	s.RecentLogins(rec, httptest.NewRequest(http.MethodGet, "/api/v1/access/recent-logins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items []struct {
			Grants     []map[string]any `json:"grants"`
			LastSeenAt string           `json:"lastSeenAt"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	// An ungranted user's grants must be [] rather than null: the SPA maps over it,
	// and "no access" is a statement it renders, not a case it special-cases.
	if resp.Items[0].Grants == nil {
		t.Error("grants serialized as null; it must be [] so the SPA can map over it")
	}
	if len(resp.Items[0].Grants) != 0 {
		t.Errorf("grants = %+v, want empty for groups no grant names", resp.Items[0].Grants)
	}
	if resp.Items[0].LastSeenAt == "" {
		t.Error("lastSeenAt is blank")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
