package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// R2-1 — /runs?jobUid= filters run history by the job's permanent identity, and
// the run rows expose it.
//
// The pair of assertions matters more than either alone: while names are still
// unique the uid filter must return EXACTLY what the name filter returns (that
// equivalence is what lets callers move over safely, one at a time), and where a
// name is already shared — same name in both source pools, legal today — the uid
// filter must return only its own job's runs while the name filter returns both.
// That second case is a live preview of what every name filter does once
// per-agency naming lands.
func TestRunsJobUIDFilter(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs(name, source, uid, run_type, concurrency_policy, synced_at)
	      VALUES('shared-name','git','uid-git','bash','Allow','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO jobs(name, source, uid, run_type, concurrency_policy, synced_at)
	      VALUES('shared-name','cronomicon','uid-ama','bash','Allow','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO jobs(name, source, uid, run_type, concurrency_policy, synced_at)
	      VALUES('solo','git','uid-solo','bash','Allow','2026-01-01T00:00:00Z')`)

	seed(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-git-1','shared-name','git','uid-git','bash','success','t','manual','2026-08-13T01:00:00Z')`)
	seed(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-ama-1','shared-name','cronomicon','uid-ama','bash','success','t','manual','2026-08-13T02:00:00Z')`)
	seed(`INSERT INTO runs(id, job_name, job_source, job_uid, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-solo-1','solo','git','uid-solo','bash','success','t','manual','2026-08-13T03:00:00Z')`)

	client := devLoginClient(t, ts)
	traceIDs := func(query string) []string {
		t.Helper()
		resp, err := client.Get(ts.URL + "/api/v1/runs?" + query)
		if err != nil {
			t.Fatalf("GET /runs?%s: %v", query, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /runs?%s = %d, want 200", query, resp.StatusCode)
		}
		var env struct {
			Items []struct {
				TraceID string `json:"traceId"`
				JobUID  string `json:"jobUid"`
				JobName string `json:"jobName"`
			} `json:"items"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("decode: %v", err)
		}
		ids := make([]string, 0, len(env.Items))
		for _, it := range env.Items {
			ids = append(ids, it.TraceID)
			// The uid must round-trip onto the wire, or no caller can migrate.
			if it.JobUID == "" {
				t.Errorf("run %s carries no jobUid in the API response", it.TraceID)
			}
		}
		return ids
	}

	// Equivalence while the name is unique: both filters, same single run.
	byName := traceIDs("job=solo")
	byUID := traceIDs("jobUid=uid-solo")
	if len(byName) != 1 || len(byUID) != 1 || byName[0] != byUID[0] {
		t.Errorf("name filter = %v, uid filter = %v; want the same single run", byName, byUID)
	}

	// Discrimination where the name is already shared.
	sharedByName := traceIDs("job=shared-name")
	if len(sharedByName) != 2 {
		t.Errorf("?job=shared-name returned %d runs, want 2 — a NAME filter answers for every job with that name", len(sharedByName))
	}
	sharedByUID := traceIDs("jobUid=uid-ama")
	if len(sharedByUID) != 1 || sharedByUID[0] != "run-ama-1" {
		t.Errorf("?jobUid=uid-ama returned %v, want just run-ama-1", sharedByUID)
	}
}
