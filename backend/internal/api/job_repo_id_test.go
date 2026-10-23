package api_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// A job row says which repository a Git job is of (2.4.0, from R8's review):
// Publish edits a job in the repository it came from, and the publish route
// writes wherever it is pointed (it does not read `new`), so an edit pointed
// at another repository made a second job of the name there. Two agencies'
// Git jobs of one name, each in its own repository: the list and the detail
// name each one's; a job built in the app names none.
func TestJobRowsNameTheirRepository(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO git_repos (id, agency_id, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('repo-fin', 'ag:FIN', 't', 't', 't', 't')`)
	exec(`INSERT INTO jobs (uid, name, source, repo_id, run_type, enabled) VALUES
	      ('uid-g', 'nightly-pub', 'git', 'global', 'bash', 1),
	      ('uid-f', 'nightly-pub', 'git', 'repo-fin', 'bash', 1),
	      ('uid-c', 'built-here', 'cronomicon', NULL, 'bash', 1)`)

	rec := gateReq(t, h, http.MethodGet, "/api/v1/jobs?pageSize=200", gRoot, "")
	var list struct {
		Items []struct {
			ID     int64  `json:"id"`
			UID    string `json:"uid"`
			RepoID string `json:"repoId"`
		} `json:"items"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("GET /jobs = %d (%s)", rec.Code, rec.Body)
	}
	want := map[string]string{"uid-g": "global", "uid-f": "repo-fin", "uid-c": ""}
	seen := 0
	for _, it := range list.Items {
		w, ok := want[it.UID]
		if !ok {
			continue
		}
		seen++
		if it.RepoID != w {
			t.Errorf("list: job %s repoId = %q, want %q", it.UID, it.RepoID, w)
		}
		d := gateReq(t, h, http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", it.ID), gRoot, "")
		var detail struct {
			RepoID string `json:"repoId"`
		}
		if d.Code != http.StatusOK || json.Unmarshal(d.Body.Bytes(), &detail) != nil {
			t.Fatalf("GET /jobs/%d = %d (%s)", it.ID, d.Code, d.Body)
		}
		if detail.RepoID != w {
			t.Errorf("detail: job %s repoId = %q, want %q", it.UID, detail.RepoID, w)
		}
	}
	if seen != len(want) {
		t.Fatalf("the list showed %d of the %d jobs", seen, len(want))
	}
}
