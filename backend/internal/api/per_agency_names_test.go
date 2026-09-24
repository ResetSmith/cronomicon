package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// R2-5 (the rbac2 plan) — per-agency name uniqueness, end to end.
//
// The invariant: within a source pool a definition's name must be unique
// within EACH agency its scope maps to, and an All-pool definition collides
// with everything. These drive the real compose endpoints because the check
// lives in the write paths, not the schema — a unit test of the checker would
// pass against a handler that forgot to call it.
func TestPerAgencyJobNames(t *testing.T) {
	ts, pool := newTestServer(t)
	ctx := context.Background()
	client, csrf := devLoginWithCSRF(t, ts)

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Two agencies, two disjoint scopes, one shared script.
	seed(`INSERT INTO agencies(id, name, created_at) VALUES('ag-fin','FIN','t'),('ag-dss','DSS','t')`)
	seed(`INSERT OR IGNORE INTO scopes(id, name, source, created_at) VALUES('sc-fin','fin-prod','amadeus','t'),('sc-dss','dss-prod','amadeus','t')`)
	seed(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc-fin','ag-fin'),('sc-dss','ag-dss')`)
	seed(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('backup-db','bash','pg_dump','ssh','sha256:aaa','scripts/backup-db.yaml','t')`)

	compose := func(name, scope string) (int, string) {
		t.Helper()
		body := `{"name":"` + name + `","scriptRef":"backup-db","scope":"` + scope + `"}`
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/jobs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("compose: %v", err)
		}
		defer resp.Body.Close()
		var buf strings.Builder
		_ = json.NewEncoder(&buf)
		b := make([]byte, 512)
		n, _ := resp.Body.Read(b)
		return resp.StatusCode, string(b[:n])
	}

	// THE HEADLINE: the same name in two different agencies, both accepted.
	if code, body := compose("backup-daily", "fin-prod"); code != http.StatusCreated {
		t.Fatalf("FIN backup-daily = %d (%s), want 201", code, body)
	}
	if code, body := compose("backup-daily", "dss-prod"); code != http.StatusCreated {
		t.Fatalf("DSS backup-daily = %d (%s), want 201 — per-agency naming is the point of this stage", code, body)
	}

	// Same agency: refused, and with the GENERIC wording (oracle hygiene).
	if code, body := compose("backup-daily", "fin-prod"); code != http.StatusConflict {
		t.Errorf("duplicate within FIN = %d, want 409", code)
	} else if !strings.Contains(body, "name already in use") || strings.Contains(body, "FIN") || strings.Contains(body, "DSS") {
		t.Errorf("refusal wording leaks or drifts: %s", body)
	}

	// The All pool overlaps everything: a global job of that name is refused…
	if code, _ := compose("backup-daily", ""); code != http.StatusConflict {
		t.Errorf("All-pool duplicate = %d, want 409", code)
	}
	// …and a scoped duplicate of an existing All-pool name is refused too.
	if code, _ := compose("global-sweep", ""); code != http.StatusCreated {
		t.Errorf("All-pool create = %d, want 201", code)
	}
	if code, _ := compose("global-sweep", "fin-prod"); code != http.StatusConflict {
		t.Errorf("scoped duplicate of an All-pool name = %d, want 409", code)
	}

	// Each twin carries its own identity, and both are intact in the catalog.
	var uids []string
	rows, err := pool.QueryContext(ctx, `SELECT uid FROM jobs WHERE name='backup-daily' ORDER BY scope`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var u string
		_ = rows.Scan(&u)
		uids = append(uids, u)
	}
	rows.Close()
	if len(uids) != 2 || uids[0] == uids[1] || uids[0] == "" {
		t.Errorf("twin uids = %v, want two distinct non-empty identities", uids)
	}
}
