package api_test

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// R2-2 — the compose and pause paths stamp the owner's uid on the satellite
// rows they write, and purging a definition takes those rows with it.
//
// End-to-end through HTTP rather than against the SQL directly, because the
// thing being verified is that the REAL write path stamps the uid — a unit test
// of the statement would pass just as happily against a writer nobody calls.
func TestComposeAndPauseStampSatelliteUIDs(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	if _, err := pool.ExecContext(ctx, `INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}

	body := `{
		"name": "uid-sat-job",
		"scriptRef": "backup-db",
		"scope": "",
		"schedules": [{"name": "nightly", "cron": "0 2 * * *"}]
	}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("compose job = %d, want 2xx", resp.StatusCode)
	}

	var jobUID string
	if err := pool.QueryRowContext(ctx,
		`SELECT uid FROM jobs WHERE name='uid-sat-job' AND source='amadeus'`).Scan(&jobUID); err != nil {
		t.Fatalf("read job uid: %v", err)
	}
	if jobUID == "" {
		t.Fatal("composed job carries no uid — AF-4a's writer regressed")
	}

	var schedUID sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT owner_uid FROM definition_schedules WHERE owner_name='uid-sat-job'`).Scan(&schedUID); err != nil {
		t.Fatalf("read schedule owner_uid: %v", err)
	}
	if schedUID.String != jobUID {
		t.Errorf("definition_schedules.owner_uid = %v, want the job's uid %q", schedUID, jobUID)
	}

	// The revision snapshot the compose wrote must carry it too.
	var revUID sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT uid FROM definition_revisions WHERE name='uid-sat-job' ORDER BY revision_no DESC LIMIT 1`).Scan(&revUID); err != nil {
		t.Fatalf("read revision uid: %v", err)
	}
	if revUID.String != jobUID {
		t.Errorf("definition_revisions.uid = %v, want %q", revUID, jobUID)
	}

	// Pause it through the API and check the pause row.
	var jobID int64
	if err := pool.QueryRowContext(ctx,
		`SELECT rowid FROM jobs WHERE name='uid-sat-job' AND source='amadeus'`).Scan(&jobID); err != nil {
		t.Fatalf("read job rowid: %v", err)
	}
	pauseReq, _ := http.NewRequest("POST",
		ts.URL+"/api/v1/jobs/"+strconv.FormatInt(jobID, 10)+"/pause", strings.NewReader("{}"))
	pauseReq.Header.Set("Content-Type", "application/json")
	pauseReq.Header.Set("X-CSRF-Token", csrf)
	pauseResp, err := client.Do(pauseReq)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	pauseResp.Body.Close()
	if pauseResp.StatusCode != http.StatusOK && pauseResp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause = %d, want 2xx", pauseResp.StatusCode)
	}
	var pauseUID sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT owner_uid FROM paused_jobs WHERE name='uid-sat-job'`).Scan(&pauseUID); err != nil {
		t.Fatalf("read pause owner_uid: %v", err)
	}
	if pauseUID.String != jobUID {
		t.Errorf("paused_jobs.owner_uid = %v, want %q", pauseUID, jobUID)
	}
}
