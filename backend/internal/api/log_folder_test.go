// Why this file exists (LU-6/LU-7).
//
// Migration 710 turned a flat directory of `{traceID}.log` files into a tree of
// per-entity folders — while leaving every pre-existing run exactly where it was.
// That means two layouts are live at once, forever (LU-Q8(a)), and the ONLY thing
// selecting between them is the run row's entity_code. Every assertion below
// drives the real ingest and read handlers rather than calling LogPath, because
// the unit-level path builder can be perfectly correct while the handler passes
// it the wrong code, forgets to create the folder, or writes flat anyway — and
// each of those fails silently: the POST still returns 204, the run still goes
// green, and the log is simply not where the reader looks.
//
// The delete-versus-prune asymmetry is asserted here too. Its other half lives in
// internal/gitlab/sync_entitycode_test.go, which proves a sync prune/return cycle
// KEEPS the code; this file proves an explicit operator delete does not. The two
// are deliberately opposite outcomes for the same tuple shape, and a "cleanup"
// that unified them would silently break one or the other.
package api_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/runner"
)

// seedIngestRunner registers a runner with a usable bearer token, since only the
// runner that claimed a run may stream logs into it.
func seedIngestRunner(t *testing.T, pool *sql.DB, runnerID, token string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO runners(id, name, status, os, capabilities, load, max_concurrent, version, registered_at, created_at)
		VALUES (?, ?, 'online', 'Linux', '["bash"]', 0, 5, '1.0', ?, ?)`,
		runnerID, runnerID, now, now); err != nil {
		t.Fatalf("insert runner: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO runner_tokens(token_hash, runner_id, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)`,
		auth.HashToken(token), runnerID, "runner:"+runnerID, now,
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("insert runner token: %v", err)
	}
}

// seedRunningRun inserts a run in the state the ingest handler requires. entityCode
// is written verbatim; "" is stored as NULL, which is exactly the shape of every
// run enqueued before migration 710.
func seedRunningRun(t *testing.T, pool *sql.DB, runnerID, jobName, entityCode string) string {
	t.Helper()
	traceID := db.NewTraceID()
	now := time.Now().UTC().Format(time.RFC3339)
	var code any
	if entityCode != "" {
		code = entityCode
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, scope, status, runner_id, executor,
		                 triggered_by, trigger_kind, entity_code, started_at, created_at)
		VALUES (?, ?, 'bash', '', 'running', ?, 'runner', 'test', 'manual', ?, ?, ?)`,
		traceID, jobName, runnerID, code, now, now); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	return traceID
}

// postRunLog streams a chunk through the real runner ingest endpoint.
func postRunLog(t *testing.T, ts *httptest.Server, token, traceID, body string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/runs/"+traceID+"/log",
		strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST run log: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ingest = %d, want 204; body: %s", resp.StatusCode, respBody)
	}
}

// TestFolderedRunLogLandsInItsEntityFolderAndNotFlat is the core LU-7 write
// assertion. Both halves matter: the log must appear under {logDir}/{code}/ AND
// must not also appear at the flat path — a handler that ignored the code would
// still 204 and still produce a readable log, just in the wrong place, so only
// the negative catches it.
func TestFolderedRunLogLandsInItsEntityFolderAndNotFlat(t *testing.T) {
	logDir := t.TempDir()
	ts, pool := newTestServerWithLogDir(t, logDir)
	ctx := context.Background()

	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "deploy", "uid-deploy")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	const token = "crn_run_folder"
	seedIngestRunner(t, pool, "runner-folder", token)
	traceID := seedRunningRun(t, pool, "runner-folder", "deploy", code)

	postRunLog(t, ts, token, traceID, "foldered output\n")

	foldered := filepath.Join(logDir, code, traceID+".log")
	data, err := os.ReadFile(foldered)
	if err != nil {
		t.Fatalf("run log is not in the entity folder %s: %v", foldered, err)
	}
	if !strings.Contains(string(data), "foldered output") {
		t.Errorf("foldered log = %q, want the ingested output", data)
	}
	if _, err := os.Stat(filepath.Join(logDir, traceID+".log")); err == nil {
		t.Errorf("the run ALSO wrote to the flat path %s — the entity code was ignored on the write path", filepath.Join(logDir, traceID+".log"))
	}

	// And the read handler resolves the same file, so History shows the output.
	client, _ := devLoginWithCSRF(t, ts)
	resp, err := client.Get(ts.URL + "/api/v1/runs/" + traceID + "/log")
	if err != nil {
		t.Fatalf("GET run log: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run log = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(got), "foldered output") {
		t.Errorf("GET run log returned %q — the reader is not looking in the entity folder", got)
	}
}

// TestFlatRunLogStillWritesAndReadsEndToEnd is what LU-Q8(a) bought: the two
// layouts coexist. A run with a NULL entity_code — i.e. every run that existed
// before migration 710 — must still write to and read from {logDir}/{trace}.log,
// with no folder conjured for it. Regressing this would make every historical run
// in History render an empty log while its bytes sit untouched on disk.
func TestFlatRunLogStillWritesAndReadsEndToEnd(t *testing.T) {
	logDir := t.TempDir()
	ts, pool := newTestServerWithLogDir(t, logDir)

	const token = "crn_run_flat"
	seedIngestRunner(t, pool, "runner-flat", token)
	traceID := seedRunningRun(t, pool, "runner-flat", "legacy-job", "")

	// Precondition: the column really is NULL, not an empty string.
	var code sql.NullString
	if err := pool.QueryRow(`SELECT entity_code FROM runs WHERE id=?`, traceID).Scan(&code); err != nil {
		t.Fatalf("read entity_code: %v", err)
	}
	if code.Valid {
		t.Fatalf("seeded run has entity_code %q, want NULL", code.String)
	}

	postRunLog(t, ts, token, traceID, "legacy output\n")

	flat := filepath.Join(logDir, traceID+".log")
	data, err := os.ReadFile(flat)
	if err != nil {
		t.Fatalf("pre-710 run's log is not at the flat path %s: %v", flat, err)
	}
	if !strings.Contains(string(data), "legacy output") {
		t.Errorf("flat log = %q, want the ingested output", data)
	}
	// No stray folder was created for a run that has no code.
	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read log dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("a directory %q appeared in the log root for a run with no entity code", e.Name())
		}
	}

	client, _ := devLoginWithCSRF(t, ts)
	resp, err := client.Get(ts.URL + "/api/v1/runs/" + traceID + "/log")
	if err != nil {
		t.Fatalf("GET run log: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET run log = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(got), "legacy output") {
		t.Errorf("GET run log returned %q — the reader no longer resolves the flat layout", got)
	}
}

// TestFolderedAndFlatRunsCoexistInOneLogDirectory is the coexistence property
// stated directly: one log root, one foldered run and one flat run, both readable.
// Asserted as a pair because a "migrate everything to folders" change would pass
// each single-layout test on its own database and still break this.
func TestFolderedAndFlatRunsCoexistInOneLogDirectory(t *testing.T) {
	logDir := t.TempDir()
	ts, pool := newTestServerWithLogDir(t, logDir)
	ctx := context.Background()

	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "mixed", "uid-mixed")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	const token = "crn_run_mixed"
	seedIngestRunner(t, pool, "runner-mixed", token)
	newRun := seedRunningRun(t, pool, "runner-mixed", "mixed", code)
	oldRun := seedRunningRun(t, pool, "runner-mixed", "mixed", "")

	postRunLog(t, ts, token, newRun, "new-era output\n")
	postRunLog(t, ts, token, oldRun, "old-era output\n")

	if _, err := os.Stat(filepath.Join(logDir, code, newRun+".log")); err != nil {
		t.Errorf("post-710 run is not in its folder: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, oldRun+".log")); err != nil {
		t.Errorf("pre-710 run is not at the flat path: %v", err)
	}

	client, _ := devLoginWithCSRF(t, ts)
	for _, tc := range []struct{ trace, want string }{
		{newRun, "new-era output"},
		{oldRun, "old-era output"},
	} {
		resp, err := client.Get(ts.URL + "/api/v1/runs/" + tc.trace + "/log")
		if err != nil {
			t.Fatalf("GET %s log: %v", tc.trace, err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(got), tc.want) {
			t.Errorf("GET %s log = %q, want %q — the two layouts do not coexist", tc.trace, got, tc.want)
		}
	}
}

// TestFirstLogInAFolderWritesTheMetaSidecar covers the on-disk explanation.
// `logs/a3f2c1d0/` tells a human nothing, and since a deleted entity's folder is
// left in place there is no naming convention distinguishing live from dead. The
// sidecar is the only answer available from the log tree alone — which is the
// case that matters, because that tree gets archived and mounted where the
// database is not. It must appear alongside the FIRST log, not on some later
// sweep that may never run.
func TestFirstLogInAFolderWritesTheMetaSidecar(t *testing.T) {
	logDir := t.TempDir()
	ts, pool := newTestServerWithLogDir(t, logDir)
	ctx := context.Background()

	code, err := entitycode.Allocate(ctx, pool, entitycode.KindJob, "git", "sidecar-job", "uid-sidecar-job")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	const token = "crn_run_sidecar"
	seedIngestRunner(t, pool, "runner-sidecar", token)
	traceID := seedRunningRun(t, pool, "runner-sidecar", "sidecar-job", code)

	postRunLog(t, ts, token, traceID, "output\n")

	metaPath := filepath.Join(logDir, code, runner.MetaFileName)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("no %s beside the first log in %s: %v", runner.MetaFileName, code, err)
	}
	var ent entitycode.Entity
	if err := json.Unmarshal(raw, &ent); err != nil {
		t.Fatalf("parse sidecar (%s): %v", raw, err)
	}
	if ent.Code != code || ent.Kind != entitycode.KindJob || ent.Source != "git" || ent.Name != "sidecar-job" {
		t.Errorf("sidecar = %+v, want code=%s kind=job source=git name=sidecar-job", ent, code)
	}
	// It is deliberately not named "*.log": the retention reaper only removes
	// *.log, so a folder's explanation outlives the logs it explains.
	if strings.HasSuffix(runner.MetaFileName, ".log") {
		t.Errorf("the sidecar is named %q — the run-log reaper would delete it", runner.MetaFileName)
	}
}

// TestExplicitJobDeleteMintsAFreshCodeOnRecreate is the counterpart to the sync
// prune test. An operator who deletes a job and later creates one with the same
// name created a NEW entity (LU-Q6(b)); it must not inherit the old one's log
// folder, which is still sitting on disk full of the previous job's output.
// AUTOINCREMENT on the registry's primary key is what guarantees the fresh code
// can never be a recycled one.
func TestExplicitJobDeleteMintsAFreshCodeOnRecreate(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	if _, err := pool.ExecContext(ctx, `
		INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
		VALUES('body','bash','echo hi','ssh','sha256:aaa','scripts/body.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)

	createJob := func(name string) int64 {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"name": name, "scriptRef": "body", "scope": ""})
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/jobs", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /jobs: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST /jobs = %d, want 201; body: %s", resp.StatusCode, body)
		}
		var created struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
			t.Fatalf("decode created job: %v", err)
		}
		return created.ID
	}

	id := createJob("recycled")
	uidOf := func() string {
		var u string
		_ = pool.QueryRowContext(ctx, `SELECT uid FROM jobs WHERE source='amadeus' AND name='recycled'`).Scan(&u)
		return u
	}
	first, err := entitycode.Lookup(ctx, pool, entitycode.KindJob, uidOf())
	if err != nil || first == "" {
		t.Fatalf("job create allocated no entity code (%q, %v)", first, err)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/jobs/"+strconv.FormatInt(id, 10), nil)
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("DELETE /jobs/%d: %v", id, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE = %d, want 204; body: %s", resp.StatusCode, body)
	}

	// RH moved WHEN the code retires. A delete now bins the definition, which has
	// not gone anywhere and must keep its folder — so the tuple still resolves,
	// and the name is still taken. The purge is what retires it.
	if got, err := entitycode.Lookup(ctx, pool, entitycode.KindJob, uidOf()); err != nil || got != first {
		t.Fatalf("a binned job's code = %q (%v), want %q — a recoverable definition keeps its folder", got, err, first)
	}
	purgeReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/v1/recycle-bin/job/recycled", nil)
	purgeReq.Header.Set("X-CSRF-Token", csrf)
	purgeResp, err := client.Do(purgeReq)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	purgeResp.Body.Close()
	if purgeResp.StatusCode != http.StatusNoContent {
		t.Fatalf("purge = %d, want 204", purgeResp.StatusCode)
	}

	// The purge stamped the row, so nothing live resolves for the tuple — and the
	// historical row survives so the folder left on disk stays attributable.
	if got, err := entitycode.Lookup(ctx, pool, entitycode.KindJob, uidOf()); err != nil || got != "" {
		t.Fatalf("after the purge the tuple still resolves to %q (%v) — a recreated job would inherit the folder", got, err)
	}
	if ent, err := entitycode.Describe(ctx, pool, first); err != nil || ent == nil || ent.DeletedAt == nil {
		t.Errorf("the deleted era's registry row is gone or unstamped (%+v, %v) — its log folder becomes unattributable", ent, err)
	}

	createJob("recycled")
	second, err := entitycode.Lookup(ctx, pool, entitycode.KindJob, uidOf())
	if err != nil || second == "" {
		t.Fatalf("recreate allocated no entity code (%q, %v)", second, err)
	}
	if second == first {
		t.Errorf("recreated job reused code %q — its runs would land in the deleted job's log folder", first)
	}
}
