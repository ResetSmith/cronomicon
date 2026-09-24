package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// RF-17 — the workflow half of RB-23 (the RBAC-fixes plan).
//
// The Jobs catalog got a derived agency column in v0.56.3; its workflow sibling,
// named in the same plan item, was missed. That left the two halves of one
// catalog answering "whose is this?" differently — and once access is
// departmental, "show me my department's work" is the first thing anyone asks.
//
// A workflow has NO scope of its own, so unlike a job its agencies cannot be read
// off one column: they are the union over its constituent jobs' scopes, which is
// the same walk workflowScopesPermit authorizes over. That correspondence is the
// property worth testing — a display that disagreed with the authorization walk
// would be worse than no display.
func TestWorkflowListDerivesAgenciesFromItsJobs(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-tax','tax','amadeus','t')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-fin','finance','amadeus','t')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-tax','ag-tax')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-fin','ag-fin')`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled) VALUES ('j-tax','amadeus','bash','tax',1)`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled) VALUES ('j-fin','amadeus','bash','finance',1)`)
	// A job whose scope belongs to NO agency — the orphan case the pre-flight
	// reports and the catalog renders as an em-dash.
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-orph','orphan','amadeus','t')`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled) VALUES ('j-orph','amadeus','bash','orphan',1)`)

	exec(`INSERT INTO workflows (uid,name,source,steps,enabled,synced_at)
	      VALUES ('uid-wf-tax','wf-tax','amadeus','[{"type":"job","name":"j-tax"}]',1,'t')`)
	// Spanning two departments: BOTH must appear. Picking one would be a lie, and
	// picking none would hide that this workflow crosses a boundary — which is
	// exactly what an operator needs to see.
	exec(`INSERT INTO workflows (uid,name,source,steps,enabled,synced_at)
	      VALUES ('uid-wf-both','wf-both','amadeus','[{"type":"job","name":"j-tax"},{"type":"job","name":"j-fin"}]',1,'t')`)
	exec(`INSERT INTO workflows (uid,name,source,steps,enabled,synced_at)
	      VALUES ('uid-wf-orph','wf-orph','amadeus','[{"type":"job","name":"j-orph"}]',1,'t')`)
	// Nested steps: the union must walk the whole graph, not just top-level jobs.
	exec(`INSERT INTO workflows (uid,name,source,steps,enabled,synced_at)
	      VALUES ('uid-wf-nested','wf-nested','amadeus','[{"type":"parallel","jobs":[{"type":"job","name":"j-fin"}]}]',1,'t')`)

	rec := reqAs(t, h, http.MethodGet, "/api/v1/workflows", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /workflows = %d (%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			Name     string   `json:"name"`
			Agencies []string `json:"agencies"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	got := map[string][]string{}
	for _, it := range page.Items {
		got[it.Name] = it.Agencies
	}
	if len(got) == 0 {
		t.Fatalf("no workflows returned: %s", rec.Body.String())
	}

	eq := func(a []string, want ...string) bool {
		if len(a) != len(want) {
			return false
		}
		for i := range want {
			if a[i] != want[i] {
				return false
			}
		}
		return true
	}
	if !eq(got["wf-tax"], "Tax") {
		t.Errorf("wf-tax agencies = %v, want [Tax]", got["wf-tax"])
	}
	// Sorted, so the rendering is stable across requests rather than following map
	// iteration order.
	if !eq(got["wf-both"], "Finance", "Tax") {
		t.Errorf("wf-both agencies = %v, want [Finance Tax] — a workflow spanning two "+
			"departments must list both, since that is what its authorization walk covers", got["wf-both"])
	}
	if len(got["wf-orph"]) != 0 {
		t.Errorf("wf-orph agencies = %v, want empty — its job's scope belongs to no agency", got["wf-orph"])
	}
	if !eq(got["wf-nested"], "Finance") {
		t.Errorf("wf-nested agencies = %v, want [Finance] — the union must walk NESTED "+
			"steps, not just top-level jobs", got["wf-nested"])
	}
}
