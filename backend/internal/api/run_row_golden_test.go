package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/rowgolden"
)

// RR-1/RR-2 — byte-identity golden for the sixth `runs` writer, the one the
// original survey missed: the SSH "Test connection" probe mirrored into History
// as a terminal kind='ssh-test' row (V1.1-2). Companion to the goldens in
// internal/scheduler and internal/workflow; same contract. Cut against the
// hand-rolled INSERT before RR-2 converted it to scheduler.InsertRun, so an
// unchanged golden proves the conversion.
//
// duration_ms is the probe's measured latency and is masked like a timestamp.
//
// Regenerate with: ROWGOLDEN_UPDATE=1 go test ./internal/api -run RunRowGolden

func TestRunRowGolden_W6_SSHTestRun(t *testing.T) {
	ts, pool := newTestServerWithLogDir(t, t.TempDir())
	client, csrf := devLogin(t, ts.URL)

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := pool.Exec(`
		INSERT INTO ssh_hosts(id, hostname, address, port, username, auth_key_env_var,
		                      created_by, created_at, last_modified_by, last_modified_at)
		VALUES('hg1','goldenhost','10.0.0.9',22,'tester','MISSING_KEY','tester',?,'tester',?)`, now, now); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/ssh/hosts/hg1/test", nil)
	req.Header.Set("X-CSRF-Token", csrf)
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /ssh/hosts/hg1/test = %d, want 200", r.StatusCode)
	}

	row := rowgolden.Snapshot(t, pool, "runs", "kind = 'ssh-test' AND target_host = ?", "goldenhost")
	rowgolden.Compare(t, "runrow_w6_ssh_test_run", rowgolden.Normalize(row, map[string]string{
		"id": "<id>", "created_at": "<ts>", "started_at": "<ts>", "completed_at": "<ts>",
		"duration_ms": "<ms>",
	}))
}
