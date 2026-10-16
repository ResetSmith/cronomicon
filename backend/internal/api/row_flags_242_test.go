package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Question 55 (2.4.2): the schedule and calendar lists and the repository
// names carry what the caller may do with each row, worked out as the routes
// work it out, so that the screens stop deriving it from roles.
func TestRV_SchedulesCalendarsAndRepositoriesSayWhatTheCallerMayDo(t *testing.T) {
	h, pool := gateServer(t)
	agenciesGetTheirOwnRepository(t, pool)
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/calendars", gFinAdmin, `{"name":"fin-holidays","ownerAgency":"ag:FIN","days":[{"day":"2026-12-25"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("FIN's calendar: %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/schedule-defs", gFinAdmin, `{"name":"fin-nightly","ownerAgency":"ag:FIN","cron":"0 2 * * *"}`); rec.Code/100 != 2 {
		t.Fatalf("FIN's schedule: %d %s", rec.Code, rec.Body)
	}
	type flags struct {
		Name      string `json:"name"`
		CanEdit   bool   `json:"canEdit"`
		CanDelete bool   `json:"canDelete"`
	}
	list := func(who, path string) map[string]flags {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, who, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s as %s: %d %s", path, who, rec.Code, rec.Body)
		}
		var body struct {
			Items []flags `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]flags{}
		for _, f := range body.Items {
			out[f.Name] = f
		}
		return out
	}
	for _, path := range []string{"/api/v1/calendars", "/api/v1/schedule-defs"} {
		name := "fin-holidays"
		if path == "/api/v1/schedule-defs" {
			name = "fin-nightly"
		}
		if f := list(gFinAdmin, path)[name]; !f.CanEdit || !f.CanDelete {
			t.Errorf("%s: FIN's own row says %+v to FIN, want edit and delete", path, f)
		}
		if f, ok := list(gTaxAdmin, path)[name]; ok && (f.CanEdit || f.CanDelete) {
			t.Errorf("%s: FIN's row says %+v to TAX, want neither", path, f)
		}
	}

	rec := gateReq(t, h, http.MethodGet, "/api/v1/git-repo-names", gFinAdmin, "")
	var repos struct {
		Items []struct {
			ID         string `json:"id"`
			AgencyID   string `json:"agencyId"`
			CanSync    bool   `json:"canSync"`
			CanPublish bool   `json:"canPublish"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &repos); err != nil {
		t.Fatal(err)
	}
	for _, r := range repos.Items {
		own := r.AgencyID == "ag:FIN"
		if r.CanSync != own || r.CanPublish != own {
			t.Errorf("repository %s (%s) says sync %v, publish %v to FIN; want %v for both", r.ID, r.AgencyID, r.CanSync, r.CanPublish, own)
		}
	}
}
