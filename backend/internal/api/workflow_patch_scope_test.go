package api_test

import (
	"context"
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

// TestWorkflowPatchScopeGuard covers FX-19: PATCH /workflows/{id} — the
// pause/resume route — used to be requireSession + requireCSRF and nothing else.
// No role check, and no scope check either, so ANY authenticated user could pause
// ANY workflow in the system and suppress someone else's schedule.
//
// Its two siblings both guard: triggerWorkflow over every constituent job's scope
// (WB-S1/D1, TestWorkflowTriggerScopeGuard) and pauseJob/resumeJob via
// auth.ScopeReadable (SU-2). This route was missed by both sweeps. It now carries
// triggerWorkflow's guard, so this mirrors that test on the pause path.
func TestWorkflowPatchScopeGuard(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "wf_patch.db"))
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
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-a','ScopeA','amadeus','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-a','ag-a')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at) VALUES
		('gv','wf-viewers','viewer','ag-a',0,'2026-01-01T00:00:00Z'),
		('go','wf-operators','operator','ag-a',0,'2026-01-01T00:00:00Z'),
		('ga','wf-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-a','git','bash','echo a','ScopeA','sha256:a','jobs/a.yaml','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-b','git','bash','echo b','ScopeB','sha256:b','jobs/b.yaml','t')`)
	exec(`INSERT INTO workflows(name, source, description, steps, enabled, created_at, last_modified_at)
	      VALUES('wf-a','amadeus','', '[{"type":"job","name":"job-a"}]', 1, 't','t')`)
	exec(`INSERT INTO workflows(name, source, description, steps, enabled, created_at, last_modified_at)
	      VALUES('wf-b','amadeus','', '[{"type":"job","name":"job-b"}]', 1, 't','t')`)
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

	pause := func(group, wfID string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/workflows/"+wfID, strings.NewReader(`{"disabled":true}`))
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	pausedRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM paused_jobs WHERE owner_kind='workflow'`).Scan(&n); err != nil {
			t.Fatalf("count paused: %v", err)
		}
		return n
	}

	// The hole itself: a caller restricted to ScopeA pausing a ScopeB workflow.
	if code := pause("wf-viewers", wfB); code != http.StatusForbidden {
		t.Errorf("viewer pause of out-of-scope workflow (ScopeB job) = %d, want 403", code)
	}
	// ...and nothing was written. A guard that answers 403 after taking the action
	// would be no guard at all.
	if n := pausedRows(); n != 0 {
		t.Errorf("refused pause still wrote %d paused_jobs row(s), want 0", n)
	}

	// 🔴 FR-M1 (RB-2, v0.56.4): pausing is a run-suppression action and takes
	// killJobs, which a viewer does not hold — so an in-scope viewer is denied now.
	if code := pause("wf-viewers", wfA); code != http.StatusForbidden {
		t.Errorf("viewer pause of IN-scope workflow = %d, want 403 (RB-2/FR-M1)", code)
	}
	if n := pausedRows(); n != 0 {
		t.Errorf("refused pause still wrote %d paused_jobs row(s), want 0", n)
	}

	// The in-scope case must keep working for someone who HOLDS the verb — this is
	// a guard, not a lockout.
	if code := pause("wf-operators", wfA); code != http.StatusOK {
		t.Errorf("operator pause of in-scope workflow = %d, want 200 — RB-2 must narrow, not revoke", code)
	}
	if n := pausedRows(); n != 1 {
		t.Errorf("allowed pause wrote %d paused_jobs row(s), want 1", n)
	}

	// An unrestricted actor is never scope-gated, matching triggerWorkflow.
	if code := pause("wf-admins", wfB); code != http.StatusOK {
		t.Errorf("admin (unrestricted) pause of any scope = %d, want 200", code)
	}
}
