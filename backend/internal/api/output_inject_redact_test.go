package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"database/sql"
	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/web"
)

// injectionServer builds a dev-auth test server with secret injection enabled and
// a KEK so stored secrets can be created — the fixture for the H2 run-detail
// output-masking tests.
func injectionServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "inj.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	cfg := &config.Config{
		Addr:                    ":0",
		DevAuth:                 true,
		SecretsInjectionEnabled: true,
		SecretKEKEnv:            "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	srv := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool, WebFS: web.DistFS()})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, pool
}

func fetchRunOutputs(t *testing.T, client *http.Client, ts *httptest.Server, trace string) map[string]string {
	t.Helper()
	resp, err := client.Get(ts.URL + "/api/v1/runs/" + trace)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get run = %d, want 200", resp.StatusCode)
	}
	var run struct {
		Outputs map[string]string `json:"outputs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	return run.Outputs
}

// TestRunDetailMasksInjectedOutputValue (H2): the run-detail path resolves the
// run's bound secret and masks its value where it appears in a captured output,
// while leaving a clean value visible. This exercises the per-run injected
// dictionary that the plain per-scope redactor does not carry for vault-source
// values.
func TestRunDetailMasksInjectedOutputValue(t *testing.T) {
	ts, pool := injectionServer(t)
	client := devLoginClient(t, ts)

	const scope = "prod"
	const secretVal = "INJECTED-DBPASS-9f3a2b"
	sec := secrets.New(pool, &config.Config{SecretKEKEnv: "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM="}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sp := scope
	if _, err := sec.Create(context.Background(), secrets.CreateInput{Key: "DB_PASS", Source: "stored", Scope: &sp, Value: secretVal}, "seed"); err != nil {
		t.Fatalf("create secret: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','DB_PASS','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	const trace = "run-inj-00000001"
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, scope, triggered_by, trigger_kind, created_at, outputs_json)
		VALUES(?, 'j1', 'amadeus', 'bash', 'success', ?, 't@x', 'manual', '2026-01-01T00:00:00Z', ?)`,
		trace, scope, `{"LEAK":"`+secretVal+`","CLEAN":"pg-prod-01"}`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	out := fetchRunOutputs(t, client, ts, trace)
	if out["LEAK"] != "[REDACTED]" {
		t.Errorf("injected secret value not masked in run detail: LEAK=%q", out["LEAK"])
	}
	if out["CLEAN"] != "pg-prod-01" {
		t.Errorf("clean output wrongly altered: CLEAN=%q", out["CLEAN"])
	}
}

// TestRunDetailFailsClosedWhenInjectedUnresolvable (H2): if a run declared a
// secret binding whose value can no longer be resolved (deleted secret / Vault
// outage), the detail path cannot know what to mask, so it fails closed and masks
// ALL outputs rather than risk rendering a secret in the clear.
func TestRunDetailFailsClosedWhenInjectedUnresolvable(t *testing.T) {
	ts, pool := injectionServer(t)
	client := devLoginClient(t, ts)

	const scope = "prod"
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, synced_at)
		VALUES('j1','amadeus','bash','echo hi','Allow','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	// Bind a secret that does NOT exist → resolution fails → fail closed.
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_at)
		VALUES('job','amadeus','j1','secret','GONE','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	const trace = "run-inj-00000002"
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, scope, triggered_by, trigger_kind, created_at, outputs_json)
		VALUES(?, 'j1', 'amadeus', 'bash', 'success', ?, 't@x', 'manual', '2026-01-01T00:00:00Z', ?)`,
		trace, scope, `{"CLEAN":"pg-prod-01"}`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	out := fetchRunOutputs(t, client, ts, trace)
	if out["CLEAN"] != "[REDACTED]" {
		t.Errorf("expected fail-closed masking of all outputs, got CLEAN=%q", out["CLEAN"])
	}
}
