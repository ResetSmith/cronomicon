package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// R2F-3 — the backend half: every surface that prints a definition's NAME must
// also send the identity and agencies a client needs to qualify it.
//
// The badge itself is a frontend rule (utils/disambiguate.ts), and it is keyed
// on exactly these two fields. If a projection drops either, the rule silently
// degrades to "never badge" — the surface keeps rendering, looks fine, and shows
// two departments' definitions under one indistinguishable name. So these assert
// the DATA reaches the client, one test per data-shape class.

// Class 1: a run-row surface. The activity feed carries its own uid columns
// (R2-1) and derives agencies from the row's scope.
func TestActivityProjectsIdentityAndAgencies(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','FIN','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-fin','fin-prod','cronomicon','t')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-fin','ag-fin')`)
	exec(`INSERT INTO activity (kind, actor, job_name, job_uid, scope, at, created_at)
	      VALUES ('run-end','ops@x','deploy','uid-deploy-fin','fin-prod','2026-08-14T00:00:00Z','2026-08-14T00:00:00Z')`)
	// A pre-backfill row: the 1010 migration could not attribute it, so it has no
	// uid. It must still be SERVED — it simply never badges.
	exec(`INSERT INTO activity (kind, actor, job_name, scope, at, created_at)
	      VALUES ('run-end','ops@x','legacy','fin-prod','2026-08-14T00:00:01Z','2026-08-14T00:00:01Z')`)

	rec := reqAs(t, h, http.MethodGet, "/api/v1/activity", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /activity = %d (%s)", rec.Code, rec.Body.String())
	}
	var page struct {
		Items []struct {
			JobName  string   `json:"jobName"`
			JobUID   string   `json:"jobUid"`
			Agencies []string `json:"agencies"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var seenDeploy, seenLegacy bool
	for _, it := range page.Items {
		switch it.JobName {
		case "deploy":
			seenDeploy = true
			if it.JobUID != "uid-deploy-fin" {
				t.Errorf("activity jobUid = %q, want uid-deploy-fin — without it the feed cannot tell two same-named jobs apart", it.JobUID)
			}
			if len(it.Agencies) != 1 || it.Agencies[0] != "FIN" {
				t.Errorf("activity agencies = %v, want [FIN] — the badge has nothing to say without them", it.Agencies)
			}
		case "legacy":
			seenLegacy = true
			if it.JobUID != "" {
				t.Errorf("unattributable row invented a uid: %q", it.JobUID)
			}
		}
	}
	if !seenDeploy || !seenLegacy {
		t.Fatalf("activity did not return both seeded rows (deploy=%v legacy=%v)", seenDeploy, seenLegacy)
	}
}

// Class 2: a projection-fed list. The schedule reverse index names its owners,
// and gains their identity + derived agencies additively — `name` and `kind` are
// untouched, because they are what every existing client reads.
func TestScheduleUsedByProjectsOwnerIdentity(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','FIN','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-dss','DSS','2026-01-01T00:00:00Z')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-fin','fin-prod','cronomicon','t')`)
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES ('sc-dss','dss-prod','cronomicon','t')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-fin','ag-fin')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-dss','ag-dss')`)
	// Two departments' jobs of ONE name — the state that makes this necessary.
	exec(`INSERT INTO jobs (uid,name,source,run_type,scope,enabled) VALUES ('uid-fin','nightly','cronomicon','bash','fin-prod',1)`)
	exec(`INSERT INTO jobs (uid,name,source,run_type,scope,enabled) VALUES ('uid-dss','nightly','git','bash','dss-prod',1)`)
	exec(`INSERT INTO schedules (name,source,cron,content_hash,synced_at) VALUES ('overnight','git','0 2 * * *','sha256:x','t')`)
	exec(`INSERT INTO definition_schedules (owner_source,owner_kind,owner_name,owner_uid,name,cron,source_ref)
	      VALUES ('cronomicon','job','nightly','uid-fin','overnight','0 2 * * *','overnight')`)
	exec(`INSERT INTO definition_schedules (owner_source,owner_kind,owner_name,owner_uid,name,cron,source_ref)
	      VALUES ('git','job','nightly','uid-dss','overnight','0 2 * * *','overnight')`)

	rec := reqAs(t, h, http.MethodGet, "/api/v1/schedule-defs/overnight", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /schedule-defs/overnight = %d (%s)", rec.Code, rec.Body.String())
	}
	var sd struct {
		UsedBy []struct {
			Name     string   `json:"name"`
			Kind     string   `json:"kind"`
			UID      string   `json:"uid"`
			Agencies []string `json:"agencies"`
		} `json:"usedBy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sd); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sd.UsedBy) != 2 {
		t.Fatalf("usedBy = %d entries, want 2 (both same-named owners)", len(sd.UsedBy))
	}
	byUID := map[string][]string{}
	for _, u := range sd.UsedBy {
		if u.Name != "nightly" || u.Kind != "job" {
			t.Errorf("existing fields changed shape: %+v — name/kind must stay exactly what they were", u)
		}
		byUID[u.UID] = u.Agencies
	}
	// Two entries, one name, and the ONLY thing telling them apart is what this
	// band added. Without it the operator sees "job: nightly" twice.
	if got := byUID["uid-fin"]; len(got) != 1 || got[0] != "FIN" {
		t.Errorf("uid-fin agencies = %v, want [FIN]", got)
	}
	if got := byUID["uid-dss"]; len(got) != 1 || got[0] != "DSS" {
		t.Errorf("uid-dss agencies = %v, want [DSS]", got)
	}
}
