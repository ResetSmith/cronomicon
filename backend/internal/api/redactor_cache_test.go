package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/runner"
)

// TestRunsListBuildsOneRedactorPerScope is the CC.3 regression: rendering a
// page of runs must build ONE redactor per distinct scope per request, not one
// per row — every build envelope-decrypts all stored secrets. It also covers
// the batched jobs-rowid lookup that replaced the per-row reverse query.
func TestRunsListBuildsOneRedactorPerScope(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "cc3.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s := &Server{
		cfg: &config.Config{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		db:  pool,
	}

	// Two jobs, 40 runs split across two scopes, every run carrying outputs so
	// each row exercises the redaction path.
	jobIDs := map[string]int64{}
	for _, name := range []string{"job-a", "job-b"} {
		res, err := pool.Exec(`
			INSERT INTO jobs(name, source, run_type, command, content_hash, synced_at)
			VALUES (?, 'git', 'bash', 'echo hi', 'h', '2026-01-01T00:00:00Z')`, name)
		if err != nil {
			t.Fatalf("seed job %s: %v", name, err)
		}
		id, _ := res.LastInsertId()
		jobIDs[name] = id
	}
	for i := range 40 {
		jobName, scope := "job-a", "alpha"
		if i%2 == 1 {
			jobName, scope = "job-b", "beta"
		}
		if _, err := pool.Exec(`
			INSERT INTO runs(id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, executor, created_at, outputs_json)
			VALUES (?, ?, 'git', 'bash', ?, 'success', 'test', 'manual', 'ssh', '2026-01-01T00:00:00Z', '{"TOKEN":"value-1"}')`,
			// Distinct ids; identical created_at keeps ordering irrelevant.
			// (Two scopes ⇒ the redactor cache must be hit 38 times.)
			jobKeyForTest(i), jobName, scope); err != nil {
			t.Fatalf("seed run %d: %v", i, err)
		}
	}

	builds := 0
	orig := newRunRedactor
	newRunRedactor = func(ctx context.Context, d *sql.DB, cfg *config.Config, scope string, extra ...string) (*runner.Redactor, error) {
		builds++
		return orig(ctx, d, cfg, scope, extra...)
	}
	t.Cleanup(func() { newRunRedactor = orig })

	// listRuns runs behind RequireSession in production; called directly here it needs
	// an identity. Use an unrestricted ("*") admin so the scope filter shows all runs
	// (A5: no identity now means GLOBAL-only, not "see everything").
	req := httptest.NewRequest("GET", "/api/v1/runs?pageSize=200", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), auth.Identity{
		Roles:         []string{"admin"},
		AllowedScopes: []string{auth.AllScopes},
	}))
	w := httptest.NewRecorder()
	s.listRuns(w, req)

	if w.Code != 200 {
		t.Fatalf("listRuns = %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			JobID   int64             `json:"jobId"`
			JobName string            `json:"jobName"`
			Outputs map[string]string `json:"outputs"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.TotalItems != 40 || len(env.Items) != 40 {
		t.Fatalf("totalItems = %d, len(items) = %d, want 40/40", env.TotalItems, len(env.Items))
	}
	if builds > 2 {
		t.Errorf("redactor built %d times for a 40-row page with 2 scopes, want ≤ 2", builds)
	}
	for _, it := range env.Items {
		if want := jobIDs[it.JobName]; it.JobID != want {
			t.Errorf("run of %s: jobId = %d, want %d (batched rowid lookup)", it.JobName, it.JobID, want)
		}
		if len(it.Outputs) != 1 {
			t.Errorf("run of %s: outputs = %v, want the seeded 1-entry map", it.JobName, it.Outputs)
		}
	}
}

func jobKeyForTest(i int) string { return "run-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) }
