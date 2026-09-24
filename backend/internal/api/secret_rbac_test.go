package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/api"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// secretRBACServer boots a trusted-header server with two groups: sec-admins→admin
// (has ManageEnvVars) and sec-viewers→viewer (no ManageEnvVars). `restrict` maps a
// role to the single scope it is restricted to (a non-empty AllowedScopes ⇒ a
// scope-restricted actor); a role absent from the map is unrestricted. It seeds one
// global, one 'prod', and one 'staging' stored secret (envelope-encrypted).
func secretRBACServer(t *testing.T, restrict map[string]string) (http.Handler, *sql.DB) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "sec_rbac.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Authorization resolves from access_grants and nothing else (RB-15; the
	// two-axis tables this fixture also used to seed were dropped in v0.57.8). The
	// migration-810 backfill runs over empty tables at Migrate time, long before
	// any of this, so the fixture must write the grants itself. A restricted role
	// becomes a grant over an agency CONTAINING its scope (there is no
	// single-scope grant shape, RB-Q1); an unrestricted one becomes a "*" grant.
	grantFor := func(group, role string) {
		scope, restricted := restrict[role]
		if !restricted {
			exec(`INSERT OR IGNORE INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
			      VALUES (?,?,?,NULL,1,'2026-01-01T00:00:00Z')`, "g:"+group+":"+role, group, role)
			return
		}
		agencyID, scopeID := "ag:"+scope, "sc:"+scope
		exec(`INSERT OR IGNORE INTO agencies (id,name,created_at) VALUES (?,?,'2026-01-01T00:00:00Z')`, agencyID, "agency-"+scope)
		exec(`INSERT OR IGNORE INTO scopes (id,name,source,created_at) VALUES (?,?,'amadeus','2026-01-01T00:00:00Z')`, scopeID, scope)
		exec(`INSERT OR IGNORE INTO scope_agencies (scope_id,agency_id) VALUES (?,?)`, scopeID, agencyID)
		exec(`INSERT OR IGNORE INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at)
		      VALUES (?,?,?,?,0,'2026-01-01T00:00:00Z')`, "g:"+group+":"+role, group, role, agencyID)
	}
	grantFor("sec-admins", "admin")
	grantFor("sec-viewers", "viewer")
	// Tests that seed their own operator mapping (RB-2/RB-26 cases) get the matching
	// grant here so they do not each have to reproduce the conversion.
	grantFor("sec-operators", "operator")

	cfg := &config.Config{
		AuthMode:       config.AuthModeTrustedHeader,
		TrustedProxies: []string{"192.0.2.0/24"},
		SecretKEKEnv:   "YTM0NTY3ODkwMTIzNDU2Nzg5MDEyMzQ1Njc4OTAxMjM=",
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := secrets.New(pool, cfg, log)
	sp := func(s string) *string { return &s }
	for _, s := range []struct {
		key   string
		scope *string
	}{{"SEC_GLOBAL", nil}, {"SEC_PROD", sp("prod")}, {"SEC_STAGING", sp("staging")}} {
		if _, err := svc.Create(context.Background(), secrets.CreateInput{Key: s.key, Source: "stored", Scope: s.scope, Value: "v"}, "seed"); err != nil {
			t.Fatalf("seed secret %s: %v", s.key, err)
		}
	}
	authSvc := auth.NewService(context.Background(), cfg, pool, log)
	h := api.New(api.Options{Config: cfg, Logger: log, Auth: authSvc, DB: pool}).Handler()
	return h, pool
}

func secretID(t *testing.T, pool *sql.DB, key string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(`SELECT id FROM secrets WHERE key=?`, key).Scan(&id); err != nil {
		t.Fatalf("secret id for %s: %v", key, err)
	}
	return id
}

// reqAs issues a request as a group member, with a CSRF cookie+header for writes.
func reqAs(t *testing.T, h http.Handler, method, path, group, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Remote-User", group+"@example.com")
	req.Header.Set("Remote-Groups", group)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", "tok")
		req.AddCookie(&http.Cookie{Name: "amadeus_csrf", Value: "tok"})
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestEnvSecretsListSessionReadableAndFiltered (P1.7/D3): the metadata list stays
// session-readable by NON-managers (Job Composer / Scripts cross-reference secret
// keys), so a viewer is 200 — but it is scope-filtered. A viewer restricted to
// 'prod' sees only global + prod, never staging.
func TestEnvSecretsListSessionReadableAndFiltered(t *testing.T) {
	h, _ := secretRBACServer(t, map[string]string{"viewer": "prod"})
	rec := reqAs(t, h, http.MethodGet, "/api/v1/env-secrets", "sec-viewers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET /env-secrets = %d, want 200 (session-readable)", rec.Code)
	}
	var got []struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.Key] = true
	}
	if !seen["SEC_GLOBAL"] || !seen["SEC_PROD"] || seen["SEC_STAGING"] {
		t.Errorf("scope-restricted viewer list = %v, want SEC_GLOBAL+SEC_PROD only", seen)
	}
}

// TestEnvSecretsListScopeFiltered (P1.7/D3): a scope-restricted manager sees only
// global + their-scope secrets; an unrestricted admin sees all.
func TestEnvSecretsListScopeFiltered(t *testing.T) {
	// Unrestricted admin: all 3.
	h, _ := secretRBACServer(t, nil)
	rec := reqAs(t, h, http.MethodGet, "/api/v1/env-secrets", "sec-admins", "")
	var all []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &all)
	if len(all) != 3 {
		t.Errorf("unrestricted admin sees %d secrets, want 3", len(all))
	}

	// Admin restricted to 'prod': global + prod only (never staging).
	hr, _ := secretRBACServer(t, map[string]string{"admin": "prod"})
	rec = reqAs(t, hr, http.MethodGet, "/api/v1/env-secrets", "sec-admins", "")
	var got []struct {
		Key string `json:"key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.Key] = true
	}
	if !seen["SEC_GLOBAL"] || !seen["SEC_PROD"] || seen["SEC_STAGING"] {
		t.Errorf("scope-restricted list = %v, want SEC_GLOBAL+SEC_PROD only", seen)
	}
}

// TestEnvSecretRevealScopeEnforced (P1.7/D3): a scope-restricted manager may reveal
// an in-scope (or global) secret but gets 404 (no existence oracle) for one outside
// their scope.
func TestEnvSecretRevealScopeEnforced(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	prodID := secretID(t, pool, "SEC_PROD")
	stagingID := secretID(t, pool, "SEC_STAGING")
	globalID := secretID(t, pool, "SEC_GLOBAL")

	// RB-32/RB-Q14: reveal is departmental now. The restricted admin's grant is over
	// the agency containing `prod` (see secretRBACServer), so the prod secret must
	// BELONG to that agency for them to reveal it — scope access alone is no longer
	// sufficient for secret material.
	if _, err := pool.Exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:prod')`, prodID); err != nil {
		t.Fatalf("seed secret membership: %v", err)
	}

	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+prodID+"/reveal", "sec-admins", ""); rec.Code != http.StatusOK {
		t.Errorf("reveal in-scope (prod) = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// 🔴 RB-Q14: the GLOBAL secret has no agency, which means "no restriction" —
	// every department's jobs consume it. A restricted manager may no longer reveal
	// it, because reading what everyone depends on is not a departmental act.
	// Before v0.56.6 this was 200.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+globalID+"/reveal", "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("restricted reveal of an UNMEMBERED (global) secret = %d, want 403 — "+
			"RB-Q14 makes shared infrastructure unrestricted-only", rec.Code)
	}
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+stagingID+"/reveal", "sec-admins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("reveal out-of-scope (staging) = %d, want 404 (no existence oracle)", rec.Code)
	}
}

// TestEnvSecretWriteScopeEnforced (P1.7/D3): a scope-restricted manager may not
// create a secret outside their grants (403) NOR in the global scope (403,
// ScopeWritable), may not delete an out-of-scope secret (404), may not move an
// in-scope secret out of scope via update (403), and may not migrate/tag one
// out-of-scope (404). In-scope create is allowed (201).
func TestEnvSecretWriteScopeEnforced(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	prodID := secretID(t, pool, "SEC_PROD")
	stagingID := secretID(t, pool, "SEC_STAGING")

	// Create out-of-scope → 403.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"NEW_STAGING","source":"stored","scope":"staging","value":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("create out-of-scope = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Create in the GLOBAL scope → 403 (a restricted actor can't write global).
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"NEW_GLOBAL","source":"stored","value":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("create global by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Create in-scope WITHOUT agencyIds → 201, and the secret INHERITS the creator's
	// department (RA-9, superseding RF-Q2(a)'s 422). Secret material is the
	// highest-consequence case for the shadowing failure this closes: an unmembered
	// prod secret is revealable-by-nobody-restricted but CONSUMABLE by every
	// department's prod runs, so the membership row is the assertion, not the 201.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"NEW_INHERIT","source":"stored","scope":"prod","value":"x"}`); rec.Code != http.StatusCreated {
		t.Errorf("create in-scope without agencyIds = %d, want 201 (RA-9 inheritance); body: %s",
			rec.Code, rec.Body.String())
	}
	var inherited string
	if err := pool.QueryRow(`SELECT COALESCE(GROUP_CONCAT(sa.agency_id),'')
	                         FROM secret_agencies sa JOIN secrets s ON s.id = sa.secret_id
	                         WHERE s.key = 'NEW_INHERIT'`).Scan(&inherited); err != nil {
		t.Fatalf("read inherited membership: %v", err)
	}
	if inherited != "ag:prod" {
		t.Errorf("inherited membership = %q, want \"ag:prod\"", inherited)
	}
	// Create in-scope naming a held agency → 201 (the explicit path still works).
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"NEW_PROD","source":"stored","scope":"prod","value":"x","agencyIds":["ag:prod"]}`); rec.Code != http.StatusCreated {
		t.Errorf("create in-scope = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	// Move an in-scope secret OUT of scope via update → 403.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-secrets/"+prodID, "sec-admins",
		`{"key":"SEC_PROD","source":"stored","scope":"staging","value":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("update moving prod→staging = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Delete an out-of-scope secret → 404.
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-secrets/"+stagingID, "sec-admins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("delete out-of-scope = %d, want 404", rec.Code)
	}
	// Migrate an out-of-scope secret → 404.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+stagingID+"/migrate-to-vault", "sec-admins",
		`{"vaultPath":"secret/data/x#f"}`); rec.Code != http.StatusNotFound {
		t.Errorf("migrate out-of-scope = %d, want 404", rec.Code)
	}
	// Tag an out-of-scope secret → 404.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-secret-tags/"+stagingID, "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusNotFound {
		t.Errorf("tag out-of-scope = %d, want 404", rec.Code)
	}
}

// TestEnvSecretGlobalWriteBlockedForRestricted is the H3 regression: a
// scope-restricted manager can READ a GLOBAL secret's metadata (P1.7) but must not
// DELETE, re-scope (capture), migrate, or label it — a global secret injects into
// every scope, so mutating it is beyond a restricted actor's reach. Unlike the
// out-of-scope cases (404, no existence oracle), the global case returns 403:
// global existence is not scope-secret.
func TestEnvSecretGlobalWriteBlockedForRestricted(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	globalID := secretID(t, pool, "SEC_GLOBAL")

	// Delete a global secret → 403 (would fail-closed every scope's runs that bind it).
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-secrets/"+globalID, "sec-admins", ""); rec.Code != http.StatusForbidden {
		t.Errorf("delete global by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Re-scope (capture) a global secret into the actor's own scope → 403.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-secrets/"+globalID, "sec-admins",
		`{"key":"SEC_GLOBAL","source":"stored","scope":"prod","value":"x"}`); rec.Code != http.StatusForbidden {
		t.Errorf("capture global→prod by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Migrate a global secret's storage → 403.
	if rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets/"+globalID+"/migrate-to-vault", "sec-admins",
		`{"vaultPath":"secret/data/x#f"}`); rec.Code != http.StatusForbidden {
		t.Errorf("migrate global by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// Label a global secret → 403.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-secret-tags/"+globalID, "sec-admins",
		`{"tags":["x"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("tag global by restricted manager = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}

	// The global secret still exists — none of the refused writes took effect.
	var count int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM secrets WHERE id=? AND scope IS NULL`, globalID).Scan(&count); err != nil {
		t.Fatalf("count global secret: %v", err)
	}
	if count != 1 {
		t.Errorf("global secret was mutated despite refusals: count=%d", count)
	}
}

// TestEnvSecretGlobalWriteAllowedForUnrestricted proves the H3 guard is scoped to
// RESTRICTED actors: an unrestricted admin still manages a global secret normally.
func TestEnvSecretGlobalWriteAllowedForUnrestricted(t *testing.T) {
	h, pool := secretRBACServer(t, nil) // no restrictions → unrestricted admin
	globalID := secretID(t, pool, "SEC_GLOBAL")

	// Label a global secret → 200.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/env-secret-tags/"+globalID, "sec-admins",
		`{"tags":["ok"]}`); rec.Code != http.StatusOK {
		t.Errorf("tag global by unrestricted admin = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	// Delete a global secret → 204.
	if rec := reqAs(t, h, http.MethodDelete, "/api/v1/env-secrets/"+globalID, "sec-admins", ""); rec.Code != http.StatusNoContent {
		t.Errorf("delete global by unrestricted admin = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
}
