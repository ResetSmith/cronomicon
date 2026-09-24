package api_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
)

func TestIDORScopeGates(t *testing.T) {
	tempDir := t.TempDir()
	logDir := filepath.Join(tempDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		t.Fatalf("mkdir log dir: %v", err)
	}

	pool, err := db.Open(filepath.Join(tempDir, "idor.db"))
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
	// Set up AD mappings and scope restrictions
	// Restrict viewer to Staging scope only; admin is unrestricted via the explicit
	// "*" grant (A5 scope-model: an empty grant now means ZERO, so "unrestricted"
	// must be seeded — mirrors the Phase-5 backfill).
	// RB-15: scope access resolves from access_grants now. The viewer's reach is
	// expressed the way a real grant is — an agency containing Staging (RB-Q1: there
	// is no single-scope grant shape) — which expands back to exactly that scope at
	// login, so every assertion below is unchanged.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-stg','agency-staging','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-stg','Staging','amadeus','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-stg','ag-stg')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at)
	      VALUES ('g-view','run-viewers','viewer','ag-stg',0,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at)
	      VALUES ('g-admin','run-admins','admin',NULL,1,'2026-01-01T00:00:00Z')`)

	// Set up jobs with different scopes
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-prod','git','bash','echo prod','Production','sha256:p','jobs/p.yaml','t')`)
	exec(`INSERT INTO jobs(name, source, run_type, command, scope, content_hash, source_path, synced_at)
	      VALUES('job-stage','git','bash','echo stage','Staging','sha256:s','jobs/s.yaml','t')`)

	// Set up a log storage config so the server resolves logDir correctly
	exec(`INSERT INTO log_storage_config(id, backend, local_path) VALUES(1, 'local', ?)`, logDir)

	cfg := &config.Config{
		Addr:           ":0",
		CookieSecure:   false,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, logger)

	srv := api.New(api.Options{
		Config: cfg,
		Logger: logger,
		Auth:   authSvc,
		DB:     pool,
	})

	dispatch := func(method, path, group string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Remote-User", group+"@example.com")
		req.Header.Set("Remote-Groups", group)
		// Set IP to a trusted proxy
		req.RemoteAddr = "192.0.2.1:1234"
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "dummy-csrf-token"})
		req.Header.Set("X-CSRF-Token", "dummy-csrf-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// 1. Seed Runs
	const runProdID = "run-prod-1"
	const runStageID = "run-stage-1"
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, executor, scope, created_at)
	      VALUES (?, 'job-prod', 'bash', 'success', 't', 'manual', 'ssh', 'Production', '2026-01-01T00:00:00Z')`, runProdID)
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, executor, scope, created_at)
	      VALUES (?, 'job-stage', 'bash', 'success', 't', 'manual', 'ssh', 'Staging', '2026-01-01T00:00:00Z')`, runStageID)

	// Create dummy log files
	err = os.WriteFile(filepath.Join(logDir, runProdID+".log"), []byte("prod output"), 0644)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(logDir, runStageID+".log"), []byte("stage output"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// 2. Test getRun (GET /api/v1/runs/{traceId})
	t.Run("getRun scope gate", func(t *testing.T) {
		// Viewer requesting Staging run -> 200 OK
		rec := dispatch(http.MethodGet, "/api/v1/runs/"+runStageID, "run-viewers")
		if rec.Code != http.StatusOK {
			t.Errorf("viewer get Staging run: status = %d, want 200", rec.Code)
		}

		// Viewer requesting Production run -> 403 Forbidden
		rec = dispatch(http.MethodGet, "/api/v1/runs/"+runProdID, "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer get Production run: status = %d, want 403", rec.Code)
		}

		// Admin requesting Production run -> 200 OK
		rec = dispatch(http.MethodGet, "/api/v1/runs/"+runProdID, "run-admins")
		if rec.Code != http.StatusOK {
			t.Errorf("admin get Production run: status = %d, want 200", rec.Code)
		}
	})

	// 3. Test HandleGetLog (GET /api/v1/runs/{traceId}/log)
	t.Run("HandleGetLog scope gate", func(t *testing.T) {
		// Viewer requesting Staging log -> 200 OK
		rec := dispatch(http.MethodGet, "/api/v1/runs/"+runStageID+"/log", "run-viewers")
		if rec.Code != http.StatusOK {
			t.Errorf("viewer get Staging log: status = %d, want 200", rec.Code)
		}

		// Viewer requesting Production log -> 403 Forbidden
		rec = dispatch(http.MethodGet, "/api/v1/runs/"+runProdID+"/log", "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer get Production log: status = %d, want 403", rec.Code)
		}

		// Admin requesting Production log -> 200 OK
		rec = dispatch(http.MethodGet, "/api/v1/runs/"+runProdID+"/log", "run-admins")
		if rec.Code != http.StatusOK {
			t.Errorf("admin get Production log: status = %d, want 200", rec.Code)
		}
	})

	// 4. Test getWorkflowRun (GET /api/v1/workflow-runs/{traceId})
	t.Run("getWorkflowRun scope gate", func(t *testing.T) {
		// Seed workflow run A: steps snapshot contains prod job, no child runs yet
		const wfProdID = "wf-prod-run"
		snapshotProd := `[{"type":"job","name":"job-prod","id":"r-prod"}]`
		exec(`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot)
		      VALUES (?, 1, 'wf-prod', 'success', 't', 'manual', '2026-01-01T00:00:00Z', ?)`, wfProdID, snapshotProd)

		// Viewer requesting Prod workflow run (due to steps snapshot) -> 403 Forbidden
		rec := dispatch(http.MethodGet, "/api/v1/workflow-runs/"+wfProdID, "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer get Prod workflow run (snapshot): status = %d, want 403", rec.Code)
		}

		// Admin requesting Prod workflow run -> 200 OK
		rec = dispatch(http.MethodGet, "/api/v1/workflow-runs/"+wfProdID, "run-admins")
		if rec.Code != http.StatusOK {
			t.Errorf("admin get Prod workflow run (snapshot): status = %d, want 200", rec.Code)
		}

		// Seed workflow run B: steps snapshot contains stage job, no child runs yet
		const wfStageID = "wf-stage-run"
		snapshotStage := `[{"type":"job","name":"job-stage","id":"r-stage"}]`
		exec(`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot)
		      VALUES (?, 2, 'wf-stage', 'success', 't', 'manual', '2026-01-01T00:00:00Z', ?)`, wfStageID, snapshotStage)

		// Viewer requesting Stage workflow run -> 200 OK
		rec = dispatch(http.MethodGet, "/api/v1/workflow-runs/"+wfStageID, "run-viewers")
		if rec.Code != http.StatusOK {
			t.Errorf("viewer get Stage workflow run: status = %d, want 200", rec.Code)
		}

		// Seed workflow run C: steps snapshot is empty/invalid, but child run has scope Production
		const wfChildProdID = "wf-child-prod-run"
		exec(`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot)
		      VALUES (?, 3, 'wf-child-prod', 'success', 't', 'manual', '2026-01-01T00:00:00Z', '')`, wfChildProdID)
		exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, executor, scope, workflow_run_id, created_at)
		      VALUES ('c-run-prod', 'job-prod', 'bash', 'success', 't', 'workflow', 'ssh', 'Production', ?, '2026-01-01T00:00:00Z')`, wfChildProdID)

		// Viewer requesting Stage workflow run with a Prod child -> 403 Forbidden
		rec = dispatch(http.MethodGet, "/api/v1/workflow-runs/"+wfChildProdID, "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer get workflow run with Prod child: status = %d, want 403", rec.Code)
		}
	})

	// 5. Test cancelWorkflowRun (POST /api/v1/workflows/runs/{traceId}/cancel)
	t.Run("cancelWorkflowRun scope gate", func(t *testing.T) {
		// Viewer cancelling Prod workflow run (due to steps snapshot) -> 403 Forbidden
		rec := dispatch(http.MethodPost, "/api/v1/workflows/runs/wf-prod-run/cancel", "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer cancel Prod workflow run (snapshot): status = %d, want 403", rec.Code)
		}

		// Viewer cancelling Stage workflow run -> 403: cancel is run suppression and
		// takes the killJobs verb since RF-6 (this assertion used to expect 409,
		// i.e. "the scope check is the ONLY gate" — that was the pre-RB-2 rule and
		// this is its regression test now). The admin case below proves the route
		// still reaches the 409 status check for a verb-holder.
		rec = dispatch(http.MethodPost, "/api/v1/workflows/runs/wf-stage-run/cancel", "run-viewers")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer cancel Stage workflow run: status = %d, want 403 (killJobs required, RF-6)", rec.Code)
		}

		// Admin cancelling Prod workflow run -> 409 Conflict (scope check passed)
		rec = dispatch(http.MethodPost, "/api/v1/workflows/runs/wf-prod-run/cancel", "run-admins")
		if rec.Code != http.StatusConflict {
			t.Errorf("admin cancel Prod workflow run: status = %d, want 409", rec.Code)
		}
	})

	// 6. SU-2: getJob detail scope gate — 404 (no existence oracle, per SU-Q2)
	t.Run("getJob scope gate", func(t *testing.T) {
		var prodID, stageID int64
		_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='job-prod'`).Scan(&prodID)
		_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='job-stage'`).Scan(&stageID)

		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", stageID), "run-viewers"); rec.Code != http.StatusOK {
			t.Errorf("viewer get Staging job: status = %d, want 200", rec.Code)
		}
		// Out-of-scope detail is 404 (not 403) so it cannot serve as an existence oracle.
		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", prodID), "run-viewers"); rec.Code != http.StatusNotFound {
			t.Errorf("viewer get Production job: status = %d, want 404", rec.Code)
		}
		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d", prodID), "run-admins"); rec.Code != http.StatusOK {
			t.Errorf("admin get Production job: status = %d, want 200", rec.Code)
		}
	})

	// 6b. FX-E1: the file-sightings sub-route carries the same gate. It was the
	// first job sub-route added AFTER the sweep existed, and it shipped without
	// the gate — its comment even claimed fetchJobByID provided one. This case is
	// what makes the next such route fail a test instead of a review.
	t.Run("file-sightings scope gate", func(t *testing.T) {
		var prodID, stageID int64
		_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='job-prod'`).Scan(&prodID)
		_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='job-stage'`).Scan(&stageID)

		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d/file-sightings", stageID), "run-viewers"); rec.Code != http.StatusOK {
			t.Errorf("viewer list Staging sightings: status = %d, want 200", rec.Code)
		}
		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d/file-sightings", prodID), "run-viewers"); rec.Code != http.StatusNotFound {
			t.Errorf("viewer list Production sightings: status = %d, want 404 — watched paths and "+
				"refusal reasons are operational detail, and 200 here is also an existence oracle", rec.Code)
		}
		if rec := dispatch(http.MethodGet, fmt.Sprintf("/api/v1/jobs/%d/file-sightings", prodID), "run-admins"); rec.Code != http.StatusOK {
			t.Errorf("admin list Production sightings: status = %d, want 200", rec.Code)
		}
	})

	// 7. SU-2: listJobs must not return out-of-scope rows.
	t.Run("listJobs scope filter", func(t *testing.T) {
		rec := dispatch(http.MethodGet, "/api/v1/jobs", "run-viewers")
		if rec.Code != http.StatusOK {
			t.Fatalf("viewer list jobs: status = %d, want 200", rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "job-prod") {
			t.Errorf("viewer sees out-of-scope Production job in listJobs:\n%s", body)
		} else if !strings.Contains(body, "job-stage") {
			t.Errorf("viewer missing in-scope Staging job in listJobs:\n%s", body)
		}
		rec = dispatch(http.MethodGet, "/api/v1/jobs", "run-admins")
		if b := rec.Body.String(); !strings.Contains(b, "job-prod") || !strings.Contains(b, "job-stage") {
			t.Errorf("admin should see all jobs:\n%s", b)
		}
	})

	// 8. SU-2: listWorkflowRuns filters scope-bearing rows (mirrors getWorkflowRun).
	t.Run("listWorkflowRuns scope filter", func(t *testing.T) {
		exec(`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot, scope)
		      VALUES ('wf-scoped-prod', 10, 'wf-scoped-prod', 'success', 't', 'manual', '2026-01-02T00:00:00Z', '', 'Production')`)
		exec(`INSERT INTO workflow_runs(id, workflow_id, workflow_name, status, triggered_by, trigger_kind, created_at, steps_snapshot, scope)
		      VALUES ('wf-scoped-stage', 11, 'wf-scoped-stage', 'success', 't', 'manual', '2026-01-02T00:00:00Z', '', 'Staging')`)

		rec := dispatch(http.MethodGet, "/api/v1/workflow-runs", "run-viewers")
		if rec.Code != http.StatusOK {
			t.Fatalf("viewer list workflow-runs: status = %d, want 200", rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "wf-scoped-prod") {
			t.Errorf("viewer sees out-of-scope Production workflow run:\n%s", body)
		} else if !strings.Contains(body, "wf-scoped-stage") {
			t.Errorf("viewer missing in-scope Staging workflow run:\n%s", body)
		}
		rec = dispatch(http.MethodGet, "/api/v1/workflow-runs", "run-admins")
		if !strings.Contains(rec.Body.String(), "wf-scoped-prod") {
			t.Errorf("admin should see the Production workflow run")
		}
	})

	// 9. SU-2: listSchedules must not leak a scoped job owner's plaintext env.
	t.Run("listSchedules scope filter", func(t *testing.T) {
		exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, env, position)
		      VALUES('git','job','job-prod','nightly','0 0 * * *','{"PROD_ENV":"prod-secret-val"}',0)`)
		exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, env, position)
		      VALUES('git','job','job-stage','nightly','0 0 * * *','{"STAGE_ENV":"stage-val"}',0)`)

		rec := dispatch(http.MethodGet, "/api/v1/schedules", "run-viewers")
		if rec.Code != http.StatusOK {
			t.Fatalf("viewer list schedules: status = %d, want 200", rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "prod-secret-val") {
			t.Errorf("viewer sees out-of-scope Production schedule env:\n%s", body)
		} else if !strings.Contains(body, "stage-val") {
			t.Errorf("viewer missing in-scope Staging schedule env:\n%s", body)
		}
		rec = dispatch(http.MethodGet, "/api/v1/schedules", "run-admins")
		if !strings.Contains(rec.Body.String(), "prod-secret-val") {
			t.Errorf("admin should see the Production schedule env")
		}

		// CAL-12 — the calendar roll-up is a SECOND channel on this same response
		// and must obey the same filter. It is keyed by job name and its value is
		// the job's compliance policy, so building it from unfiltered rows leaks
		// both through a field the items array correctly hides.
		exec(`UPDATE definition_schedules SET skip_calendars = '["treasury-blackout"]'
		      WHERE owner_name = 'job-prod'`)
		rec = dispatch(http.MethodGet, "/api/v1/schedules", "run-viewers")
		if body := rec.Body.String(); strings.Contains(body, "treasury-blackout") || strings.Contains(body, "job-prod") {
			t.Errorf("viewer sees the out-of-scope job's calendar roll-up:\n%s", body)
		}
		rec = dispatch(http.MethodGet, "/api/v1/schedules", "run-admins")
		if !strings.Contains(rec.Body.String(), "treasury-blackout") {
			t.Errorf("admin should see the roll-up")
		}
	})

	// 10. SU-2: pause/resume also return the full job detail (script + plaintext env)
	// AND mutate the job — they must be gated like getJob (404 out-of-scope), else a
	// restricted actor exfiltrates the detail via these routes.
	t.Run("pause/resume scope gate", func(t *testing.T) {
		var prodID int64
		_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='job-prod'`).Scan(&prodID)
		for _, action := range []string{"pause", "resume"} {
			path := fmt.Sprintf("/api/v1/jobs/%d/%s", prodID, action)
			if rec := dispatch(http.MethodPost, path, "run-viewers"); rec.Code != http.StatusNotFound {
				t.Errorf("viewer %s Production job: status = %d, want 404 (no leak)", action, rec.Code)
			}
			if rec := dispatch(http.MethodPost, path, "run-admins"); rec.Code != http.StatusOK {
				t.Errorf("admin %s Production job: status = %d, want 200", action, rec.Code)
			}
		}
	})
}
