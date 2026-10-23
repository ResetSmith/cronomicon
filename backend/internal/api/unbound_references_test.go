package api_test

import (
	"database/sql"
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
	// Finance's job with no scope: it runs against Finance's scopes (2.5.0 C2,
	// question 78: a job with no scope is still one agency's).
	exec(`INSERT INTO jobs (name, source, run_type, concurrency_policy, synced_at, owner_agency)
	      VALUES ('unscoped-job','git','bash','Allow','t','ag-fin')`)
	// One department-owned secret and one global one, so the test can tell the
	// precise rule from a blunt "has bindings ⇒ refuse".
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-owned','DEPT_PASSWORD','stored','ag-fin','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('s-owned','ag-fin')`)
	exec(`INSERT INTO secrets(id,key,source,owner_agency,created_at)
	      VALUES('s-global','SHARED_TOKEN','stored','','2026-01-01T00:00:00Z')`)
	return h, pool
}

// TestScopelessRunOfAnAgencysJobUsesItsOwnCredential — the headline until 2.5.3: the job
// declared a binding to a department-owned row, and run unbound (Global's
// snapshot) that row resolved for nobody, so the trigger refused it.
//
// Since 2.5.3 (CA-5) a run with no scope is its job's agency's: Finance's
// scopeless run is stamped Finance and resolves Finance's own credential, so it
// is not refused, and is enqueued as Finance's.
func TestScopelessRunOfAnAgencysJobUsesItsOwnCredential(t *testing.T) {
	h, pool := unboundRefServer(t)
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	                        VALUES('job','git','unscoped-job','secret','DEPT_PASSWORD','t')`); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("scopeless run of Finance's job consuming Finance's credential = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	var agencies string
	if err := pool.QueryRow(`SELECT COALESCE(agencies_json, '') FROM runs WHERE job_name = 'unscoped-job'`).Scan(&agencies); err != nil {
		t.Fatalf("exactly one run was expected: %v", err)
	}
	if agencies != `["Finance"]` {
		t.Errorf("the scopeless run's agencies = %s, want [\"Finance\"]: its job's agency's (CA-5)", agencies)
	}
}

// TestScopelessRunOfAnAgencysJobCannotUseCentralsCredential was the precision half: a job
// binding only GLOBAL rows worked unbound, because the unbound run was Global's,
// and a guard that over-refuses gets switched off.
//
// Since 2.5.3 (CA-5) Finance's scopeless run is Finance's, and a row of the
// built-in agency's is usable by its own runs only (§0a, IN-6): the credential
// would not resolve at dispatch, so the trigger refuses the run up front, naming
// it, and enqueues nothing.
func TestScopelessRunOfAnAgencysJobCannotUseCentralsCredential(t *testing.T) {
	h, pool := unboundRefServer(t)
	if _, err := pool.Exec(`INSERT INTO reference_bindings(owner_kind,owner_source,owner_name,ref_kind,ref_name,created_at)
	                        VALUES('job','git','unscoped-job','secret','SHARED_TOKEN','t')`); err != nil {
		t.Fatal(err)
	}
	var jobID int64
	_ = pool.QueryRow(`SELECT rowid FROM jobs WHERE name='unscoped-job'`).Scan(&jobID)

	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs/"+itoa(jobID)+"/run", "sec-admins", `{}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "SHARED_TOKEN") {
		t.Errorf("scopeless run of Finance's job consuming the built-in agency's credential = %d, want 422 naming SHARED_TOKEN (%s)",
			rec.Code, rec.Body.String())
	}
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("refused trigger created %d runs, want 0", runs)
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
