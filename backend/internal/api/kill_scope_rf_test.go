package api_test

import (
	"database/sql"
	"net/http"
	"strconv"
	"testing"
)

// jobRowIDStr is jobRowID as the path segment the job routes take.
func jobRowIDStr(t *testing.T, pool *sql.DB, name string) string {
	t.Helper()
	return strconv.FormatInt(jobRowID(t, pool, name), 10)
}

// RF-10 — the run-effective-scope coverage for kill/pause/resume
// (the RBAC-fixes plan).
//
// Until now NO test in this repository issued POST /api/v1/jobs/{id}/kill at all,
// which left the most security-significant single change in v0.56.4 unfenced:
// killJob authorizes on the RUN's frozen scope rather than the job's. The two
// differ whenever a per-trigger override was used, and under RB-26 they ALWAYS
// differ for an unscoped job — its runs each carry the scope their triggerer
// chose while the job itself carries none.
//
// The pause/resume rows are the other half: v0.56.4 added a 403 for a caller who
// can SEE the job but lacks killJobs, and only the pre-existing out-of-scope 404
// was covered.

// seedRunFor inserts an active run for a job with an explicit frozen scope. The
// scope argument is written verbatim — "" is the unbound run (system-global, or a
// scheduled fire of an unscoped job under RB-Q11(c)).
func seedRunFor(t *testing.T, exec func(string, ...any), runID, jobName, scope string) {
	t.Helper()
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by,
	                        trigger_kind, executor, scope, created_at)
	      VALUES (?, ?, 'amadeus', 'bash', 'running', 't', 'manual', 'ssh', ?, '2026-01-01T00:00:00Z')`,
		runID, jobName, scope)
}

// TestKillAuthorizesOnTheRunsScopeNotTheJobs — the headline RF-10 case, and the
// reason killJob looks up the run BEFORE the guard.
//
// The job is UNSCOPED. Two runs of it exist, bound at trigger time to different
// departments (RB-26). Checking jobs.scope would make both runs killable by
// anyone (empty ⇒ allowed under the old Q-F7 rule) or by no one; checking the
// RUN's scope makes each killable by exactly the department that launched it.
func TestKillAuthorizesOnTheRunsScopeNotTheJobs(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at)
	      VALUES ('s-fin','finance','amadeus','2026-01-01T00:00:00Z')`)
	// The template job (§2.5): no scope of its own.
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('restart-service','amadeus','bash',NULL,1)`)
	killPath := "/api/v1/jobs/" + jobRowIDStr(t, pool, "restart-service") + "/kill"

	// A run bound to FINANCE. The prod operator holds killJobs — but not there.
	seedRunFor(t, exec, "r-fin", "restart-service", "finance")
	if rec := reqAs(t, h, http.MethodPost, killPath, "sec-operators", ""); rec.Code != http.StatusForbidden {
		t.Errorf("prod operator killing a run bound to FINANCE = %d, want 403 — the guard "+
			"must read the RUN's scope, not the job's empty one", rec.Code)
	}
	// Still running: the denial was not cosmetic.
	var status string
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='r-fin'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "running" {
		t.Errorf("Finance run status after the refused kill = %q, want running", status)
	}

	// Same job, same caller, a run bound to their OWN department: allowed.
	exec(`DELETE FROM runs WHERE id='r-fin'`)
	seedRunFor(t, exec, "r-prod", "restart-service", "prod")
	if rec := reqAs(t, h, http.MethodPost, killPath, "sec-operators", ""); rec.Code != http.StatusAccepted &&
		rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("prod operator killing a run bound to PROD = %d, want success (%s)", rec.Code, rec.Body.String())
	}
	if err := pool.QueryRow(`SELECT status FROM runs WHERE id='r-prod'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "killed" {
		t.Errorf("own-department run status after kill = %q, want killed", status)
	}
}

// TestKillOfAnUnboundRunIsUnrestrictedOnly — the RB-Q11(c) side of the kill guard.
// A run with no scope at all is system-global (or a scheduled fire of an unscoped
// job), and stopping it is unrestricted-only, mirroring the trigger side.
func TestKillOfAnUnboundRunIsUnrestrictedOnly(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('nightly-backup','amadeus','bash',NULL,1)`)
	killPath := "/api/v1/jobs/" + jobRowIDStr(t, pool, "nightly-backup") + "/kill"
	seedRunFor(t, exec, "r-unbound", "nightly-backup", "")

	if rec := reqAs(t, h, http.MethodPost, killPath, "sec-operators", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted operator killing an UNBOUND run = %d, want 403", rec.Code)
	}
	// An unrestricted admin may. (Fresh server: the run above is untouched.)
	h2, pool2 := secretRBACServer(t, nil)
	exec2 := func(q string, args ...any) {
		t.Helper()
		if _, err := pool2.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec2(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	       VALUES ('nightly-backup','amadeus','bash',NULL,1)`)
	seedRunFor(t, exec2, "r-unbound", "nightly-backup", "")
	killPath2 := "/api/v1/jobs/" + jobRowIDStr(t, pool2, "nightly-backup") + "/kill"
	if rec := reqAs(t, h2, http.MethodPost, killPath2, "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("unrestricted admin killing an unbound run = 403, want it allowed (%s)", rec.Body.String())
	}
}

// TestKillRequiresTheVerbNotJustTheScope — FR-M1 on the kill route. A viewer with
// full scope access held execute until v0.56.4; this is the regression that says
// so.
func TestKillRequiresTheVerbNotJustTheScope(t *testing.T) {
	h, pool := secretRBACServer(t, nil) // viewer is UNRESTRICTED here
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('j-prod','amadeus','bash','prod',1)`)
	seedRunFor(t, exec, "r1", "j-prod", "prod")
	killPath := "/api/v1/jobs/" + jobRowIDStr(t, pool, "j-prod") + "/kill"

	// Unrestricted VIEWER: reaches every scope, holds no verb at all. Scope reach
	// is not authority — the distinction v0.56.4 exists to draw.
	if rec := reqAs(t, h, http.MethodPost, killPath, "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("unrestricted viewer killing an in-scope run = %d, want 403 (FR-M1)", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPost, killPath, "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("admin killing the same run = 403, want it allowed (%s)", rec.Body.String())
	}
}

// TestPauseResumeRequireKillJobsForAnInScopeViewer — the half of v0.56.4's
// pause/resume change that had no coverage. The pre-existing out-of-scope 404 is
// covered by TestIDORScopeGates; this is the NEW 403, for a caller who can
// already see the job and simply lacks the verb.
func TestPauseResumeRequireKillJobsForAnInScopeViewer(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs (name, source, run_type, scope, enabled)
	      VALUES ('j-prod','amadeus','bash','prod',1)`)
	base := "/api/v1/jobs/" + jobRowIDStr(t, pool, "j-prod")

	for _, verb := range []string{"/pause", "/resume"} {
		// 403, NOT 404: the viewer can see this job (unrestricted reach), so hiding
		// it would be dishonest — they are told plainly they lack the verb.
		rec := reqAs(t, h, http.MethodPost, base+verb, "sec-viewers", "")
		if rec.Code != http.StatusForbidden {
			t.Errorf("viewer POST %s on an in-scope job = %d, want 403", verb, rec.Code)
		}
		if rec := reqAs(t, h, http.MethodPost, base+verb, "sec-admins", ""); rec.Code == http.StatusForbidden {
			t.Errorf("admin POST %s = 403, want it allowed (%s)", verb, rec.Body.String())
		}
	}
}
