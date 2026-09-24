package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Authorization-denial auditing (LU-9).
//
// A 403 is the only externally-visible trace of someone reaching for a resource
// they were not granted. Before LU-9 nothing recorded them, so an actor could
// walk every scope in the system and leave behind exactly zero evidence. These
// tests pin the three properties that make the trail worth having:
//
//  1. A denial is recorded with enough structure to query — who, what reason,
//     which scope/permission — and the operator-facing response is UNCHANGED.
//  2. Denials are not collapsed, so a walk shows up as a walk.
//  3. Row-level filters do NOT write rows, so the trail is not buried under
//     routine "this list omitted a row you can't see" noise.

// deniedRow is one auth_events row of kind 'denied'.
type deniedRow struct {
	Actor   string
	Reason  string
	Target  string
	Outcome string
	Details string
}

// deniedRows reads the denial trail, oldest first.
//
// It filters on kind='denied' deliberately: the trusted-header auth mode resolves
// identity per REQUEST and records login events of its own, so an unfiltered
// count would be measuring the login throttle rather than the guards under test.
func deniedRows(t *testing.T, pool *sql.DB) []deniedRow {
	t.Helper()
	rows, err := pool.Query(`SELECT COALESCE(actor,''), COALESCE(reason,''), COALESCE(target,''),
	                                COALESCE(outcome,''), COALESCE(details,'')
	                         FROM auth_events WHERE kind = 'denied' ORDER BY rowid`)
	if err != nil {
		t.Fatalf("query auth_events: %v", err)
	}
	defer rows.Close()
	var out []deniedRow
	for rows.Next() {
		var d deniedRow
		if err := rows.Scan(&d.Actor, &d.Reason, &d.Target, &d.Outcome, &d.Details); err != nil {
			t.Fatalf("scan auth_event: %v", err)
		}
		out = append(out, d)
	}
	return out
}

// errMessage extracts the operator-facing message from the canonical error envelope.
func errMessage(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	return e.Message
}

// TestScopeDenialAuditsOnceAndPreservesResponse: the denial must be BOTH recorded
// and invisible to the caller. Recording it is the point of LU-9; leaving the
// response byte-identical is what makes the change safe to land across 21 call
// sites — the SPA surfaces these specific messages, so an audit refactor that
// homogenised them would be a UI regression disguised as a security improvement.
func TestScopeDenialAuditsOnceAndPreservesResponse(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})

	// A prod-restricted manager creating a staging secret: the actor supplied the
	// scope, so this is a decision (403), not an existence question (404).
	rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
		`{"key":"NEW_STAGING","source":"stored","scope":"staging","value":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-scope secret create = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	if got, want := errMessage(t, rec.Body.Bytes()), "cannot create a secret in a scope outside your access"; got != want {
		t.Errorf("denial message = %q, want %q (the SPA surfaces this verbatim)", got, want)
	}

	rows := deniedRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("denial rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	d := rows[0]
	if d.Reason != "insufficient_scope" {
		t.Errorf("reason = %q, want insufficient_scope (the token a detection rule keys on)", d.Reason)
	}
	if d.Target != "staging" {
		t.Errorf("target = %q, want the denied scope 'staging'", d.Target)
	}
	if d.Actor != "sec-admins@example.com" {
		t.Errorf("actor = %q, want the caller's email", d.Actor)
	}
	if d.Outcome != "failure" {
		t.Errorf("outcome = %q, want failure", d.Outcome)
	}
	// Without the request line an auditor cannot tell one fat-fingered write from
	// a sweep across the API surface.
	if want := "POST /api/v1/env-secrets"; !strings.Contains(d.Details, want) {
		t.Errorf("details = %q, want it to name the request (%q)", d.Details, want)
	}
}

// TestPermissionDenialRecordsPermissionName: requirePerm takes an opaque
// predicate, so before LU-9 the permission name was erased at the call boundary
// and a denial could only say "insufficient permissions". The recorded target is
// the rolePermissions JSON name — the SAME string /capabilities reports — so a
// denial can be joined against the capability set the operator was shown.
func TestPermissionDenialRecordsPermissionName(t *testing.T) {
	h, pool := secretRBACServer(t, nil)

	// A viewer holds no ManageEnvVars; the route is gated before any handler runs.
	rec := reqAs(t, h, http.MethodPost, "/api/v1/env-vars", "sec-viewers",
		`{"key":"NOPE","value":"x"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer env-var create = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	// RB-25: the message NAMES the permission now. LU-9 deliberately kept it
	// byte-identical — that refactor's rule was that adding an audit row must not
	// change what an operator sees — but "insufficient permissions" is unactionable
	// on its own: the reader cannot tell whether to ask for a role change, a scope
	// grant, or neither. The permission has been in the audit row since LU-9; this
	// puts it in front of the one person who most needs it.
	if got := errMessage(t, rec.Body.Bytes()); !strings.Contains(got, "manageEnvVars") {
		t.Errorf("denial message = %q, want it to name the required permission", got)
	}

	rows := deniedRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("denial rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	if rows[0].Reason != "insufficient_permission" {
		t.Errorf("reason = %q, want insufficient_permission", rows[0].Reason)
	}
	if rows[0].Target != "manageEnvVars" {
		t.Errorf("target = %q, want the JSON permission name manageEnvVars", rows[0].Target)
	}
	if rows[0].Actor != "sec-viewers@example.com" {
		t.Errorf("actor = %q, want the caller's email", rows[0].Actor)
	}
}

// TestDenialsAreNotThrottled: login events ARE throttled (trusted-header mode
// mints identity per request, so an unthrottled login row per HTTP call would
// bury everything). Denials deliberately are NOT — each one is a distinct
// decision about a distinct resource, and collapsing them into one row per
// window would hide exactly the scope-enumeration pattern the trail exists to
// reveal. This test is the guard against someone "fixing" that asymmetry.
func TestDenialsAreNotThrottled(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})

	for _, scope := range []string{"staging", "qa"} {
		rec := reqAs(t, h, http.MethodPost, "/api/v1/env-secrets", "sec-admins",
			`{"key":"PROBE","source":"stored","scope":"`+scope+`","value":"x"}`)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("create in %s = %d, want 403; body: %s", scope, rec.Code, rec.Body.String())
		}
	}

	rows := deniedRows(t, pool)
	if len(rows) != 2 {
		t.Fatalf("denial rows = %d, want 2 (one per probed scope): %+v", len(rows), rows)
	}
	if rows[0].Target != "staging" || rows[1].Target != "qa" {
		t.Errorf("targets = %q,%q; want staging,qa — the walk must be reconstructable",
			rows[0].Target, rows[1].Target)
	}
}

// TestRowLevelFilterDoesNotAudit: handleListEnvVars drops rows the caller cannot
// see rather than refusing the request. That is normal operation on every single
// list request, and auditing it would write one row PER HIDDEN ROW — burying the
// real denials under noise proportional to the size of the database. Filters are
// deliberately excluded from LU-9; this pins that they stay excluded.
func TestRowLevelFilterDoesNotAudit(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"viewer": "prod"})
	seedEnvVar := func(id, key string, scope any) {
		t.Helper()
		if _, err := pool.Exec(`INSERT INTO env_vars (id, key, scope, value, created_by, created_at,
			last_modified_by, last_modified_at)
			VALUES (?, ?, ?, 'v', 'seed', '2026-01-01T00:00:00Z', 'seed', '2026-01-01T00:00:00Z')`, id, key, scope); err != nil {
			t.Fatalf("seed env var %s: %v", key, err)
		}
	}
	seedEnvVar("ev-global", "EV_GLOBAL", nil)
	seedEnvVar("ev-prod", "EV_PROD", "prod")
	seedEnvVar("ev-staging", "EV_STAGING", "staging")

	rec := reqAs(t, h, http.MethodGet, "/api/v1/env-vars", "sec-viewers", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("filtered list = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen := map[string]bool{}
	for _, ev := range got {
		seen[ev.Key] = true
	}
	// Sanity: the filter must actually have dropped something, or this test would
	// pass for the wrong reason (nothing to audit ⇒ no rows regardless).
	if !seen["EV_PROD"] || !seen["EV_GLOBAL"] {
		t.Fatalf("in-scope/global vars missing — fixture wrong: %v", seen)
	}
	if seen["EV_STAGING"] {
		t.Fatalf("out-of-scope var leaked — the filter under test did not run: %v", seen)
	}

	if rows := deniedRows(t, pool); len(rows) != 0 {
		t.Errorf("row-level filter wrote %d denial row(s), want 0: %+v", len(rows), rows)
	}
}
