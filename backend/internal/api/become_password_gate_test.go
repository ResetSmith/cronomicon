package api_test

import (
	"database/sql"
	"net/http"
	"testing"
)

// RA-21 — the authorization gate on the Composer's `becomePasswordSecret` field
// (the runas-update plan §11.1, §14.1).
//
// v0.57.3 shipped this field with NO check of any kind — no verb, no agency, no
// run type, no existence — while its OpenAPI text claimed it "carries the same
// ManageEnvVars rule as sshCredential". Anyone who could compose a job could point
// it at ANY department's secret and have the runner write that value to a file for
// `--become-password-file`. `sshCredential`, two fields above it in the same
// handler, had all four checks.
//
// These are persona tests, not route tests: the question is what a *restricted
// departmental admin* can and cannot bind, because that is the control the gate
// exists to enforce.

// becomeGateServer builds the RBAC fixture plus an ansible script to hang jobs on,
// a second department, and one secret owned by each.
func becomeGateServer(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// An ansible script — the run type the become flag applies to — and a bash one
	// for the run-type refusal.
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('play','ansible','site.yml','runner','sha256:aaa','scripts/play.yaml','t')`)
	exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
	      VALUES('shell','bash','echo hi','ssh','sha256:bbb','scripts/shell.yaml','t')`)

	// Finance: a department the 'sec-admins' persona does NOT hold. Its scope must
	// exist and map to its agency, or a Finance-scoped job resolves nothing.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc-fin','finance','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-fin','ag-fin')`)

	// One secret per department, both named the same in their own scope — the
	// Phase-E shape, and the one a hand-rolled "SELECT id FROM secrets WHERE key=?"
	// would resolve wrongly.
	exec(`INSERT INTO secrets(id,key,source,scope,owner_agency,created_at)
	      VALUES('sec-prod','BECOME_PASSWORD','stored','prod','ag:prod','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('sec-prod','ag:prod')`)
	exec(`INSERT INTO secrets(id,key,source,scope,owner_agency,created_at)
	      VALUES('sec-fin','BECOME_PASSWORD','stored','finance','ag-fin','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('sec-fin','ag-fin')`)
	return h, pool
}

// TestBecomePasswordIsDepartmental is THE fence: the restricted admin may bind
// their own department's become password and must not be able to bind another
// department's. Before RA-21 the second case returned 200 and persisted.
func TestBecomePasswordIsDepartmental(t *testing.T) {
	h, pool := becomeGateServer(t)

	// Own department: prod's admin binds prod's BECOME_PASSWORD. Any non-403 proves
	// the gate passed — this test is about authorization, not about the rest of the
	// compose body.
	own := `{"name":"j-own","scriptRef":"play","scope":"prod","becomePasswordSecret":"BECOME_PASSWORD"}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-admins", own); rec.Code == http.StatusForbidden {
		t.Fatalf("binding OWN department's become password = 403, want the gate to pass (%s)", rec.Body.String())
	}

	// Another department's. The job is scoped to finance, so the row that resolves
	// is Finance's — which this actor holds no permission over.
	other := `{"name":"j-fin","scriptRef":"play","scope":"finance","becomePasswordSecret":"BECOME_PASSWORD"}`
	rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-admins", other)
	if rec.Code != http.StatusForbidden {
		t.Errorf("binding ANOTHER department's become password = %d, want 403 — this is the "+
			"hole v0.57.3 shipped (%s)", rec.Code, rec.Body.String())
	}
	// And nothing was persisted on the refusal: a gate that 403s after the INSERT
	// is not a gate.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name='j-fin'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("refused job rows = %d, want 0 — the refusal must precede the write", n)
	}
}

// TestBecomePasswordRequiresManageEnvVars — the coarse verb half. An operator can
// compose nothing at all (compose is admin-gated), so the persona that isolates
// the verb is a viewer: it must not be able to reach the field either way.
func TestBecomePasswordRequiresManageEnvVars(t *testing.T) {
	h, _ := becomeGateServer(t)
	body := `{"name":"j-v","scriptRef":"play","scope":"prod","becomePasswordSecret":"BECOME_PASSWORD"}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-viewers", body); rec.Code != http.StatusForbidden {
		t.Errorf("viewer binding a become password = %d, want 403", rec.Code)
	}
}

// TestBecomePasswordRunTypeAndExistence — the two validation halves the plan
// specifies alongside the authorization ones. A become password on a bash job is
// silently inert at dispatch, which is the misconfiguration that looks like it
// worked; a name with no resolvable row fails closed at dispatch, far from the
// person who typed it.
func TestBecomePasswordRunTypeAndExistence(t *testing.T) {
	h, _ := becomeGateServer(t)

	bash := `{"name":"j-bash","scriptRef":"shell","scope":"prod","becomePasswordSecret":"BECOME_PASSWORD"}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-admins", bash); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("become password on a BASH job = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}

	missing := `{"name":"j-missing","scriptRef":"play","scope":"prod","becomePasswordSecret":"NO_SUCH_SECRET"}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-admins", missing); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("become password naming no row = %d, want 422 (%s)", rec.Code, rec.Body.String())
	}
}

// TestBecomePasswordUnchangedJobIsNotLockedOut — the set-or-changed rule, mirrored
// from sshCredential. The Composer's PUT is a FULL REPLACE (JC1), so a job that
// already carries a become password is resent with it on every unrelated edit. If
// the gate fired on presence rather than on change, an editor who may not bind the
// secret could never touch the job's description again.
func TestBecomePasswordUnchangedJobIsNotLockedOut(t *testing.T) {
	h, pool := becomeGateServer(t)

	create := `{"name":"j-keep","scriptRef":"play","scope":"prod","becomePasswordSecret":"BECOME_PASSWORD"}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/jobs", "sec-admins", create); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	var stored sql.NullString
	if err := pool.QueryRow(`SELECT become_password_secret FROM jobs WHERE source='cronomicon' AND name='j-keep'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.String != "BECOME_PASSWORD" {
		t.Fatalf("stored become_password_secret = %q, want BECOME_PASSWORD", stored.String)
	}

	// Re-send unchanged, with an unrelated edit. Must not be refused.
	same := `{"name":"j-keep","scriptRef":"play","scope":"prod","becomePasswordSecret":"BECOME_PASSWORD","description":"edited"}`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/jobs/j-keep", "sec-admins", same); rec.Code == http.StatusForbidden {
		t.Errorf("full-replace PUT of an UNCHANGED become password = 403; the gate must fire on "+
			"set-or-changed, not on presence (%s)", rec.Body.String())
	}

	// Clearing shrinks the grant and needs nothing extra.
	cleared := `{"name":"j-keep","scriptRef":"play","scope":"prod"}`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/jobs/j-keep", "sec-admins", cleared); rec.Code == http.StatusForbidden {
		t.Errorf("clearing the become password = 403, want it allowed (%s)", rec.Body.String())
	}
}
