package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Honest View's access column had NO test, which is how it kept deriving from
// ad_group_mappings for the three releases after v0.56.5 made access_grants
// authoritative. v0.57.7 pointed it at grants; v0.57.8 (RB-21) replaced the single
// `resolvedRole` name with the grant list, because under multi-grant one name was
// not merely terse but FALSE — and this is the screen an auditor uses to answer
// "who holds what".

// seedLogin writes a recent_logins row directly — the Honest View reads what login
// recorded, and these tests are about resolution, not about recording.
func seedLogin(t *testing.T, s *Service, email string, groups []string) {
	t.Helper()
	g, _ := json.Marshal(groups)
	exec(t, s.db, `INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		VALUES (?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z')`,
		email, email, string(g))
}

type loginGrant struct {
	Role       string `json:"role"`
	AgencyName string `json:"agencyName"`
	AllScopes  bool   `json:"allScopes"`
}

func recentLogins(t *testing.T, s *Service) map[string][]loginGrant {
	t.Helper()
	rec := httptest.NewRecorder()
	s.RecentLogins(rec, httptest.NewRequest(http.MethodGet, "/api/v1/recent-logins", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("RecentLogins = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			Email  string       `json:"email"`
			Grants []loginGrant `json:"grants"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out := map[string][]loginGrant{}
	for _, it := range body.Items {
		out[it.Email] = it.Grants
	}
	return out
}

// TestHonestViewNamesRoleAndWhere is the RB-21 fence, and the reason the column
// changed shape: a user who is operator on ONE department must not be presented in
// a way that reads as operator everywhere.
//
// Alice is operator on Tax and viewer on Finance. The old column showed "operator"
// — the highest of her roles — which an auditor reads as unrestricted operator.
// Both halves of both grants must survive to the wire.
func TestHonestViewNamesRoleAndWhere(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	seedTwoAgencies(t, s.db)
	exec(t, s.db, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at) VALUES
		('g-ops',  'sg-ops',  'operator', 'a-tax', 0, 'test', '2026-01-01T00:00:00Z'),
		('g-view', 'sg-view', 'viewer',   'a-fin', 0, 'test', '2026-01-01T00:00:00Z')`)
	seedLogin(t, s, "alice@ex.com", []string{"sg-ops", "sg-view"})

	got := recentLogins(t, s)["alice@ex.com"]
	if len(got) != 2 {
		t.Fatalf("grants = %+v, want both (operator@Tax, viewer@Finance) — collapsing them "+
			"to one name is the defect RB-21 exists to fix", got)
	}
	// ResolveGrants sorts by role then agency, so the order is deterministic.
	if got[0].Role != "operator" || got[0].AgencyName != "Tax" {
		t.Errorf("grants[0] = %+v, want operator on Tax", got[0])
	}
	if got[1].Role != "viewer" || got[1].AgencyName != "Finance" {
		t.Errorf("grants[1] = %+v, want viewer on Finance", got[1])
	}
	// Neither is unrestricted, and saying so is the whole point: an operator grant
	// bounded to one department must never present as an unbounded one.
	for _, g := range got {
		if g.AllScopes {
			t.Errorf("%+v claims allScopes; it was authored against an agency", g)
		}
	}
}

// TestHonestViewMarksUnrestricted separates the two shapes a grant can take. An
// unrestricted grant carries no agency, and the flag is what lets the UI style it
// apart — these are the rows worth auditing first.
func TestHonestViewMarksUnrestricted(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	exec(t, s.db, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at)
		VALUES ('g1', 'SG-Admins', 'admin', NULL, 1, 'test', '2026-01-01T00:00:00Z')`)
	seedLogin(t, s, "root@ex.com", []string{"SG-Admins"})

	got := recentLogins(t, s)["root@ex.com"]
	if len(got) != 1 {
		t.Fatalf("grants = %+v, want one", got)
	}
	if !got[0].AllScopes {
		t.Error("an all_scopes grant must set allScopes — a UI cannot otherwise tell it " +
			"apart from a departmental one, which is the distinction that matters most")
	}
	if got[0].AgencyName != "" {
		t.Errorf("agencyName = %q, want empty for an unrestricted grant", got[0].AgencyName)
	}
}

// TestHonestViewEmptyWithoutGrants covers the honest default: no grant, no access.
// Fail-closed means this user can do nothing, and the empty list is the true
// statement the UI renders as "No access".
func TestHonestViewEmptyWithoutGrants(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	seedLogin(t, s, "nobody@ex.com", []string{"SG-Unknown"})
	seedLogin(t, s, "groupless@ex.com", nil)

	got := recentLogins(t, s)
	for _, email := range []string{"nobody@ex.com", "groupless@ex.com"} {
		if len(got[email]) != 0 {
			t.Errorf("%s resolved %+v, want no grants", email, got[email])
		}
	}
}

// TestHonestViewDropsGrantsWithTheirAgency pins what actually happens when a
// department is deleted, which is better than the fallback I first wrote a test
// for: `access_grants.agency_id` is `REFERENCES agencies(id) ON DELETE CASCADE`,
// so a dangling grant cannot exist. Deleting the agency deletes the grants that
// named it, and the Honest View simply stops showing them.
//
// grantViews still falls back to the raw id if a name ever fails to resolve. That
// path is unreachable through the schema and is kept as defence rather than
// behaviour — a blank "where" would be indistinguishable from unrestricted, which
// is the most dangerous thing it could be mistaken for.
func TestHonestViewDropsGrantsWithTheirAgency(t *testing.T) {
	s := headerService(t, "10.0.0.0/8", "")
	seedTwoAgencies(t, s.db)
	exec(t, s.db, `INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at) VALUES
		('g-tax', 'sg-both', 'operator', 'a-tax', 0, 'test', '2026-01-01T00:00:00Z'),
		('g-fin', 'sg-both', 'viewer',   'a-fin', 0, 'test', '2026-01-01T00:00:00Z')`)
	seedLogin(t, s, "both@ex.com", []string{"sg-both"})

	if got := recentLogins(t, s)["both@ex.com"]; len(got) != 2 {
		t.Fatalf("fixture: grants = %+v, want 2 before the delete", got)
	}

	exec(t, s.db, `DELETE FROM agencies WHERE id = 'a-fin'`)

	got := recentLogins(t, s)["both@ex.com"]
	if len(got) != 1 {
		t.Fatalf("grants = %+v, want only the Tax grant to survive", got)
	}
	if got[0].AgencyName != "Tax" {
		t.Errorf("surviving grant = %+v, want operator on Tax", got[0])
	}
	// The remaining grant must not have silently widened into an unrestricted one.
	if got[0].AllScopes {
		t.Error("deleting an unrelated agency turned a departmental grant unrestricted")
	}
}
