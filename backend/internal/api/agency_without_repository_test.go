package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// An agency may be created without a repository (2.5.1, SG-40): its
// administrator connects one later. Until then the notice says so, and a
// publish to a repository with no connection is refused by name rather than
// failing in the clone.
func TestSG5_AnAgencyIsCreatedWithoutARepositoryAndConnectsOneLater(t *testing.T) {
	h, pool := repoGateServer(t)
	ctx := context.Background()

	// A repository given without its URL is a mistake, not "none".
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/agencies", gRoot, `{"name":"Ops","repository":{"pat":"glpat-x"}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with a repository and no URL = %d %s", rec.Code, rec.Body)
	}

	rec := gateReq(t, h, http.MethodPost, "/api/v1/agencies", gRoot, `{"name":"Ops"}`)
	var made struct {
		ID         string
		Repository *struct{ ID string }
	}
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" || made.Repository != nil {
		t.Fatalf("create without a repository = %d %s", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE agency_id = ?`, made.ID); n != 0 {
		t.Fatalf("a repository row was made: %d", n)
	}
	open := func() int {
		t.Helper()
		if err := notices.RunChecks(ctx, pool); err != nil {
			t.Fatal(err)
		}
		return count(t, pool, `SELECT COUNT(*) FROM notices WHERE kind = 'agency_no_repository' AND agency_id = ? AND resolved_at IS NULL`, made.ID)
	}
	if n := open(); n != 1 {
		t.Fatalf("agency_no_repository under Ops = %d, want 1", n)
	}

	// Its administrator connects it; the notice clears.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+made.ID+"/git-repo", gRoot,
		`{"repoUrl":"https://git.example.com/ops/defs.git","pat":"glpat-ops-1234"}`); rec.Code != http.StatusCreated {
		t.Fatalf("connect Ops's repository = %d %s", rec.Code, rec.Body)
	}
	if n := open(); n != 0 {
		t.Fatalf("agency_no_repository after connecting = %d", n)
	}
	// And it is deleted, empty, with its repository as before.
	syncsSettled(t, pool)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+made.ID, gRoot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete Ops = %d %s", rec.Code, rec.Body)
	}

	// An agency with no repository is deleted too.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/agencies", gRoot, `{"name":"Bare"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &made)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/"+made.ID, gRoot, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete an agency with no repository = %d %s", rec.Code, rec.Body)
	}
}

// A publish to a repository whose connection has no URL is 422 no_repository,
// not a failure in the clone. Global's unconfigured row was the fixture until
// 2.5.3; the built-in agency has no repository since (CA-6), so it is an
// agency's row from before 2.4.0 with no connection, there when the server
// starts its repositories' sync services.
func TestSG5_APublishToAnUnconnectedRepositoryIsRefusedByName(t *testing.T) {
	t.Setenv("CRONOMICON_GIT_CACHE_DIR", filepath.Join(t.TempDir(), "git-cache", "job-definitions"))
	h, pool := gateServerSeeded(t, nil, func(exec func(string, ...any)) {
		exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag:OPS', 'OPS', '2026-01-01T00:00:00Z')`)
		exec(`INSERT INTO git_repos (id, agency_id, url, branch) VALUES ('repo-ops', 'ag:OPS', '', 'main')`)
	})
	if n := count(t, pool, `SELECT COUNT(*) FROM git_repos WHERE id = 'repo-ops' AND COALESCE(url, '') = ''`); n != 1 {
		t.Fatalf("the fixture's agency repository is not there with no URL")
	}
	body := `{"repoId":"repo-ops","filePath":"schedules/nightly.yaml","content":"apiVersion: cronomicon.io/v1\nkind: Schedule\nmetadata:\n  name: nightly\nspec:\n  cron: '0 2 * * *'\n","commitMessage":"m","new":true}`
	rec := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/schedules/publish", gRoot, body, "If-Match", "deadbeef")
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no_repository") {
		t.Fatalf("publish to OPS's unconnected repository = %d %s", rec.Code, rec.Body)
	}
}
