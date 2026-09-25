package api_test

import (
	"context"
	"testing"
)

// TestProvenanceColumnsExposed pins V1.1-17 Phase A: the four definition-list
// endpoints (jobs, workflows, scripts, schedule-defs) each surface readOnly
// `createdAt` + `lastModifiedAt` timestamps. cronomicon-source rows carry the real
// S4 provenance dates; git-source rows (and all scripts, whose table has no
// provenance columns until the V2 migration) serialize them as null.
//
// This mirrors the assertion pattern of settings_test.go:883-884 (which pins the
// scope provenance fields) but lives in the internal/api package, where the four
// handlers are defined.
func TestProvenanceColumnsExposed(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	const (
		createdAt  = "2026-01-02T03:04:05Z"
		modifiedAt = "2026-02-03T04:05:06Z"
	)

	// ── Jobs: one git (no provenance) + one cronomicon (with provenance). ──────────
	seed(`INSERT INTO jobs(name, source, run_type, command, content_hash, synced_at)
	      VALUES('git-job','git','bash','echo git','sha256:aaa','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO jobs(name, source, run_type, command, created_at, last_modified_at)
	      VALUES('cronomicon-job','cronomicon','bash','echo cronomicon',?,?)`, createdAt, modifiedAt)

	// ── Workflows: one git + one amadeus. ──────────────────────────────────────
	seed(`INSERT INTO workflows(name, source, steps, synced_at)
	      VALUES('git-wf','git','[]','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO workflows(name, source, steps, created_at, last_modified_at)
	      VALUES('cronomicon-wf','cronomicon','[]',?,?)`, createdAt, modifiedAt)

	// ── Schedules: one git + one amadeus. ──────────────────────────────────────
	seed(`INSERT INTO schedules(name, source, cron, content_hash, source_path, synced_at)
	      VALUES('git-sched','git','0 0 2 * * *','sha256:bbb','schedules/git-sched.yaml','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO schedules(name, source, cron, content_hash, created_at, last_modified_at)
	      VALUES('cronomicon-sched','cronomicon','0 0 3 * * *','sha256:ccc',?,?)`, createdAt, modifiedAt)

	// ── Scripts: git-only catalog; the table has no provenance columns in Phase A
	// so createdAt/lastModifiedAt are always null. ──────────────────────────────
	seed(`INSERT INTO scripts(name, run_type, command, content_hash, source_path, synced_at)
	      VALUES('git-script','bash','echo hi','sha256:ddd','scripts/git-script.yaml','2026-01-01T00:00:00Z')`)

	client := devLoginClient(t, ts)

	// provRow captures just the two provenance fields keyed by name. The pointers
	// distinguish "key present and null" (nil) from "key present and populated".
	type provRow struct {
		Name           string  `json:"name"`
		CreatedAt      *string `json:"createdAt"`
		LastModifiedAt *string `json:"lastModifiedAt"`
	}
	type listResp struct {
		Items []provRow `json:"items"`
	}

	// assertProv looks up `name` in the list and checks its provenance fields.
	// wantSet=false asserts both are null (git/script rows); wantSet=true asserts
	// both equal the seeded cronomicon dates.
	assertProv := func(t *testing.T, label string, items []provRow, name string, wantSet bool) {
		t.Helper()
		var row *provRow
		for i := range items {
			if items[i].Name == name {
				row = &items[i]
				break
			}
		}
		if row == nil {
			t.Fatalf("%s: row %q not found in list", label, name)
		}
		if wantSet {
			if row.CreatedAt == nil || *row.CreatedAt != createdAt {
				t.Errorf("%s %q createdAt = %v, want %q", label, name, row.CreatedAt, createdAt)
			}
			if row.LastModifiedAt == nil || *row.LastModifiedAt != modifiedAt {
				t.Errorf("%s %q lastModifiedAt = %v, want %q", label, name, row.LastModifiedAt, modifiedAt)
			}
		} else {
			if row.CreatedAt != nil {
				t.Errorf("%s %q createdAt = %v, want null", label, name, *row.CreatedAt)
			}
			if row.LastModifiedAt != nil {
				t.Errorf("%s %q lastModifiedAt = %v, want null", label, name, *row.LastModifiedAt)
			}
		}
	}

	cases := []struct {
		label  string
		url    string
		gitRow string
		amaRow string // empty ⇒ no cronomicon row to assert (scripts)
	}{
		{"jobs", ts.URL + "/api/v1/jobs", "git-job", "cronomicon-job"},
		{"workflows", ts.URL + "/api/v1/workflows", "git-wf", "cronomicon-wf"},
		// schedule-defs defaults to the git source filter; assert the two
		// sources separately so both rows are reachable.
		{"schedule-defs(git)", ts.URL + "/api/v1/schedule-defs?source=git", "git-sched", ""},
		{"schedule-defs(cronomicon)", ts.URL + "/api/v1/schedule-defs?source=cronomicon", "", "cronomicon-sched"},
		{"scripts", ts.URL + "/api/v1/scripts", "git-script", ""},
	}

	for _, c := range cases {
		var list listResp
		getJSON(t, client, c.url, &list)
		if c.gitRow != "" {
			assertProv(t, c.label, list.Items, c.gitRow, false)
		}
		if c.amaRow != "" {
			assertProv(t, c.label, list.Items, c.amaRow, true)
		}
	}

	// ── Detail endpoints carry the same fields. ────────────────────────────────
	// cronomicon job detail (looked up by rowid) returns populated provenance.
	var amaJobID int64
	if err := pool.QueryRowContext(ctx,
		`SELECT rowid FROM jobs WHERE name='cronomicon-job'`).Scan(&amaJobID); err != nil {
		t.Fatalf("lookup cronomicon-job rowid: %v", err)
	}
	var jobDetail provRow
	getJSON(t, client, ts.URL+"/api/v1/jobs/"+itoa(amaJobID), &jobDetail)
	if jobDetail.CreatedAt == nil || *jobDetail.CreatedAt != createdAt {
		t.Errorf("job detail createdAt = %v, want %q", jobDetail.CreatedAt, createdAt)
	}
	if jobDetail.LastModifiedAt == nil || *jobDetail.LastModifiedAt != modifiedAt {
		t.Errorf("job detail lastModifiedAt = %v, want %q", jobDetail.LastModifiedAt, modifiedAt)
	}

	// cronomicon workflow detail (by rowid) returns populated provenance.
	var amaWfID int64
	if err := pool.QueryRowContext(ctx,
		`SELECT rowid FROM workflows WHERE name='cronomicon-wf'`).Scan(&amaWfID); err != nil {
		t.Fatalf("lookup cronomicon-wf rowid: %v", err)
	}
	var wfDetail provRow
	getJSON(t, client, ts.URL+"/api/v1/workflows/"+itoa(amaWfID), &wfDetail)
	if wfDetail.CreatedAt == nil || *wfDetail.CreatedAt != createdAt {
		t.Errorf("workflow detail createdAt = %v, want %q", wfDetail.CreatedAt, createdAt)
	}
	if wfDetail.LastModifiedAt == nil || *wfDetail.LastModifiedAt != modifiedAt {
		t.Errorf("workflow detail lastModifiedAt = %v, want %q", wfDetail.LastModifiedAt, modifiedAt)
	}

	// cronomicon schedule-def detail returns populated provenance.
	var schedDetail provRow
	getJSON(t, client, ts.URL+"/api/v1/schedule-defs/cronomicon-sched?source=cronomicon", &schedDetail)
	if schedDetail.CreatedAt == nil || *schedDetail.CreatedAt != createdAt {
		t.Errorf("schedule detail createdAt = %v, want %q", schedDetail.CreatedAt, createdAt)
	}
	if schedDetail.LastModifiedAt == nil || *schedDetail.LastModifiedAt != modifiedAt {
		t.Errorf("schedule detail lastModifiedAt = %v, want %q", schedDetail.LastModifiedAt, modifiedAt)
	}

	// script detail: the key is present and null in Phase A.
	var scriptDetail provRow
	getJSON(t, client, ts.URL+"/api/v1/scripts/git-script", &scriptDetail)
	if scriptDetail.CreatedAt != nil {
		t.Errorf("script detail createdAt = %v, want null", *scriptDetail.CreatedAt)
	}
	if scriptDetail.LastModifiedAt != nil {
		t.Errorf("script detail lastModifiedAt = %v, want null", *scriptDetail.LastModifiedAt)
	}
}
