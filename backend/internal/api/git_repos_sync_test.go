package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The sync, its status, its history and its problems, for a repository other
// than Global's (2.4.0 Phase R6; GR-9, GR-26, GR-30).
//
// A sync is started by who may configure the repository: its agency's
// administrator, and for Global's a global administrator, as before. The
// status, the history and the problems of a repository are read by its
// agency's people and by those who reach every agency; Global's by every
// session. A repository that is not there and one the caller may not read are
// one answer to a read; one answer, another, to a write.
func TestARepositorysSyncAndHistoryAreItsAgencys(t *testing.T) {
	h, pool := repoGateServer(t)
	exec := mustExec(t, pool)
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/git-repo", gFinAdmin, `{"repoUrl":"http://127.0.0.1:1/fin.git"}`)
	var made struct{ ID string }
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &made) != nil || made.ID == "" {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	fin := made.ID
	code := func(method, path, who string) int {
		t.Helper()
		return gateReq(t, h, method, path, who, "").Code
	}

	// Starting a sync.
	for _, c := range []struct {
		who, repo string
		want      int
	}{
		{gFinAdmin, fin, http.StatusAccepted},
		{gRoot, fin, http.StatusAccepted},
		{gRoot, "", http.StatusAccepted},
		{gRoot, "global", http.StatusAccepted},
		{gTaxAdmin, fin, http.StatusForbidden},
		{gFinApprover, fin, http.StatusForbidden},
		{gAllViewer, fin, http.StatusForbidden},
		{gFinAdmin, "", http.StatusForbidden},       // Global's
		{gFinAdmin, "global", http.StatusForbidden}, // Global's, by name
		{gFinAdmin, "repo-nope", http.StatusForbidden},
		{gTaxAdmin, "repo-nope", http.StatusForbidden},
		{gRoot, "repo-nope", http.StatusNotFound},
	} {
		path := "/api/v1/git/sync"
		if c.repo != "" {
			path += "?repo=" + c.repo
		}
		if got := code(http.MethodPost, path, c.who); got != c.want {
			t.Errorf("%s starting a sync of %q = %d, want %d", c.who, c.repo, got, c.want)
		}
	}

	// Reading: the status, the history, the problems. (Once the syncs that
	// were just started have finished writing theirs.)
	syncsSettled(t, pool)
	exec(`DELETE FROM git_sync_events`)
	exec(`INSERT INTO git_sync_events (repo_id, triggered_by, sha, status, jobs_synced, wfs_synced, scopes_synced, started_at, finished_at) VALUES
	        ('global', 'manual', 'aaaa000000000000000000000000000000000000', 'success', 1, 0, 0, '2026-01-01T00:00:00Z', '2026-01-01T00:00:01Z'),
	        (?,        'manual', 'bbbb000000000000000000000000000000000000', 'success', 2, 0, 0, '2026-01-02T00:00:00Z', '2026-01-02T00:00:01Z'),
	        (?,        'manual', 'cccc000000000000000000000000000000000000', 'success', 3, 0, 0, '2026-01-03T00:00:00Z', '2026-01-03T00:00:01Z')`, fin, fin)
	exec(`DELETE FROM git_sync_problems`)
	exec(`INSERT INTO git_sync_problems (repo_id, path, kind, severity, message, first_seen, last_seen, pass) VALUES
	        ('global', 'jobs/g.yaml',  'job', 'error',   'Global''s file is wrong', 't1', 't2', 'p'),
	        (?,        'jobs/b.yaml',  'job', 'warning', 'FIN''s file warns',       't1', 't2', 'p'),
	        (?,        'jobs/a.yaml',  'job', 'error',   'FIN''s file is wrong',    't1', 't2', 'p')`, fin, fin)
	exec(`UPDATE git_repos SET last_sha = 'cccc000000000000000000000000000000000000', last_status = 'success', last_synced_at = 't' WHERE id = ?`, fin)

	reads := []string{"/api/v1/git/sync?repo=", "/api/v1/git/history?repo=", "/api/v1/git-repos/%s/problems"}
	path := func(tmpl, repo string) string {
		if strings.Contains(tmpl, "%s") {
			return strings.Replace(tmpl, "%s", repo, 1)
		}
		return tmpl + repo
	}
	for _, tmpl := range reads {
		// FIN's: FIN's people, whatever their role, and those who reach everything.
		for _, who := range []string{gFinAdmin, gFinApprover, gFinOperator, gFinViewer, gAllViewer, gRoot} {
			if got := code(http.MethodGet, path(tmpl, fin), who); got != http.StatusOK {
				t.Errorf("%s reading %s = %d, want 200", who, path(tmpl, fin), got)
			}
		}
		// Another agency's people: as if it were not there.
		if got, gone := code(http.MethodGet, path(tmpl, fin), gTaxAdmin), code(http.MethodGet, path(tmpl, "repo-nope"), gTaxAdmin); got != http.StatusNotFound || gone != got {
			t.Errorf("TAX's administrator reading %s = %d, and one that is not there = %d; want 404 for both", path(tmpl, fin), got, gone)
		}
		if got := code(http.MethodGet, path(tmpl, "repo-nope"), gRoot); got != http.StatusNotFound {
			t.Errorf("a global administrator reading %s of a repository that is not there = %d, want 404", tmpl, got)
		}
		// Global's: those who reach every agency (2.5.0 C2, question 77; it
		// was every session's). To anyone else, as if it were not there.
		for _, who := range []string{gAllViewer, gRoot} {
			if got := code(http.MethodGet, path(tmpl, "global"), who); got != http.StatusOK {
				t.Errorf("%s reading %s = %d, want 200", who, path(tmpl, "global"), got)
			}
		}
		for _, who := range []string{gTaxAdmin, gFinViewer} {
			if got := code(http.MethodGet, path(tmpl, "global"), who); got != http.StatusNotFound {
				t.Errorf("%s reading %s = %d, want 404", who, path(tmpl, "global"), got)
			}
		}
	}

	// Each list is its repository's, and no other's.
	history := func(who, repo string) string {
		p := "/api/v1/git/history"
		if repo != "" {
			p += "?repo=" + repo
		}
		return gateReq(t, h, http.MethodGet, p, who, "").Body.String()
	}
	if got := history(gRoot, ""); !strings.Contains(got, "aaaa0000") || strings.Contains(got, "bbbb0000") || strings.Contains(got, "cccc0000") {
		t.Errorf("the history with no repository named is not Global's alone: %s", got)
	}
	if got := history(gFinViewer, fin); strings.Contains(got, "aaaa0000") || !strings.Contains(got, "bbbb0000") || !strings.Contains(got, "cccc0000") || !strings.Contains(got, `"totalItems":2`) {
		t.Errorf("FIN's repository's history is not FIN's alone: %s", got)
	}
	if got := gateReq(t, h, http.MethodGet, "/api/v1/git/sync?repo="+fin, gFinViewer, "").Body.String(); !strings.Contains(got, "cccc0000") {
		t.Errorf("FIN's repository's status is not its own: %s", got)
	}
	problems := gateReq(t, h, http.MethodGet, "/api/v1/git-repos/"+fin+"/problems", gFinViewer, "").Body.String()
	if strings.Contains(problems, "Global's file") || !strings.Contains(problems, "FIN's file is wrong") || !strings.Contains(problems, "FIN's file warns") {
		t.Errorf("FIN's repository's problems are not FIN's alone: %s", problems)
	}
	if strings.Index(problems, "FIN's file is wrong") > strings.Index(problems, "FIN's file warns") {
		t.Errorf("the problems do not list errors first: %s", problems)
	}
	if got := gateReq(t, h, http.MethodGet, "/api/v1/git-repos/global/problems", gRoot, "").Body.String(); !strings.Contains(got, "Global's file is wrong") || strings.Contains(got, "FIN's") {
		t.Errorf("Global's repository's problems are not Global's alone: %s", got)
	}
}
