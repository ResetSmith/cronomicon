package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// rbacServer boots a trusted-header server with access grants seeded so a
// request's Remote-Groups header resolves to a known role. The httptest default
// RemoteAddr (192.0.2.1:1234) is inside the trusted-proxy net, so Remote-* headers
// are honored (PP-B1).
func rbacServer(t *testing.T) http.Handler {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "rbac.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Map one group per role (roles stored lowercase canonical).
	seed := []struct{ group, role string }{
		{"rbac-admins", "admin"},
		{"rbac-approvers", "approver"},
		{"rbac-viewers", "viewer"},
	}
	for i, s := range seed {
		// The grant is what authorizes (RB-15). These cases are about the PERMISSION
		// matrix (which role reaches which route), not about scoping, so each group
		// gets an unrestricted "*" grant and the scope axis stays out of the way.
		if _, err := pool.Exec(
			`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
			 VALUES (?, ?, ?, NULL, 1, '2026-01-01T00:00:00Z')`,
			"g"+string(rune('0'+i)), s.group, s.role); err != nil {
			t.Fatalf("seed grant %s: %v", s.group, err)
		}
	}
	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	return api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
}

// asGroup issues a GET as a member of the given AD group (resolving to its role).
func asGroup(t *testing.T, h http.Handler, path, group string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Remote-User", group+"@example.com")
	req.Header.Set("Remote-Groups", group)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRBACCapabilitiesMatrix verifies /capabilities reports the caller's effective
// permissions per role (PP-B1 B1-8), exercising permsForRoles end-to-end.
func TestRBACCapabilitiesMatrix(t *testing.T) {
	h := rbacServer(t)
	type caps struct {
		ManageRoles     bool `json:"manageRoles"`
		ConfigureApp    bool `json:"configureApp"`
		ManageEnvVars   bool `json:"manageEnvVars"`
		PublishSchedule bool `json:"publishSchedule"`
	}
	want := map[string]caps{
		"rbac-admins":    {true, true, true, true},
		"rbac-approvers": {false, false, false, true},
		"rbac-viewers":   {false, false, false, false},
	}
	for group, exp := range want {
		rec := asGroup(t, h, "/api/v1/capabilities", group)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s GET /capabilities = %d, want 200", group, rec.Code)
		}
		var got caps
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s decode caps: %v", group, err)
		}
		if got != exp {
			t.Errorf("%s caps = %+v, want %+v", group, got, exp)
		}
	}
}

// TestRBACManageRolesGate verifies a ManageRoles-gated route is admin-only: a
// viewer/approver is 403, an admin is 200 (PP-B1 B1-2).
func TestRBACManageRolesGate(t *testing.T) {
	h := rbacServer(t)

	for _, group := range []string{"rbac-viewers", "rbac-approvers"} {
		if rec := asGroup(t, h, "/api/v1/roles", group); rec.Code != http.StatusForbidden {
			t.Errorf("%s GET /roles = %d, want 403", group, rec.Code)
		}
		// A second ManageRoles-gated route, so the gate is proven on the surface
		// rather than on one handler. /ad-group-mappings played this part until
		// v0.57.8 retired it; access-grants is the live privilege-granting surface.
		if rec := asGroup(t, h, "/api/v1/access-grants", group); rec.Code != http.StatusForbidden {
			t.Errorf("%s GET /access-grants = %d, want 403", group, rec.Code)
		}
	}
	if rec := asGroup(t, h, "/api/v1/roles", "rbac-admins"); rec.Code != http.StatusOK {
		t.Errorf("admin GET /roles = %d, want 200", rec.Code)
	}

	// A viewer keeps access to non-gated operational reads.
	if rec := asGroup(t, h, "/api/v1/jobs", "rbac-viewers"); rec.Code != http.StatusOK {
		t.Errorf("viewer GET /jobs = %d, want 200 (not gated)", rec.Code)
	}
}

// TestRBACConfigureAppGate verifies ConfigureApp-gated sensitive reads (Q2) deny
// a viewer and allow an admin (PP-B1 B1-3/B1-7).
func TestRBACConfigureAppGate(t *testing.T) {
	h := rbacServer(t)
	for _, path := range []string{"/api/v1/settings/gitlab", "/api/v1/settings/vault", "/api/v1/audit/export"} {
		if rec := asGroup(t, h, path, "rbac-viewers"); rec.Code != http.StatusForbidden {
			t.Errorf("viewer GET %s = %d, want 403", path, rec.Code)
		}
		if rec := asGroup(t, h, path, "rbac-admins"); rec.Code == http.StatusForbidden {
			t.Errorf("admin GET %s = 403, want allowed", path)
		}
	}
}

// TestRBACSshCredentialsWriteGate verifies the SSH key credential WRITE route is
// ConfigureApp-gated (ssh-keys-update.md SK.15). With a valid CSRF token (so the
// shared CSRF check is satisfied and we isolate the perm gate), a viewer is 403
// while an admin gets past the gate into the handler (a 422 on the deliberately
// invalid key body, never 403).
func TestRBACSshCredentialsWriteGate(t *testing.T) {
	h := rbacServer(t)
	const body = `{"label":"x","source":"stored","material":"not-a-key"}`
	for _, tc := range []struct {
		group         string
		wantForbidden bool
	}{
		{"rbac-viewers", true},
		{"rbac-admins", false},
	} {
		// An authenticated GET issues the CSRF cookie the POST must echo.
		var csrf string
		for _, ck := range asGroup(t, h, "/api/v1/capabilities", tc.group).Result().Cookies() {
			if ck.Name == "amadeus_csrf" {
				csrf = ck.Value
			}
		}
		if csrf == "" {
			t.Fatalf("%s: no CSRF cookie issued on GET", tc.group)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/ssh/credentials", strings.NewReader(body))
		req.Header.Set("Remote-User", tc.group+"@example.com")
		req.Header.Set("Remote-Groups", tc.group)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: csrf})
		req.Header.Set("X-CSRF-Token", csrf)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if tc.wantForbidden && rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST /ssh/credentials = %d, want 403 (perm gate)", rec.Code)
		}
		if !tc.wantForbidden && rec.Code == http.StatusForbidden {
			t.Errorf("admin POST /ssh/credentials = 403, want past the perm gate")
		}
	}
}

// TestRunPathScopeGuard verifies the F-R write-side scope-access mirror on the run
// path (architecture-update.md §8): a scope-restricted caller gets 403 triggering a
// job bound to a scope outside their allowed set, is allowed for an in-scope job,
// and an unrestricted caller (admin — the explicit "*" grant) is never scope-gated.
// Mirrors the listRuns read filter (A5).
//
// It is ALSO the FR-M1 regression as of v0.56.4 (RB-2): the per-role triggerJobs
// verb is enforced here now, so a viewer is denied even IN scope. That gate was
// deferred for the whole life of the project — computed, displayed, and checked
// nowhere — which meant a Tax viewer could trigger Tax jobs. The operator case
// below is what proves the change narrowed rather than simply broke the route.
func TestRunPathScopeGuard(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "rbac_run.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Clear the migration-630 "*" backfill so this test defines the exact matrix.
	// RB-2: an operator restricted to ScopeA — holds the verb, so the route must
	// still work for them. Without this the test could pass on a route that denies
	// everyone.
	// RB-15: the same matrix, expressed as grants — agency-shaped for the scoped
	// roles (RB-Q1 has no single-scope shape) and "*" for the unrestricted admin.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-a','agency-a','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-a','ScopeA','amadeus','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-a','ag-a')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('gv','run-viewers','viewer','ag-a',0,'2026-01-01T00:00:00Z'),
		('go','run-operators','operator','ag-a',0,'2026-01-01T00:00:00Z'),
		('ga','run-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)
	// Restrict the viewer role to ScopeA only; admin holds the explicit "*" grant ⇒
	// unrestricted (A5 scope-model: an empty grant now means ZERO — mirrors the
	// Phase-5 backfill for a role that was unrestricted by having no rows).
	// Two git jobs, one bound to each scope.
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-a','git','bash','echo a','ScopeA','sha256:a','jobs/a.yaml','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-b','git','bash','echo b','ScopeB','sha256:b','jobs/b.yaml','t')`)
	rowid := func(name string) string {
		t.Helper()
		var id int64
		if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name=?`, name).Scan(&id); err != nil {
			t.Fatalf("rowid %s: %v", name, err)
		}
		return strconv.FormatInt(id, 10)
	}
	jobA, jobB := rowid("job-a"), rowid("job-b")

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()

	// POST /jobs/{id}/run as a group member, with a matching CSRF cookie+header.
	run := func(group, jobID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs/"+jobID+"/run", nil)
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := run("run-viewers", jobB); code != http.StatusForbidden {
		t.Errorf("viewer trigger of out-of-scope job (ScopeB) = %d, want 403", code)
	}
	// 🔴 THE FR-M1 CHANGE. A viewer holds scope access to ScopeA and still may not
	// execute, because viewer carries no triggerJobs. Before v0.56.4 this was 202.
	if code := run("run-viewers", jobA); code != http.StatusForbidden {
		t.Errorf("viewer trigger of IN-scope job = %d, want 403 — the triggerJobs verb "+
			"is enforced as of v0.56.4 (RB-2/FR-M1)", code)
	}
	// …and the verb is what distinguishes them: an operator on the same scope runs it.
	if code := run("run-operators", jobA); code != http.StatusAccepted {
		t.Errorf("operator trigger of in-scope job = %d, want 202 — RB-2 must narrow, not revoke", code)
	}
	// Scope still bounds the verb: the operator holds triggerJobs but not ScopeB.
	if code := run("run-operators", jobB); code != http.StatusForbidden {
		t.Errorf("operator trigger of out-of-scope job = %d, want 403", code)
	}
	if code := run("run-admins", jobB); code != http.StatusAccepted {
		t.Errorf("admin (unrestricted) trigger of any scope = %d, want 202", code)
	}
}
