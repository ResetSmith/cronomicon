package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A publish that names a repository is that repository's (GR-24): its Service
// commits and pushes, to its remote, on its branch, with its head as the base;
// the answer and the audit row say which repository it was. A publish that
// names none was Global's until 2.5.3, and is refused since (no_repository).
func TestAPublishThatNamesARepositoryLandsInIt(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	// The fixture's Service is made another repository's.
	if _, err := svc.db.Exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag-b', 'B', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.db.Exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-b', 'ag-b', ?, ?)`, bare, branch); err != nil {
		t.Fatal(err)
	}
	svc.repoID, svc.agencyID = "repo-b", "ag-b"
	synced := svc.SyncBlocking(ctx, "manual")
	if synced.Status == "failed" {
		t.Fatalf("sync: %s", synced.ErrorMessage)
	}
	h := NewHandlers(svc)
	var asked []PublishTarget
	publish := func(repo string) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"filePath": "jobs/b-new.yaml", "content": jobYAML("b-new")}
		if repo != "" {
			body["repoId"] = repo
		}
		b, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules/publish", strings.NewReader(string(b)))
		req.Header.Set("If-Match", synced.SHA)
		rec := httptest.NewRecorder()
		h.PublishSchedule("a@example.com", func(_ *http.Request, t PublishTarget) string {
			asked = append(asked, t)
			return ""
		}, rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Naming none is the built-in agency's, which has no repository since
	// 2.5.3 (CA-6): nothing to publish to, as for an agency that has not
	// connected one. (Until 2.5.3 it was 503, Global's Service not running.)
	if code, out := publish(""); code != http.StatusUnprocessableEntity || out["code"] != "no_repository" {
		t.Errorf("a publish that names no repository answered %d %v, want 422 no_repository", code, out)
	}
	// One that is not there: the authorizer is asked, with nothing but that,
	// and then the caller it allowed is told.
	if code, out := publish("repo-nope"); code != http.StatusNotFound {
		t.Errorf("a publish to a repository that is not there answered %d %v, want 404", code, out)
	}
	if len(asked) != 2 || asked[0].Repo != "global" || !asked[0].NoSuchRepo || asked[1].Repo != "repo-nope" || !asked[1].NoSuchRepo {
		t.Errorf("the authorizer was asked %+v, want Global's and repo-nope, each as not there", asked)
	}

	code, out := publish("repo-b")
	if code != http.StatusCreated || out["repoId"] != "repo-b" || out["branch"] != branch {
		t.Fatalf("the publish to repo-b answered %d %v", code, out)
	}
	if last := asked[len(asked)-1]; last.Repo != "repo-b" || last.Agency != "ag-b" || last.NoSuchRepo || last.Kind != "job" || last.Name != "b-new" {
		t.Errorf("the authorizer was asked %+v, want repo-b of ag-b and the job's facts", last)
	}
	if head := sysGit(t, bare, "rev-parse", "refs/heads/"+branch); head != out["commitSha"] {
		t.Errorf("the repository's branch is at %s, the publish answered %v", head, out["commitSha"])
	}
	var repo, status string
	if err := svc.db.QueryRow(`SELECT repo_id, status FROM schedule_pushes ORDER BY id DESC LIMIT 1`).Scan(&repo, &status); err != nil || repo != "repo-b" || status != "success" {
		t.Errorf("the audit row is of %q, %q (%v), want repo-b, success", repo, status, err)
	}
}
