package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// 2.5.0 C5 — the group allowlist (GA-1 to GA-3; questions 82 to 85).
//
// A global administrator lists the directory groups each agency's grants may
// name, and an agency's delegate (manageRoles there, AF-3) grants only to
// those. The fixture is AF-3's: fin-govs administer access in FIN, tax-govs in
// TAX, root-govs everywhere.

func listGroups(t *testing.T, h http.Handler, agency string, groups string) {
	t.Helper()
	rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/groups", "root-govs", `{"groups":`+groups+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a global administrator listing %s for %s = %d, want 200. Body: %s", groups, agency, rec.Code, rec.Body.String())
	}
}

// TestADelegateGrantsOnlyToListedGroups — GA-1: a listed group yes, any other
// no; a global administrator is not bound.
func TestADelegateGrantsOnlyToListedGroups(t *testing.T) {
	h, _ := delegatedGrantServer(t)
	listGroups(t, h, "ag:FIN", `["fin-operators"]`)

	rec := composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
		`{"adGroup":"fin-operators","role":"operator","agencyId":"ag:FIN"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a delegate granting to a listed group = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
	for _, group := range []string{"Domain Users", "FIN-OPERATORS", " fin-operators-2"} {
		rec = composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "fin-govs",
			`{"adGroup":"`+group+`","role":"operator","agencyId":"ag:FIN"}`)
		if rec.Code != http.StatusForbidden || errCode(rec.Body.Bytes()) != "group_not_listed" {
			t.Errorf("a delegate granting to unlisted %q = %d %s, want 403 group_not_listed", group, rec.Code, rec.Body.String())
		}
	}
	// A group listed for FIN is not TAX's: each agency has its own list.
	rec = composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "tax-govs",
		`{"adGroup":"fin-operators","role":"operator","agencyId":"ag:TAX"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a TAX delegate granting to a group only FIN lists = %d, want 403", rec.Code)
	}
	// Question 84: a global administrator may grant to an unlisted group.
	rec = composeReq(t, h, http.MethodPost, "/api/v1/access-grants", "root-govs",
		`{"adGroup":"Domain Users","role":"viewer","agencyId":"ag:FIN"}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("a global administrator granting to an unlisted group = %d, want 201. Body: %s", rec.Code, rec.Body.String())
	}
}

// TestAnUnlistedGrantKeepsWorkingIsNoticedAndIsDeletable — GA-2: no grant is
// revoked by the list; one naming an unlisted group is noticed under its
// agency, its delegate may delete it but not change it, and the notice
// clears when either the group is listed or the grant is gone.
func TestAnUnlistedGrantKeepsWorkingIsNoticedAndIsDeletable(t *testing.T) {
	h, pool := delegatedGrantServer(t)
	ctx := context.Background()
	if _, err := pool.Exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
	                        VALUES ('g:legacy', 'fin-legacy-team', 'operator', 'ag:FIN', 0, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	auth.GrantsChanged()

	// It still resolves: the group's members hold the role in FIN.
	rec := composeReq(t, h, http.MethodGet, "/api/v1/me/access", "fin-legacy-team", "")
	var access struct {
		Grants []struct {
			AgencyID string `json:"agencyId"`
			Role     string `json:"role"`
		} `json:"grants"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &access)
	if len(access.Grants) != 1 || access.Grants[0].AgencyID != "ag:FIN" || access.Grants[0].Role != "operator" {
		t.Fatalf("the unlisted grant's members hold %+v, want operator in FIN", access.Grants)
	}

	open := func() map[string]string {
		t.Helper()
		if err := notices.RunChecks(ctx, pool); err != nil {
			t.Fatalf("checks: %v", err)
		}
		rows, err := pool.Query(`SELECT subject, agency_id FROM notices WHERE kind = ? AND resolved_at IS NULL`, notices.KindGrantGroupUnlisted)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var s, a string
			_ = rows.Scan(&s, &a)
			got[s] = a
		}
		return got
	}
	// Every agency grant names an unlisted group (the lists start empty,
	// question 83); the all-agencies grants are not agencies' and are not noticed.
	got := open()
	if got["g:legacy"] != "ag:FIN" || got["g:tax"] != "ag:TAX" || len(got) != 4 {
		t.Fatalf("open grant_group_unlisted = %v, want the four agency grants, each under its agency", got)
	}
	if _, ok := got["g:root"]; ok {
		t.Errorf("an all-agencies grant was noticed")
	}

	// Its delegate may not change it, even into a listed group.
	listGroups(t, h, "ag:FIN", `["fin-operators"]`)
	rec = composeReq(t, h, http.MethodPut, "/api/v1/access-grants/g:legacy", "fin-govs",
		`{"adGroup":"fin-operators","role":"operator","agencyId":"ag:FIN"}`)
	if rec.Code != http.StatusForbidden || errCode(rec.Body.Bytes()) != "group_not_listed" {
		t.Errorf("a delegate editing an unlisted grant = %d %s, want 403 group_not_listed", rec.Code, rec.Body.String())
	}
	// A global administrator may.
	rec = composeReq(t, h, http.MethodPut, "/api/v1/access-grants/g:legacy", "root-govs",
		`{"adGroup":"fin-legacy-team","role":"viewer","agencyId":"ag:FIN"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("a global administrator editing an unlisted grant = %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	// Listing the group clears its notice; deleting a grant clears its.
	listGroups(t, h, "ag:FIN", `["fin-operators","fin-govs","fin-legacy-team"]`)
	if got := open(); got["g:legacy"] != "" || got["g:fin"] != "" {
		t.Errorf("after FIN listed the groups, open = %v; want no FIN grant", got)
	}
	rec = composeReq(t, h, http.MethodDelete, "/api/v1/access-grants/g:target-tax", "tax-govs", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("a delegate deleting an unlisted grant of its agency = %d, want 204. Body: %s", rec.Code, rec.Body.String())
	}
	if got := open(); got["g:target-tax"] != "" {
		t.Errorf("the deleted grant is still noticed: %v", got)
	}
}

// TestAnAgencysGroupList — who reads and writes the list (GA-1, question 82),
// and what a write stores (question 85).
func TestAnAgencysGroupList(t *testing.T) {
	h, _ := delegatedGrantServer(t)

	rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/groups", "root-govs",
		`{"groups":[" SG-Fin ","SG-Fin","sg-fin",""]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set = %d. Body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Groups []string `json:"groups"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Groups) != 2 || got.Groups[0] != "SG-Fin" || got.Groups[1] != "sg-fin" {
		t.Errorf("stored %v, want [SG-Fin sg-fin]: trimmed, a repeat once, case kept", got.Groups)
	}

	t.Run("the agency's delegate reads its list", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/groups", "fin-govs", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("= %d, want 200", rec.Code)
		}
	})
	t.Run("another agency's delegate does not", func(t *testing.T) {
		if rec := composeReq(t, h, http.MethodGet, "/api/v1/agencies/ag:FIN/groups", "tax-govs", ""); rec.Code != http.StatusForbidden {
			t.Errorf("= %d, want 403", rec.Code)
		}
	})
	t.Run("a delegate does not write even its own", func(t *testing.T) {
		if rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/groups", "fin-govs", `{"groups":["any"]}`); rec.Code != http.StatusForbidden {
			t.Errorf("= %d, want 403", rec.Code)
		}
	})
	t.Run("Global has no list", func(t *testing.T) {
		rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/global/groups", "root-govs", `{"groups":["x"]}`)
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "builtin_agency" {
			t.Errorf("= %d %s, want 422 builtin_agency", rec.Code, rec.Body.String())
		}
	})
	t.Run("a body without the list is refused, not read as none", func(t *testing.T) {
		if rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/groups", "root-govs", `{}`); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("= %d, want 422", rec.Code)
		}
	})
	t.Run("an unknown agency", func(t *testing.T) {
		if rec := composeReq(t, h, http.MethodPut, "/api/v1/agencies/ag:NOPE/groups", "root-govs", `{"groups":[]}`); rec.Code != http.StatusNotFound {
			t.Errorf("= %d, want 404", rec.Code)
		}
	})
}
