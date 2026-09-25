package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// RA-24 at the TRIGGER boundary (the runas-update plan §11.1b).
//
// An unbound run carries an empty agency snapshot, so a department-owned row
// resolves for nobody. Left alone the run reaches dispatch and dies with a 409
// that reads as a permissions problem, or queues forever on a departmentalised
// fleet. The refusal moves that to the boundary, where the remedy — bind a scope —
// is one field away from the person reading it.
//
// ⚠️ Being UNRESTRICTED makes this MORE likely, not less: unrestricted is exactly
// what permits an unbound run in the first place (RB-26), so the ops team is the
// population most exposed. The fixture's admin is unrestricted for that reason.

func unboundRefServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	h, pool := secretRBACServer(t, nil) // admin unrestricted — the exposed persona
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	// An UNSCOPED job — the shape that produces an unbound run.
	exec(`INSERT INTO jobs (name, source, run_type, concurrency_policy, synced_at)
	      VALUES ('unscoped-job','git','bash','Allow','t')`)
	// One department-owned secret and one global one, so the test can tell the
	// precise rule from a blunt "has bindings ⇒ refuse".
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-owned','DEPT_PASSWORD','stored','ag-fin','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('s-owned','ag-fin')`)
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-global','SHARED_TOKEN','stored','','2026-01-01T00:00:00Z')`)
	return h, pool
}

// TestUnboundRunWithOwnedCredentialIsRefused — the headline. The job declares a
// binding to a department-owned row; run unbound, that row resolves for nobody.
func TestUnboundRunWithOwnedCredentialIsRefused(t *testing.T) {
	h, pool := unboundRefServer(t)
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	                        VALUES('job','git','unscoped-job','secret','DEPT_PASSWORD','t')`); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins", `{}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unbound run consuming an owned credential = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	msg, _ := body["message"].(string)
	for _, want := range []string{"DEPT_PASSWORD", "bind a scope"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q should name %q — the operator has to know which reference and what to do", msg, want)
		}
	}

	// Nothing was enqueued: a refusal after the INSERT is not a refusal.
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("refused trigger created %d runs, want 0", runs)
	}
}

// TestUnboundRunWithGlobalCredentialStillRuns is the precision half, and the more
// important of the two: a job binding only GLOBAL rows works unbound today and must
// keep working. The plan's original blunt rule ("declares bindings ⇒ refuse") would
// have broken exactly this, and a guard that over-refuses gets switched off.
func TestUnboundRunWithGlobalCredentialStillRuns(t *testing.T) {
	h, pool := unboundRefServer(t)
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	                        VALUES('job','git','unscoped-job','secret','SHARED_TOKEN','t')`); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins", `{}`)
	if rec.Code == http.StatusUnprocessableEntity {
		t.Errorf("unbound run consuming a GLOBAL credential = 422, want it allowed — "+
			"a global row resolves for everyone and this job works today (%s)", rec.Body.String())
	}
}

// TestBindingAScopeClearsTheRefusal — the message tells the operator to bind a
// scope, so binding one has to actually work. A refusal whose stated remedy does
// not resolve it is worse than no message.
func TestBindingAScopeClearsTheRefusal(t *testing.T) {
	h, pool := unboundRefServer(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Give Finance a scope, so a run bound to it carries Finance's agencies.
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc-fin','finance','cronomicon','t')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-fin','ag-fin')`)
	exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	      VALUES('job','git','unscoped-job','secret','DEPT_PASSWORD','t')`)
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins",
		`{"scope":"finance"}`)
	if rec.Code == http.StatusUnprocessableEntity {
		t.Errorf("run bound to the owning department = 422, want it allowed — "+
			"binding a scope is the remedy the refusal names (%s)", rec.Body.String())
	}
}
