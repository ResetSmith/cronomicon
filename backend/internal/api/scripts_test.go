package api_test

import (
	"context"
	"net/http"
	"strconv"
	"testing"
)

// TestScriptsCatalog exercises the read-only Scripts API (B-Git, Phase 2): the
// paginated list with usedByCount, the detail endpoint's usedBy reverse index,
// the type filter, a 404, and that a referencing job echoes scriptRef.
func TestScriptsCatalog(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// Two scripts: backup-db (referenced by 2 jobs) + orphan (referenced by none).
	seed(`INSERT INTO scripts(name, description, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','Dump pg','bash','pg_dump mydb','ssh','sha256:aaa','scripts/backup-db.yaml','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO scripts(name, run_type, script, content_hash, synced_at)
	      VALUES('orphan','ansible','- hosts: all','sha256:bbb','2026-01-01T00:00:00Z')`)

	// Two jobs reference backup-db; their executable fields are denormalized from it.
	for _, jn := range []string{"nightly-backup", "weekly-backup"} {
		seed(`INSERT INTO jobs(name, run_type, command, executor, script_ref, content_hash, synced_at)
		      VALUES(?,'bash','pg_dump mydb','ssh','backup-db','sha256:aaa','2026-01-01T00:00:00Z')`, jn)
	}

	client := devLoginClient(t, ts)

	// ── List ────────────────────────────────────────────────────────────────────
	var list struct {
		TotalItems int `json:"totalItems"`
		Items      []struct {
			Name        string `json:"name"`
			RunType     string `json:"runType"`
			ContentHash string `json:"contentHash"`
			UsedByCount int    `json:"usedByCount"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/scripts", &list)
	if list.TotalItems != 2 {
		t.Errorf("totalItems = %d, want 2", list.TotalItems)
	}
	usedBy := map[string]int{}
	for _, it := range list.Items {
		usedBy[it.Name] = it.UsedByCount
		if it.ContentHash == "" {
			t.Errorf("script %q missing contentHash", it.Name)
		}
	}
	if usedBy["backup-db"] != 2 {
		t.Errorf("backup-db usedByCount = %d, want 2", usedBy["backup-db"])
	}
	if usedBy["orphan"] != 0 {
		t.Errorf("orphan usedByCount = %d, want 0", usedBy["orphan"])
	}

	// ── type filter ───────────────────────────────────────────────────────────
	var filtered struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	getJSON(t, client, ts.URL+"/api/v1/scripts?type=ansible", &filtered)
	if len(filtered.Items) != 1 || filtered.Items[0].Name != "orphan" {
		t.Errorf("type=ansible filter = %+v, want [orphan]", filtered.Items)
	}

	// ── Detail: usedBy reverse index ──────────────────────────────────────────
	var detail struct {
		Name        string   `json:"name"`
		RunType     string   `json:"runType"`
		Command     *string  `json:"command"`
		ContentHash string   `json:"contentHash"`
		UsedBy      []string `json:"usedBy"`
		UsedByCount int      `json:"usedByCount"`
	}
	getJSON(t, client, ts.URL+"/api/v1/scripts/backup-db", &detail)
	if detail.Command == nil || *detail.Command != "pg_dump mydb" {
		t.Errorf("detail command = %v, want pg_dump mydb", detail.Command)
	}
	if detail.UsedByCount != 2 {
		t.Errorf("detail usedByCount = %d, want 2", detail.UsedByCount)
	}
	if len(detail.UsedBy) != 2 || detail.UsedBy[0] != "nightly-backup" || detail.UsedBy[1] != "weekly-backup" {
		t.Errorf("usedBy = %v, want [nightly-backup weekly-backup] (sorted)", detail.UsedBy)
	}

	// ── 404 ─────────────────────────────────────────────────────────────────────
	resp, err := client.Get(ts.URL + "/api/v1/scripts/does-not-exist")
	if err != nil {
		t.Fatalf("GET missing: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET missing script = %d, want 404", resp.StatusCode)
	}

	// ── Job detail echoes scriptRef ───────────────────────────────────────────
	var rowid int64
	if err := pool.QueryRowContext(ctx, `SELECT rowid FROM jobs WHERE name='nightly-backup'`).Scan(&rowid); err != nil {
		t.Fatalf("lookup rowid: %v", err)
	}
	var job struct {
		ScriptRef *string `json:"scriptRef"`
	}
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+strconv.FormatInt(rowid, 10), &job)
	if job.ScriptRef == nil || *job.ScriptRef != "backup-db" {
		t.Errorf("job.scriptRef = %v, want backup-db", job.ScriptRef)
	}
}
