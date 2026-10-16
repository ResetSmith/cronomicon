package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The agency switcher's choices and defaults (2.4.4 S4): GET /me/context and
// PUT /me/preferences, each step of the rule.
type myContext struct {
	Choices []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"choices"`
	StoredDefault    *string `json:"storedDefault"`
	EffectiveDefault string  `json:"effectiveDefault"`
	WorkingAgency    string  `json:"workingAgency"`
	CanOverview      bool    `json:"canOverview"`
}

func (c myContext) ids() string {
	var out []string
	for _, ch := range c.Choices {
		out = append(out, ch.ID)
	}
	return strings.Join(out, ",")
}

func readContext(t *testing.T, h http.Handler, groups string) myContext {
	t.Helper()
	rec := gateReq(t, h, http.MethodGet, "/api/v1/me/context", groups, "")
	var c myContext
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &c) != nil {
		t.Fatalf("GET /me/context as %s = %d %s", groups, rec.Code, rec.Body)
	}
	return c
}

func TestSwitcherWhereEachKindOfPersonStarts(t *testing.T) {
	h, _ := gateServer(t)
	for _, c := range []struct {
		who, groups, choices, effective, working string
		overview                                 bool
	}{
		// A global administrator: Global (SW-6), and the overview to choose.
		{"root", gRoot, "overview,global,ag:FIN,ag:TAX", "global", "global", true},
		// A viewer of every agency, the operations team: the overview (SW-13),
		// with Global for the lists.
		{"all-viewer", gAllViewer, "overview,global,ag:FIN,ag:TAX", "overview", "global", true},
		// One agency's people: their agency, and Global to read (SW-5).
		{"fin-admin", gFinAdmin, "global,ag:FIN", "ag:FIN", "ag:FIN", false},
		{"fin-viewer", gFinViewer, "global,ag:FIN", "ag:FIN", "ag:FIN", false},
		// Two agencies' people: the first by name (SW-2).
		{"fin+tax admin", gFinAdmin + "," + gTaxAdmin, "global,ag:FIN,ag:TAX", "ag:FIN", "ag:FIN", false},
		// Nobody's: Global only (SW-7).
		{"no grant", "nobody-group", "global", "global", "global", false},
	} {
		got := readContext(t, h, c.groups)
		if got.ids() != c.choices || got.EffectiveDefault != c.effective || got.WorkingAgency != c.working ||
			got.CanOverview != c.overview || got.StoredDefault != nil {
			t.Errorf("%s: choices %s, starts %s, works in %s, overview %v, stored %v; want %s, %s, %s, %v, none",
				c.who, got.ids(), got.EffectiveDefault, got.WorkingAgency, got.CanOverview, got.StoredDefault,
				c.choices, c.effective, c.working, c.overview)
		}
	}
}

// The first agency by name ignores case: "alpha-tax" comes before "FIN".
func TestSwitcherTheFirstAgencyByNameIgnoresCase(t *testing.T) {
	h, pool := gateServer(t)
	mustExec(t, pool)(`UPDATE agencies SET name = 'alpha-tax' WHERE id = 'ag:TAX'`)
	got := readContext(t, h, gFinAdmin+","+gTaxAdmin)
	if got.EffectiveDefault != "ag:TAX" || got.ids() != "global,ag:TAX,ag:FIN" {
		t.Errorf("with TAX named alpha-tax: starts %s, choices %s; want ag:TAX, global,ag:TAX,ag:FIN", got.EffectiveDefault, got.ids())
	}
}

func TestSwitcherAPersonChoosesTheirOwnDefault(t *testing.T) {
	h, pool := gateServer(t)
	finTax := gFinAdmin + "," + gTaxAdmin
	put := func(groups, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/me/preferences", groups, body)
		return rec.Code, rec.Body.String()
	}
	// Their own choice is where they start.
	if c, b := put(finTax, `{"defaultAgencyId":"ag:TAX"}`); c != http.StatusOK || !strings.Contains(b, `"effectiveDefault":"ag:TAX"`) {
		t.Fatalf("choosing TAX = %d %s", c, b)
	}
	if got := readContext(t, h, finTax); got.EffectiveDefault != "ag:TAX" || got.StoredDefault == nil || *got.StoredDefault != "ag:TAX" {
		t.Errorf("after choosing TAX: starts %s, stored %v", got.EffectiveDefault, got.StoredDefault)
	}
	// Cleared, the rule again.
	if c, b := put(finTax, `{"defaultAgencyId":null}`); c != http.StatusOK || !strings.Contains(b, `"effectiveDefault":"ag:FIN"`) {
		t.Fatalf("clearing = %d %s", c, b)
	}
	// Not theirs, not there, or the overview for one who may not see it: one
	// answer, 422.
	for _, body := range []string{`{"defaultAgencyId":"ag:TAX"}`, `{"defaultAgencyId":"ag:NOPE"}`, `{"defaultAgencyId":"overview"}`} {
		if c, b := put(gFinAdmin, body); c != http.StatusUnprocessableEntity || !strings.Contains(b, `"invalid_default_agency"`) ||
			strings.Contains(b, "NOPE") || strings.Contains(b, "TAX") {
			t.Errorf("FIN's admin choosing %s = %d %s, want 422 invalid_default_agency, naming nothing", body, c, b)
		}
	}
	// A global administrator may start on the overview, with Global for the lists.
	if c, b := put(gRoot, `{"defaultAgencyId":"overview"}`); c != http.StatusOK ||
		!strings.Contains(b, `"effectiveDefault":"overview"`) || !strings.Contains(b, `"workingAgency":"global"`) {
		t.Errorf("root choosing the overview = %d %s", c, b)
	}
	// A default the person has lost access to is ignored, not removed.
	mustExec(t, pool)(`INSERT INTO user_preferences (email, default_agency, updated_at) VALUES ('fin-admins@example.com', 'ag:TAX', 't')`)
	if got := readContext(t, h, gFinAdmin); got.EffectiveDefault != "ag:FIN" || got.StoredDefault == nil || *got.StoredDefault != "ag:TAX" {
		t.Errorf("a stored default no longer theirs: starts %s, stored %v; want ag:FIN, ag:TAX kept", got.EffectiveDefault, got.StoredDefault)
	}
}
