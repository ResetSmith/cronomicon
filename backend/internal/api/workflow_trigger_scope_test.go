package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

// TestWorkflowTriggerScopeGuard verifies the WB-S1 fix: the workflow trigger now
// enforces the same scope-access guard runJob applies (architecture-update.md §8),
// but over EVERY constituent job. A scope-restricted caller gets 403 triggering a
// workflow whose job is bound to a scope outside their allowed set, is allowed for
// an in-scope workflow, and an unrestricted (admin) caller is never scope-gated.
// Mirrors TestRunPathScopeGuard on the single-job run path.
func TestWorkflowTriggerScopeGuard(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "wf_trigger.db"))
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
	// still work for them once the viewer stops being able to use it.
	// RB-15: the same matrix as grants — agency-shaped for the scoped roles, "*"
	// for the unrestricted admin. The agency contains ScopeA, so it expands back to
	// exactly that scope at login.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-a','agency-a','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-a','ScopeA','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-a','ag-a')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('gv','wf-viewers','viewer','ag-a',0,'2026-01-01T00:00:00Z'),
		('go','wf-operators','operator','ag-a',0,'2026-01-01T00:00:00Z'),
		('ga','wf-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)
	// Restrict the viewer role to ScopeA only; admin holds the explicit "*" grant ⇒
	// unrestricted (A5 scope-model: an empty grant now means ZERO — mirrors the
	// Phase-5 backfill for a role that was unrestricted by having no rows).
	// Two git jobs, one bound to each scope, referenced by the workflows below.
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-a','git','bash','echo a','ScopeA','sha256:a','jobs/a.yaml','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-b','git','bash','echo b','ScopeB','sha256:b','jobs/b.yaml','t')`)
	// Two enabled cronomicon workflows: wf-a runs the ScopeA job, wf-b the ScopeB job.
	exec(`INSERT INTO workflows(name, source, description, steps, enabled, created_at, last_modified_at)
	      VALUES('wf-a','cronomicon','', '[{"type":"job","name":"job-a"}]', 1, 't','t')`)
	exec(`INSERT INTO workflows(name, source, description, steps, enabled, created_at, last_modified_at)
	      VALUES('wf-b','cronomicon','', '[{"type":"job","name":"job-b"}]', 1, 't','t')`)
	rowid := func(name string) string {
		t.Helper()
		var id int64
		if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE name=?`, name).Scan(&id); err != nil {
			t.Fatalf("rowid %s: %v", name, err)
		}
		return strconv.FormatInt(id, 10)
	}
	wfA, wfB := rowid("wf-a"), rowid("wf-b")

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()

	// POST /workflows/{id}/trigger as a group member, with a matching CSRF cookie+header.
	trigger := func(group, wfID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/workflows/"+wfID+"/trigger", nil)
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "cronomicon_csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := trigger("wf-viewers", wfB); code != http.StatusForbidden {
		t.Errorf("viewer trigger of out-of-scope workflow (ScopeB job) = %d, want 403", code)
	}
	// 🔴 FR-M1 (RB-2, v0.56.4): the triggerJobs verb is enforced per constituent
	// scope now, so a viewer with scope access is still denied. Before this release
	// it was 202. The unrestricted fast path in workflowScopesReadable was removed
	// for exactly this reason — it would have skipped the verb check.
	if code := trigger("wf-viewers", wfA); code != http.StatusForbidden {
		t.Errorf("viewer trigger of IN-scope workflow = %d, want 403 (RB-2/FR-M1)", code)
	}
	if code := trigger("wf-operators", wfA); code != http.StatusAccepted {
		t.Errorf("operator trigger of in-scope workflow = %d, want 202 — RB-2 must narrow, not revoke", code)
	}
	if code := trigger("wf-operators", wfB); code != http.StatusForbidden {
		t.Errorf("operator trigger of out-of-scope workflow = %d, want 403 — the verb does not widen scope", code)
	}
	if code := trigger("wf-admins", wfB); code != http.StatusAccepted {
		t.Errorf("admin (unrestricted) trigger of any scope = %d, want 202", code)
	}
}
