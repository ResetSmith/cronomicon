package api_test

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// GET /git-repo-names (2.4.0 R8, from its review): the repositories whose
// history the caller may read, by the rule `?repo=` is read by (readableRepo,
// GR-26). The screens that work on one repository at a time choose from it;
// GET /git-repos answers configureApp only, so an approver or a viewer had
// nothing to choose. Ids and agency names only.
func TestR8_RepoNamesAreWhatTheCallerMayRead(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('repo-fin', 'ag:FIN', 't', 't', 't', 't'), ('repo-tax', 'ag:TAX', 't', 't', 't', 't')`)

	for _, c := range []struct {
		who  string
		want []string
	}{
		{gFinViewer, []string{"repo-fin"}}, // Global's is everywhere-readers' (C2, question 77)
		{gFinApprover, []string{"repo-fin"}},
		{gAllViewer, []string{"global", "repo-fin", "repo-tax"}},
	} {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/git-repo-names", c.who, "")
		var body struct {
			Items []map[string]any `json:"items"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
			t.Fatalf("%s: GET /git-repo-names = %d (%s)", c.who, rec.Code, rec.Body)
		}
		var got []string
		for _, it := range body.Items {
			got = append(got, it["id"].(string))
			for k := range it {
				// canSync and canPublish (2.4.2, question 55) are about the
				// caller, not the connection.
				if k != "id" && k != "agencyId" && k != "agencyName" && k != "canSync" && k != "canPublish" {
					t.Errorf("%s: a repository name carries %q; it says which and whose, nothing of the connection", c.who, k)
				}
			}
			// Each one listed is one the history route lets this caller read
			// (the gate's refusals are 403 and 404; the fixture wires no Git
			// handler, so a read that passes the gate answers 503).
			if code := gateReq(t, h, http.MethodGet, "/api/v1/git/history?repo="+it["id"].(string), c.who, "").Code; code == http.StatusForbidden || code == http.StatusNotFound {
				t.Errorf("%s: listed %v, but its history answered %d", c.who, it["id"], code)
			}
		}
		sort.Strings(got)
		if len(got) != len(c.want) {
			t.Fatalf("%s: listed %v, want %v", c.who, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: listed %v, want %v", c.who, got, c.want)
			}
		}
	}
	// And GET /git-repos stays configureApp's.
	if code := gateReq(t, h, http.MethodGet, "/api/v1/git-repos", gFinViewer, "").Code; code != http.StatusForbidden {
		t.Errorf("a viewer's GET /git-repos = %d, want 403", code)
	}
}
