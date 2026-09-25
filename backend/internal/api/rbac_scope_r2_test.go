package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// seedScopedJob inserts an cronomicon job in the given scope (nil ⇒ global) and
// returns its rowid (the {jobId} path segment).
func seedScopedJob(t *testing.T, pool *sql.DB, name string, scope *string) string {
	t.Helper()
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, concurrency_policy, scope, synced_at)
		VALUES(?, 'cronomicon', 'bash', 'echo hi', 'Allow', ?, '2026-01-01T00:00:00Z')`, name, scope); err != nil {
		t.Fatalf("seed job %s: %v", name, err)
	}
	var rowid string
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE name=?`, name).Scan(&rowid); err != nil {
		t.Fatalf("job rowid %s: %v", name, err)
	}
	return rowid
}

// TestJobBindingWriteScopeEnforced (M1): a binding is an access-control write, so
// putJobBindings must gate on the OWNER job's scope — not stay ManageEnvVars-only.
// A prod-restricted manager may write bindings on a prod job, is 404'd on a
// staging job (no existence oracle), and 403'd on a global job.
func TestJobBindingWriteScopeEnforced(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	sp := func(s string) *string { return &s }
	prodJob := seedScopedJob(t, pool, "prodjob", sp("prod"))
	stagingJob := seedScopedJob(t, pool, "stagingjob", sp("staging"))
	globalJob := seedScopedJob(t, pool, "globaljob", nil)

	body := `{"bindings":[{"kind":"secret","name":"SEC_PROD"}]}`

	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+prodJob, "sec-admins", body); rec.Code != http.StatusOK {
		t.Errorf("write bindings on in-scope job = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+stagingJob, "sec-admins", body); rec.Code != http.StatusNotFound {
		t.Errorf("write bindings on out-of-scope job = %d, want 404; body: %s", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+globalJob, "sec-admins", body); rec.Code != http.StatusForbidden {
		t.Errorf("write bindings on global job = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

// TestJobBindingWriteUnrestrictedAllowed (M1): an unrestricted admin writes
// bindings on any job, including a global one.
func TestJobBindingWriteUnrestrictedAllowed(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	globalJob := seedScopedJob(t, pool, "globaljob", nil)
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+globalJob, "sec-admins",
		`{"bindings":[{"kind":"secret","name":"SEC_GLOBAL"}]}`); rec.Code != http.StatusOK {
		t.Errorf("unrestricted write on global job = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// TestJobBindingReadScopeEnforced (M3b): getJobBindings must not disclose the
// declared reference names of an out-of-scope job to a restricted operator.
func TestJobBindingReadScopeEnforced(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"viewer": "prod"})
	sp := func(s string) *string { return &s }
	prodJob := seedScopedJob(t, pool, "prodjob", sp("prod"))
	stagingJob := seedScopedJob(t, pool, "stagingjob", sp("staging"))

	if rec := reqAs(t, h, http.MethodGet, "/api/v1/job-reference-bindings/"+prodJob, "sec-viewers", ""); rec.Code != http.StatusOK {
		t.Errorf("read in-scope job bindings = %d, want 200", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodGet, "/api/v1/job-reference-bindings/"+stagingJob, "sec-viewers", ""); rec.Code != http.StatusForbidden {
		t.Errorf("read out-of-scope job bindings = %d, want 403", rec.Code)
	}
}

// TestScriptBindingWriteRequiresUnrestricted (M1/DEC-3): scripts have no scope, so
// only an unrestricted manager may edit their (cross-scope shared) bindings.
func TestScriptBindingWriteRequiresUnrestricted(t *testing.T) {
	seedScript := func(pool *sql.DB) {
		if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
			VALUES('shared.yaml', 'bash', 'echo hi', 'ssh', 'h1', 'scripts/shared.yaml', '2026-01-01T00:00:00Z')`); err != nil {
			t.Fatalf("seed script: %v", err)
		}
	}
	body := `{"bindings":[{"kind":"secret","name":"SEC_GLOBAL"}]}`

	// Restricted manager → 403.
	hR, poolR := secretRBACServer(t, map[string]string{"admin": "prod"})
	seedScript(poolR)
	if rec := reqAs(t, hR, http.MethodPut, "/api/v1/script-reference-bindings/shared.yaml", "sec-admins", body); rec.Code != http.StatusForbidden {
		t.Errorf("restricted script-binding write = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}

	// Unrestricted manager → 200.
	hU, poolU := secretRBACServer(t, nil)
	seedScript(poolU)
	if rec := reqAs(t, hU, http.MethodPut, "/api/v1/script-reference-bindings/shared.yaml", "sec-admins", body); rec.Code != http.StatusOK {
		t.Errorf("unrestricted script-binding write = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// TestScanBareNamesScopeFiltered (M3a): the script-scan bare-name lint must only
// echo reference names the caller can see in scope — otherwise any session confirms
// out-of-scope secret names by planting tokens in a script body.
func TestScanBareNamesScopeFiltered(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"viewer": "prod"})
	// A script body mentioning all three seeded secret keys as bare words.
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, executor, content_hash, source_path, synced_at)
		VALUES('probe.yaml', 'bash', 'echo SEC_GLOBAL SEC_PROD SEC_STAGING', 'ssh', 'h1', 'scripts/probe.yaml', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	rec := reqAs(t, h, http.MethodGet, "/api/v1/script-reference-scan/probe.yaml", "sec-viewers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		BareReferences []struct {
			Name string `json:"name"`
		} `json:"bareReferences"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode scan: %v", err)
	}
	seen := map[string]bool{}
	for _, b := range got.BareReferences {
		seen[b.Name] = true
	}
	if !seen["SEC_GLOBAL"] || !seen["SEC_PROD"] {
		t.Errorf("in-scope/global names missing from scan: %v", seen)
	}
	if seen["SEC_STAGING"] {
		t.Errorf("out-of-scope secret name leaked via scan lint: %v", seen)
	}
}

// TestBindingWriteAuditNamesTheSet (M7): a binding full-replace records the
// resulting set (kinds + bare names, never values) in change_log — so a stale tab
// that silently revokes/resurrects a binding is visible/diffable, not an empty
// "updated" row indistinguishable from a tag edit.
func TestBindingWriteAuditNamesTheSet(t *testing.T) {
	h, pool := secretRBACServer(t, nil) // unrestricted admin
	job := seedScopedJob(t, pool, "auditjob", nil)

	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+job, "sec-admins",
		`{"bindings":[{"kind":"secret","name":"SEC_GLOBAL"},{"kind":"var","name":"REGION"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("set bindings = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var details string
	if err := pool.QueryRow(
		`SELECT details FROM change_log WHERE category='Jobs' AND action='updated' AND target='auditjob' ORDER BY rowid DESC LIMIT 1`).
		Scan(&details); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if !strings.Contains(details, "secret:SEC_GLOBAL") || !strings.Contains(details, "var:REGION") {
		t.Errorf("binding audit detail does not name the set: %q", details)
	}

	// A revoke (empty set) must be a DISTINCT, non-empty audit detail.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/job-reference-bindings/"+job, "sec-admins",
		`{"bindings":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("clear bindings = %d, want 200", rec.Code)
	}
	if err := pool.QueryRow(
		`SELECT details FROM change_log WHERE category='Jobs' AND action='updated' AND target='auditjob' ORDER BY rowid DESC LIMIT 1`).
		Scan(&details); err != nil {
		t.Fatalf("read revoke audit row: %v", err)
	}
	if details == "" || strings.Contains(details, "SEC_GLOBAL") {
		t.Errorf("revoke audit detail should be a distinct non-empty '(none)', got: %q", details)
	}
}

// seedEnvVar inserts a plaintext env var in the given scope (nil ⇒ global).
func seedEnvVar(t *testing.T, pool *sql.DB, id, key string, scope *string) {
	t.Helper()
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, value, scope, created_by, created_at, last_modified_by, last_modified_at)
		VALUES(?, ?, 'v', ?, 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z')`, id, key, scope); err != nil {
		t.Fatalf("seed env var %s: %v", key, err)
	}
}

// TestEnvVarWriteScopeEnforced (M4): env-var create/update/delete/tag now enforce
// the same scope model as secrets — closing an INTEGRITY hole where a restricted
// manager could plant a scoped var that the resolver's scope-exact-beats-global
// ordering prefers over the global row in another scope's runs.
func TestEnvVarWriteScopeEnforced(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	sp := func(s string) *string { return &s }
	seedEnvVar(t, pool, "ev-global", "EV_GLOBAL", nil)
	seedEnvVar(t, pool, "ev-prod", "EV_PROD", sp("prod"))
	seedEnvVar(t, pool, "ev-staging", "EV_STAGING", sp("staging"))

	// Create out-of-scope → 403.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"NEW_STG","value":"x","scope":"staging"}`); rec.Code != http.StatusForbidden {
		t.Errorf("create out-of-scope var = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Create global → 403.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"NEW_GLOBAL","value":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("create global var by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Create in-scope WITHOUT agencyIds → 201, and the variable INHERITS the
	// creator's department (RA-9). This was a 422 under RF-Q2(a); Phase D turns the
	// refusal into a default, because the requirement was correct but made the safe
	// outcome something the caller had to opt into on every create. The assertion
	// that matters is the membership row, not the status: a 201 with no membership
	// would be the silent-global regression this whole phase exists to prevent.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"NEW_INHERIT","value":"x","scope":"prod"}`); rec.Code != http.StatusCreated {
		t.Errorf("create in-scope var without agencyIds = %d, want 201 (RA-9 inheritance); body: %s",
			rec.Code, rec.Body.String())
	}
	var inherited string
	if err := pool.QueryRow(`SELECT COALESCE(GROUP_CONCAT(va.agency_id),'')
	                         FROM env_var_agencies va JOIN env_vars v ON v.id = va.env_var_id
	                         WHERE v.key = 'NEW_INHERIT'`).Scan(&inherited); err != nil {
		t.Fatalf("read inherited membership: %v", err)
	}
	if inherited != "ag:prod" {
		t.Errorf("inherited membership = %q, want \"ag:prod\" — an unmembered scoped row is "+
			"consumable by EVERY department's prod runs (AG-Q1(b))", inherited)
	}
	// Create in-scope naming a held agency → 201 (the explicit path still works).
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"NEW_PROD","value":"x","scope":"prod","agencyIds":["ag:prod"]}`); rec.Code != http.StatusCreated {
		t.Errorf("create in-scope var = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	// Create naming an agency that EXISTS but the caller does not hold → 403.
	// (Naming a nonexistent one would only exercise the unknown_agency branch and
	// would still pass with the CanAgency half deleted — the grants check is the
	// point, so the agency has to be real.)
	if _, err := pool.Exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-other','Other','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-admins",
		`{"key":"NEW_PROD2","value":"x","scope":"prod","agencyIds":["ag-other"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("create var in an existing-but-unheld agency = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Move an in-scope var OUT of scope via update → 403.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-prod", "sec-admins",
		`{"key":"EV_PROD","value":"x","scope":"staging"}`); rec.Code != http.StatusForbidden {
		t.Errorf("update moving prod→staging var = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Rewrite a GLOBAL var → 403 (the integrity hole).
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-global", "sec-admins",
		`{"key":"EV_GLOBAL","value":"attacker","scope":"prod"}`); rec.Code != http.StatusForbidden {
		t.Errorf("rewrite global var by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Delete an out-of-scope var → 404.
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-vars/ev-staging", "sec-admins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete out-of-scope var = %d, want 404", rec.Code)
	}
	// Delete a global var → 403.
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-vars/ev-global", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete global var by restricted manager = %d, want 403", rec.Code)
	}
	// Tag a global var → 403.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-var-tags/ev-global", "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("tag global var by restricted manager = %d, want 403", rec.Code)
	}

	// The global var is untouched by any refused write.
	var val, scope sql.NullString
	if err := pool.QueryRow(`SELECT value, scope FROM env_vars WHERE id='ev-global'`).Scan(&val, &scope); err != nil {
		t.Fatalf("reread global var: %v", err)
	}
	if val.String != "v" || scope.Valid {
		t.Errorf("global var mutated: value=%q scopeValid=%v", val.String, scope.Valid)
	}
}

// TestEnvVarWriteUnrestrictedAllowed (M4): an unrestricted admin still manages a
// global var normally.
func TestEnvVarWriteUnrestrictedAllowed(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	seedEnvVar(t, pool, "ev-global", "EV_GLOBAL", nil)
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-vars/ev-global", "sec-admins",
		`{"key":"EV_GLOBAL","value":"updated"}`); rec.Code != http.StatusOK {
		t.Errorf("unrestricted update of global var = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-vars/ev-global", "sec-admins", ""); rec.Code != http.StatusNoContent {
		t.Errorf("unrestricted delete of global var = %d, want 204", rec.Code)
	}
}
