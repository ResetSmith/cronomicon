package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// RF-13 — the agency-gate coverage for the surfaces v0.56.6 was supposed to reach
// and did not (the RBAC-fixes plan).
//
// The absence of these tests is WHY the v0.56.6 gaps shipped: no test in this
// package touched ssh_credential_agencies, runner_agencies or env_var_agencies, so
// a gate on the wrong route (SSH create instead of SSH update) and two entity
// kinds with no gate at all both looked green.

// TestSSHKeyUpdateIsDepartmental — RF-1. PUT /ssh/credentials/{id} rewrites key
// MATERIAL, which RB-16 names the highest-consequence gap in the program; v0.56.6
// put its gate on POST (create), where the path has no {credentialId} at all, so
// the check could only ever evaluate the empty-membership branch.
func TestSSHKeyUpdateIsDepartmental(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credentials (id,label,source,created_at,last_modified_at)
	      VALUES ('k-own','key-own','stored','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credentials (id,label,source,created_at,last_modified_at)
	      VALUES ('k-fin','key-fin','stored','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-own','ag:prod')`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-fin','ag-fin')`)

	// Own agency: the gate passes and the request reaches the handler, which then
	// judges the body on its own terms. Any non-403 proves the gate let it through —
	// this test is about the gate, not about key parsing.
	body := `{"label":"key-own","source":"stored","material":"rotated"}`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/ssh/credentials/k-own", "sec-admins", body); rec.Code == http.StatusForbidden {
		t.Fatalf("update of an own-agency key = 403, want the gate to pass (%s)", rec.Body.String())
	}
	finBody := `{"label":"key-fin","source":"stored","material":"rotated"}`
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/ssh/credentials/k-fin", "sec-admins", finBody); rec.Code != http.StatusForbidden {
		t.Errorf("update of ANOTHER department's key material = %d, want 403 — this is the "+
			"route RB-16 was about and the one v0.56.6 left open", rec.Code)
	}

	// Tags sit beside the material and now carry the same gate (RF-5).
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/ssh-credential-tags/k-fin", "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("tagging another department's key = %d, want 403", rec.Code)
	}
}

// TestSSHKeyCreateBindsAnAgency — RF-Q2(a). A restricted creator must place the new
// key in an agency they hold, because an unmembered key is unrestricted-only
// (RB-Q14) and would otherwise be born unreachable by its own author. Before this,
// the misplaced gate made EVERY create unrestricted-only.
func TestSSHKeyCreateBindsAnAgency(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}

	if rec := reqAs(t, h, http.MethodPost, "/api/v1/ssh/credentials", "sec-admins",
		`{"label":"k1","source":"stored","material":"m"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("restricted create with no agencyIds = %d, want 422 agency_required (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/ssh/credentials", "sec-admins",
		`{"label":"k2","source":"stored","material":"m","agencyIds":["ag-fin"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("restricted create into an UNHELD agency = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	// Naming a HELD agency clears the authorization gate — the handler then rejects
	// this body on key-material grounds (422 validation_failed), which is a
	// different failure and proves the gate is no longer the one refusing.
	rec := reqAs(t, h, http.MethodPost, "/api/v1/ssh/credentials", "sec-admins",
		`{"label":"k3","source":"stored","material":"m","agencyIds":["ag:prod"]}`)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("restricted create into a HELD agency = 403, want the gate to pass (%s)", rec.Body.String())
	}
	if body := rec.Body.String(); rec.Code == http.StatusUnprocessableEntity && !contains(body, "key") {
		t.Fatalf("restricted create into a HELD agency = 422 for a non-key reason: %s", body)
	}

	// An unrestricted creator may still mint shared infrastructure deliberately —
	// no agencyIds, and the agency rule does not stop them.
	h2, _ := secretRBACServer(t, nil)
	rec2 := reqAs(t, h2, http.MethodPost, "/api/v1/ssh/credentials", "sec-admins",
		`{"label":"k-global","source":"stored","material":"m"}`)
	if rec2.Code == http.StatusForbidden || rec2.Code == http.StatusUnprocessableEntity && contains(rec2.Body.String(), "agency_required") {
		t.Errorf("unrestricted create with no agencyIds = %d, want the agency rule to allow it (%s)",
			rec2.Code, rec2.Body.String())
	}
}

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }

// TestSecretCreateBindsAnAgency — RF-Q2(a) on the secret surface, where the
// membership actually persists (the SSH create path cannot be exercised end to end
// without real key material).
func TestSecretCreateBindsAnAgency(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"S1","source":"stored","scope":"prod","value":"v","agencyIds":["ag-fin"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("create into an UNHELD agency = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"S2","source":"stored","scope":"prod","value":"v","agencyIds":["ag:prod"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create into a HELD agency = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
	// The binding PERSISTED — otherwise the creator is locked out of their own
	// secret on the very next request, which is the failure this rule prevents.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies m
	                         JOIN secrets s ON s.id = m.secret_id
	                         WHERE s.key = 'S2' AND m.agency_id = 'ag:prod'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("created secret membership rows = %d, want 1", n)
	}
	// And the creator can immediately act on it — the round trip RF-Q2(a) exists for.
	sid := secretID(t, pool, "S2")
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+sid+"/reveal", "sec-admins", ""); rec.Code != http.StatusOK {
		t.Errorf("reveal of the secret they just created = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestRehomingIsAuthorized — RF-1b, and the single most important test in this
// file: without it every other gate here is theatre.
//
// The membership endpoints (PUT /{secret,env-var,ssh-credential,runner}-agencies)
// were gated on configureApp alone, with no check on the entity being moved. So a
// Tax admin refused a write on Finance's key could simply move that key into Tax
// and try again — three requests, no privilege they did not already hold. Every
// departmental gate in v0.56.7 is only as strong as this one.
func TestRehomingIsAuthorized(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	finSecret := secretID(t, pool, "SEC_PROD")
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag-fin')`, finSecret)
	exec(`INSERT INTO ssh_credentials (id,label,source,created_at,last_modified_at)
	      VALUES ('k-fin','key-fin','stored','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-fin','ag-fin')`)
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES ('r-fin','runner-fin','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-fin','ag-fin')`)

	// Step 1 of the bypass: claim another department's entity.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/secret-agencies", "sec-admins",
		`[{"id":"`+finSecret+`","agencyIds":["ag:prod"]}]`); rec.Code != http.StatusForbidden {
		t.Errorf("re-homing another department's SECRET = %d, want 403 — this is the "+
			"three-request bypass of every gate in this release", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/ssh-credential-agencies", "sec-admins",
		`[{"id":"k-fin","agencyIds":["ag:prod"]}]`); rec.Code != http.StatusForbidden {
		t.Errorf("re-homing another department's SSH KEY = %d, want 403", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/runner-agencies", "sec-admins",
		`[{"runnerId":"r-fin","agencyIds":["ag:prod"]}]`); rec.Code != http.StatusForbidden {
		t.Errorf("re-homing another department's RUNNER = %d, want 403", rec.Code)
	}
	// The secret is still Finance's — the denial was not merely cosmetic.
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id = ? AND agency_id = 'ag-fin'`,
		finSecret).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Finance's membership row after the refused move = %d, want 1 (still Finance's)", n)
	}

	// The other direction: moving your OWN entity into a department you do not hold
	// is equally refused — otherwise you could donate access rather than steal it.
	ownSecret := secretID(t, pool, "SEC_GLOBAL")
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:prod')`, ownSecret)
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/secret-agencies", "sec-admins",
		`[{"id":"`+ownSecret+`","agencyIds":["ag-fin"]}]`); rec.Code != http.StatusForbidden {
		t.Errorf("moving your own secret INTO an unheld department = %d, want 403", rec.Code)
	}

	// And an unrestricted admin still administers membership freely — this is a
	// boundary, not the end of the Membership screen.
	h2, pool2 := secretRBACServer(t, nil)
	s2 := secretID(t, pool2, "SEC_PROD")
	if _, err := pool2.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-any','Any','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h2, http.MethodPut, "/api/v1/secret-agencies", "sec-admins",
		`[{"id":"`+s2+`","agencyIds":["ag-any"]}]`); rec.Code != http.StatusOK {
		t.Errorf("unrestricted admin setting membership = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestVariableWritesAreDepartmental — RF-3. RB-32 resolved that manageEnvVars
// becomes departmental and named secrets AND variables; v0.56.6 gated only secrets.
func TestVariableWritesAreDepartmental(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	sp := func(s string) *string { return &s }
	seedEnvVar(t, pool, "ev-own", "EV_OWN", sp("prod"))
	seedEnvVar(t, pool, "ev-fin", "EV_FIN", sp("prod"))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO env_var_agencies (env_var_id, agency_id) VALUES ('ev-own','ag:prod')`)
	exec(`INSERT INTO env_var_agencies (env_var_id, agency_id) VALUES ('ev-fin','ag-fin')`)

	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-own", "sec-admins",
		`{"key":"EV_OWN","value":"y","scope":"prod"}`); rec.Code != http.StatusOK {
		t.Fatalf("update of an own-agency variable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// Same scope, same caller, other department: refused. Scope access is not
	// departmental authority.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-fin", "sec-admins",
		`{"key":"EV_FIN","value":"y","scope":"prod"}`); rec.Code != http.StatusForbidden {
		t.Errorf("update of another department's variable = %d, want 403", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-vars/ev-fin", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete of another department's variable = %d, want 403", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-var-tags/ev-fin", "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("tagging another department's variable = %d, want 403", rec.Code)
	}
}

// TestUnmemberedVariableIsUnrestrictedOnly — RB-Q14 for the variable half.
func TestUnmemberedVariableIsUnrestrictedOnly(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	sp := func(s string) *string { return &s }
	seedEnvVar(t, pool, "ev-shared", "EV_SHARED", sp("prod")) // in scope, in NO agency

	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-shared", "sec-admins",
		`{"key":"EV_SHARED","value":"y","scope":"prod"}`); rec.Code != http.StatusForbidden {
		t.Errorf("restricted write to an unmembered variable = %d, want 403 (RB-Q14)", rec.Code)
	}

	h2, pool2 := secretRBACServer(t, nil)
	seedEnvVar(t, pool2, "ev-shared", "EV_SHARED", sp("prod"))
	if rec := reqAs(t, h2, http.MethodPut, "/api/v1/env-vars/ev-shared", "sec-admins",
		`{"key":"EV_SHARED","value":"y","scope":"prod"}`); rec.Code != http.StatusOK {
		t.Errorf("unrestricted write to an unmembered variable = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// TestRunnerWritesAreDepartmental — RF-2/RF-Q3. RB-16 said "runners are the same"
// and no runner route ever consulted runner_agencies.
//
// The general-pool case is the one worth stating plainly: a runner in NO agency
// executes every department's unscoped and system work, so draining or
// reconfiguring it is unrestricted-only — the RB-Q14 mirror ratified as RF-Q3.
func TestRunnerWritesAreDepartmental(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	seedRunner := func(id, name string) {
		exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
		      VALUES (?,?,'online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, id, name)
	}
	seedRunner("r-own", "runner-own")
	seedRunner("r-fin", "runner-fin")
	seedRunner("r-pool", "runner-pool") // deliberately unmembered: the general pool
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-own','ag:prod')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-fin','ag-fin')`)

	// Own agency: the gate lets the request through to the handler. Any non-403 is
	// a pass — the handler's own outcome (200/404/409) is not what this test is about.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/runners/r-own/drain", "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("drain of an own-agency runner = 403, want the gate to pass (%s)", rec.Body.String())
	}
	for _, path := range []string{
		"/api/v1/runners/r-fin/drain",
		"/api/v1/runners/r-fin/resync",
		"/api/v1/runners/r-fin/test",
	} {
		if rec := reqAs(t, h, http.MethodPost, path, "sec-admins", ""); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s (another department's runner) = %d, want 403", path, rec.Code)
		}
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/runners/r-fin", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("deregister of another department's runner = %d, want 403", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/runner-tags/r-fin", "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("tagging another department's runner = %d, want 403", rec.Code)
	}

	// RF-Q3: the general pool is shared infrastructure, so a restricted admin may
	// not touch it — and an unrestricted one may.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/runners/r-pool/drain", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted drain of a GENERAL-POOL runner = %d, want 403 (RF-Q3)", rec.Code)
	}
	h2, pool2 := secretRBACServer(t, nil)
	if _, err := pool2.Exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	                         VALUES ('r-pool','runner-pool','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h2, http.MethodPost, "/api/v1/runners/r-pool/drain", "sec-admins", ""); rec.Code == http.StatusForbidden {
		t.Errorf("unrestricted drain of a general-pool runner = 403, want the gate to pass (%s)", rec.Body.String())
	}

	// DRF-9: accepting a placement suggestion is a placement, and a suggestion
	// only ever targets an UNBOUND runner — i.e. the general pool. So it carries
	// RF-Q3's rule verbatim: refused for a restricted admin, passes the gate for
	// an unrestricted one. Without this the one-click was a bypass of the check
	// PUT /runner-agencies already makes.
	const accept = `{"historyId":1}`
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/runners/r-pool/placement", "sec-admins", accept); rec.Code != http.StatusForbidden {
		t.Errorf("restricted accept-placement on a GENERAL-POOL runner = %d, want 403 (RF-Q3 / DRF-9)", rec.Code)
	}
	// Unrestricted: the gate passes. With no snapshot seeded the handler answers
	// 409 placement_stale, which is the handler's own outcome and not this test's
	// concern — any non-403 is a pass, as above.
	if rec := reqAs(t, h2, http.MethodPost, "/api/v1/runners/r-pool/placement", "sec-admins", accept); rec.Code == http.StatusForbidden {
		t.Errorf("unrestricted accept-placement on a general-pool runner = 403, want the gate to pass (%s)", rec.Body.String())
	}
}

// TestPerRunReferencesAreDepartmental — RF-4, the first of the three in-handler
// sites the plan called "easy to miss" and which were duly missed.
//
// The scenario has to be chosen carefully, because the injector's OWN predicate
// (AG-Q1(b), v0.52.x) already refuses to resolve a secret whose agencies do not
// intersect the RUN's — so attaching another department's secret was never the
// live hole; it fails closed at dispatch either way.
//
// The hole is a MULTI-GRANT actor. Alice holds manageEnvVars on Finance and only
// triggerJobs on Production. The old flat check asked "do you hold manageEnvVars
// at all", she does (via Finance), so she could attach a PRODUCTION secret to a
// PRODUCTION run — a secret dispatch will happily resolve, because the run is in
// Production. That is the cross-product leak wearing a different hat, and the
// per-entity check is what closes it.
func TestPerRunReferencesAreDepartmental(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Two departments, and a scope that belongs to Production.
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-prod','Production','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc-p','prod','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc-p','ag-prod')`)

	// Alice: manageEnvVars on FINANCE, triggerJobs on PRODUCTION. Neither grant
	// gives her manageEnvVars where she is about to run.
	exec(`INSERT INTO roles (name,description,builtin,rank,trigger_jobs,kill_jobs,
	                         manage_env_vars,publish_schedule,configure_app,manage_roles)
	      VALUES ('env-only','RF-4 fixture',0,2,0,0,1,0,0,0)`)
	exec(`INSERT INTO roles (name,description,builtin,rank,trigger_jobs,kill_jobs,
	                         manage_env_vars,publish_schedule,configure_app,manage_roles)
	      VALUES ('run-only','RF-4 fixture',0,2,1,0,0,0,0,0)`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at)
	      VALUES ('g-e','alice','env-only','ag-fin',0,'2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_at)
	      VALUES ('g-r','alice','run-only','ag-prod',0,'2026-01-01T00:00:00Z')`)
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("RefreshRoles: %v", err)
	}

	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('j-prod','cronomicon','bash','prod',1,'2026-01-01T00:00:00Z')`)
	// The secret lives in Production — the SAME department as the run, so dispatch
	// would resolve it. Only the actor's grants stand between her and its value.
	prodSecret := secretID(t, pool, "SEC_PROD")
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag-prod')`, prodSecret)

	jobPath := runPath(t, pool, "j-prod")

	// Sanity: she may trigger the job itself. If this failed, the 403 below would
	// prove nothing — it could be the trigger verb rather than the reference.
	if rec := reqAs(t, h, http.MethodPost, jobPath, "alice", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("plain trigger by alice = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	// The hole: attaching a Production secret while holding manageEnvVars only on
	// Finance.
	rec := reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"references":[{"kind":"secret","name":"SEC_PROD"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("attaching a secret from a department where she lacks manageEnvVars = %d, "+
			"want 403 — holding the verb SOMEWHERE is not holding it HERE (%s)", rec.Code, rec.Body.String())
	}

	// RA-8 — the alias must not become a bypass. Entitlement is judged on the ROW
	// (kind+name); an alias only renames the key the value lands on, so dressing the
	// same out-of-department secret in a harmless-looking destination changes
	// nothing. If this ever returns 202, the attach gate is reading the alias.
	rec = reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"references":[{"kind":"secret","name":"SEC_PROD","as":"BECOME_PASSWORD"}]}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("attaching an out-of-department secret UNDER AN ALIAS = %d, want 403 — "+
			"the alias is a destination, never an entitlement (%s)", rec.Code, rec.Body.String())
	}

	// An unrestricted manager attaching the same secret is unaffected: this is a
	// boundary, not a ban on per-run references.
	if rec := reqAs(t, h, http.MethodPost, jobPath, "sec-admins",
		`{"references":[{"kind":"secret","name":"SEC_PROD"}]}`); rec.Code != http.StatusAccepted {
		t.Errorf("unrestricted admin attaching the same secret = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}

	// A name that resolves to NO row must not 403 — dispatch fails closed on it with
	// the oracle-safe message, and denying here would make the trigger route an
	// existence probe for stored secret names (M2).
	rec = reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"references":[{"kind":"secret","name":"NO_SUCH_SECRET"}]}`)
	if rec.Code == http.StatusForbidden {
		t.Errorf("reference to a NONEXISTENT secret = 403; it must not become an existence oracle (%s)",
			rec.Body.String())
	}

	// The OTHER two reference kinds go through the same predicate and are covered
	// here because "it works for secrets" is exactly the assumption that let
	// variables ship ungated in v0.56.6.
	exec(`INSERT INTO env_vars (id,key,value,scope,created_at) VALUES ('v-prod','V_PROD','x','prod','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO env_var_agencies (env_var_id, agency_id) VALUES ('v-prod','ag-prod')`)
	if rec := reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"references":[{"kind":"var","name":"V_PROD"}]}`); rec.Code != http.StatusForbidden {
		t.Errorf("attaching a VARIABLE from a department where she lacks manageEnvVars = %d, want 403", rec.Code)
	}
	exec(`INSERT INTO ssh_credentials (id,label,source,created_at,last_modified_at)
	      VALUES ('k-prod','KEY_PROD','stored','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-prod','ag-prod')`)
	if rec := reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"references":[{"kind":"key","name":"KEY_PROD"}]}`); rec.Code != http.StatusForbidden {
		t.Errorf("attaching a KEY from a department where she lacks manageEnvVars = %d, want 403", rec.Code)
	}
	// The per-run sshCredential selector is a separate in-handler site (RF-4) and
	// takes the same rule.
	if rec := reqAs(t, h, http.MethodPost, jobPath, "alice",
		`{"sshCredential":"KEY_PROD"}`); rec.Code != http.StatusForbidden {
		t.Errorf("selecting a KEY from a department where she lacks manageEnvVars = %d, want 403", rec.Code)
	}
}

// TestUnmemberedReferenceIsAttachable — the RB-Q14 branch of RF-4, stated as a
// test because it is the one place this release deliberately does NOT apply the
// unrestricted-only rule.
//
// An unmembered secret is shared infrastructure. Attaching it to a run whose scope
// the actor already holds is CONSUMPTION, not a write — RB-Q14 leaves consumption
// alone, and dispatch would resolve the same row for the same run if the job
// declared it. Applying the write rule here would also have been a cliff: migration
// 670 backfills no membership, so nearly every row in the field is unmembered.
func TestUnmemberedReferenceIsAttachable(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"operator": "prod"})
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO roles (name,description,builtin,rank,trigger_jobs,kill_jobs,
	                         manage_env_vars,publish_schedule,configure_app,manage_roles)
	      VALUES ('dept-op','RF-4 fixture',0,2,1,0,1,0,0,0)`)
	exec(`UPDATE access_grants SET role='dept-op' WHERE ad_group='sec-operators'`)
	if err := auth.RefreshRoles(context.Background(), pool); err != nil {
		t.Fatalf("RefreshRoles: %v", err)
	}
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at)
	      VALUES ('j-prod','cronomicon','bash','prod',1,'2026-01-01T00:00:00Z')`)
	// SEC_GLOBAL is seeded global and in NO agency — the shared-infrastructure row.
	if rec := reqAs(t, h, http.MethodPost, runPath(t, pool, "j-prod"), "sec-operators",
		`{"references":[{"kind":"secret","name":"SEC_GLOBAL"}]}`); rec.Code != http.StatusAccepted {
		t.Errorf("restricted operator attaching an UNMEMBERED (shared) secret = %d, want 202 — "+
			"RB-Q14 leaves run-time consumption alone, and 670 backfills no membership (%s)",
			rec.Code, rec.Body.String())
	}
}
