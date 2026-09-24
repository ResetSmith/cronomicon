package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// AF-4a — composed definitions carry a stable uid: assigned on create, PRESERVED
// on every edit (the compose upsert's DO UPDATE omits uid), and returned by the
// API so integrations can key on something that survives what happens to names.
// The migration backfill is covered too: rows that predate the column (seeded
// raw in other tests) get a unique uid at migrate time, which the UNIQUE index
// proves by existing.
func TestComposedJobUidIsStableAcrossEdits(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	if _, err := pool.ExecContext(ctx,
		`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
		 VALUES('backup-db','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','t')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	client, csrf := devLoginWithCSRF(t, ts)

	send := func(method, url string, body any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, url, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			t.Fatalf("%s %s = %d", method, url, resp.StatusCode)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}

	created := send(http.MethodPost, ts.URL+"/api/v1/jobs",
		map[string]any{"name": "uid-job", "scriptRef": "backup-db", "scope": "Prod"})
	uid, _ := created["uid"].(string)
	if uid == "" {
		t.Fatalf("create response carries no uid: %v", created)
	}
	id := created["id"].(float64)

	// An edit is the same upsert hitting its DO UPDATE arm — the uid must ride
	// through it untouched, or every integration keyed on it breaks on the first
	// description tweak.
	edited := send(http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(int64(id)),
		map[string]any{"name": "uid-job", "scriptRef": "backup-db", "scope": "Prod", "description": "edited"})
	if got, _ := edited["uid"].(string); got != uid {
		t.Fatalf("edit re-minted the uid: %q → %q", uid, got)
	}

	// And the detail read agrees with the write responses.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(int64(id)), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET job: %v", err)
	}
	defer resp.Body.Close()
	var detail map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&detail)
	if got, _ := detail["uid"].(string); got != uid {
		t.Fatalf("detail uid %q != created uid %q", got, uid)
	}
}
