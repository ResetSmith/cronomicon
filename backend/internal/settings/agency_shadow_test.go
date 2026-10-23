package settings

import (
	"context"
	"database/sql"
	"github.com/ResetSmith/cronomicon/internal/db"
	"path/filepath"
	"testing"
)

// TestShadowFindingsFlagsTheUnmemberedScopedRow is RA-10's core case, and the exact
// row of §2.5's table that nothing warned about: a scoped row with NO membership
// sitting on top of a global row of the same key. Resolution prefers the
// scope-exact row, and an empty membership set means "no restriction", so EVERY
// department's runs in that scope silently get this row instead of the shared one.
func TestShadowFindingsFlagsTheUnmemberedScopedRow(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	// preflightDB already seeds the global secret TOKEN. Add the shadowing row.
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-prod','TOKEN','prod','stored',?)`, now)

	got, err := ShadowFindings(context.Background(), pool)
	if err != nil {
		t.Fatalf("ShadowFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 shadow finding, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Kind != "secret" || f.Name != "TOKEN" || f.Scope != "prod" {
		t.Errorf("unexpected finding: %+v", f)
	}
	if !f.Unrestricted {
		t.Error("a scoped row with no membership must be flagged unrestricted — that is the severe case")
	}
	if f.Reference != "CRONOMICON_SECRET_TOKEN" {
		t.Errorf("reference = %q", f.Reference)
	}
	if f.Reason == "" {
		t.Error("a finding with no reason is not actionable")
	}
}

// TestShadowFindingsMembershipDowngradesTheWarning — a scoped row that DOES belong
// to a department is still a shadow (the operator should be able to see the
// override exists) but it is not the silent hole: it wins only for that
// department's runs. The two must be distinguishable, or the warning is noise and
// gets ignored in exactly the catalogue where it matters.
func TestShadowFindingsMembershipDowngradesTheWarning(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-prod','TOKEN','prod','stored',?)`, now)
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-prod','ag-dss')`)

	got, err := ShadowFindings(context.Background(), pool)
	if err != nil {
		t.Fatalf("ShadowFindings: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected the shadow to still be REPORTED, got %d: %+v", len(got), got)
	}
	if got[0].Unrestricted {
		t.Error("a membered scoped row must not be flagged unrestricted")
	}
	if len(got[0].Agencies) != 1 || got[0].Agencies[0] != "DSS" {
		t.Errorf("agencies = %v, want [DSS]", got[0].Agencies)
	}
}

// TestShadowFindingsIgnoresNonShadows fences the cheap-exit cases. A scoped row
// with no global twin overrides nothing; a global row alone shadows nothing; and
// kinds never shadow each other — a secret and a variable of the same name resolve
// through different tables under different derived prefixes, so reporting a
// cross-kind "shadow" would describe a system that does not exist.
func TestShadowFindingsIgnoresNonShadows(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	// Scoped-only: no global TOKEN_PROD exists, so nothing is shadowed.
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-only','TOKEN_PROD','prod','stored',?)`, now)
	// Cross-kind: a scoped VARIABLE named TOKEN against the seeded global SECRET.
	exec(`INSERT INTO env_vars(id, key, value, scope, created_at) VALUES('v-prod','TOKEN','x','prod',?)`, now)

	got, err := ShadowFindings(context.Background(), pool)
	if err != nil {
		t.Fatalf("ShadowFindings: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no findings, got %+v", got)
	}
}

// TestShadowFindingsCoversVariablesToo — variables go through the identical
// scope-exact-beats-global tier, and "it works for secrets" is exactly the
// assumption that let variables ship ungated in v0.56.6 (RF-3).
func TestShadowFindingsCoversVariablesToo(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO env_vars(id, key, value, created_at) VALUES('v-global','REGION','us-east',?)`, now)
	exec(`INSERT INTO env_vars(id, key, value, scope, created_at) VALUES('v-prod','REGION','eu-west','prod',?)`, now)

	got, err := ShadowFindings(context.Background(), pool)
	if err != nil {
		t.Fatalf("ShadowFindings: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "var" || got[0].Reference != "CRONOMICON_VAR_REGION" {
		t.Fatalf("variable shadow not detected: %+v", got)
	}
	if !got[0].Unrestricted {
		t.Error("an unmembered scoped variable is the same silent hole as an unmembered secret")
	}
}

// TestAmbiguitiesReportsMultiOwnerNames is RA-18's authoring-time warning: the
// operator must meet a cross-department ambiguity in a REPORT, not as a run that
// refuses to start. The likeliest victim is an unrestricted admin's ad-hoc run,
// since restricted users rarely carry multi-department snapshots — which is exactly
// the person who will file the refusal as a bug.
func TestAmbiguitiesReportsMultiOwnerNames(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	// Two departments, same key, same scope — the Phase E model working.
	exec(`INSERT INTO secrets(id,key,scope,source,owner_agency,created_at)
	      VALUES('s-a','BECOME_PASSWORD','prod','vault','ag-dss',?)`, now)
	exec(`INSERT INTO secrets(id,key,scope,source,owner_agency,created_at)
	      VALUES('s-b','BECOME_PASSWORD','prod','vault','ag-nwd',?)`, now)
	// One department alongside a SHARED row is NOT ambiguous — owned beats shared,
	// so it resolves cleanly and flagging it would bury the real finding in noise.
	exec(`INSERT INTO secrets(id,key,scope,source,created_at) VALUES('s-g','TOKEN','prod','vault',?)`, now)
	exec(`INSERT INTO secrets(id,key,scope,source,owner_agency,created_at)
	      VALUES('s-o','TOKEN','prod','vault','ag-dss',?)`, now)

	got, err := Ambiguities(context.Background(), pool)
	if err != nil {
		t.Fatalf("Ambiguities: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 ambiguity, got %d: %+v", len(got), got)
	}
	f := got[0]
	if f.Name != "BECOME_PASSWORD" || f.Scope != "prod" || f.Kind != "secret" {
		t.Errorf("unexpected finding: %+v", f)
	}
	if len(f.Owners) != 2 || f.Owners[0] != "DSS" || f.Owners[1] != "NWD" {
		t.Errorf("owners = %v, want sorted [DSS NWD]", f.Owners)
	}
	if f.Reason == "" {
		t.Error("a finding with no reason is not actionable")
	}
}

// TestAmbiguitiesCoversKeyLabels — RA-19. Labels have no scope dimension, so two
// departments owning one label collide outright with nothing to separate them,
// making this the easiest of the three kinds to hit.
func TestAmbiguitiesCoversKeyLabels(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO ssh_credentials(id,label,source,owner_agency,created_at,last_modified_at)
	      VALUES('k-a','deploy_key','stored','ag-dss',?,?)`, now, now)
	exec(`INSERT INTO ssh_credentials(id,label,source,owner_agency,created_at,last_modified_at)
	      VALUES('k-b','deploy_key','stored','ag-nwd',?,?)`, now, now)

	got, err := Ambiguities(context.Background(), pool)
	if err != nil {
		t.Fatalf("Ambiguities: %v", err)
	}
	if len(got) != 1 || got[0].Kind != "key" || got[0].Reference != "CRONOMICON_KEY_deploy_key" {
		t.Fatalf("key ambiguity not reported: %+v", got)
	}
	if got[0].Scope != "" {
		t.Errorf("keys carry no scope; got %q", got[0].Scope)
	}
}

// TestAmbiguitiesEmptyOnASharedCatalogue — the healthy state for every installation
// that has not adopted ownership. An all-unowned catalogue reports nothing.
func TestAmbiguitiesEmptyOnASharedCatalogue(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO secrets(id,key,scope,source,created_at) VALUES('s-p','TOKEN','prod','vault',?)`, now)
	got, err := Ambiguities(context.Background(), pool)
	if err != nil {
		t.Fatalf("Ambiguities: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected no ambiguities on an unowned catalogue, got %+v", got)
	}
}

// preflightDB is the two-department fixture these reports are tested on. (It is
// named for the agency preflight report it was written for, removed in v2.3.0.)
func preflightDB(t *testing.T) (*sql.DB, func(string, ...any)) {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "preflight.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
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
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-dss','DSS',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-nwd','NWD',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-prod','prod','cronomicon',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-dev','dev','cronomicon',?)`, now)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc-prod','ag-dss')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc-dev','ag-nwd')`)
	exec(`INSERT INTO jobs(source, name, run_type, command, scope, synced_at) VALUES('cronomicon','prod-job','bash','true','prod',?)`, now)
	exec(`INSERT INTO jobs(source, name, run_type, command, scope, synced_at) VALUES('cronomicon','dev-job','bash','true','dev',?)`, now)
	exec(`INSERT INTO secrets(id, key, source, created_at) VALUES('s-global','TOKEN','stored',?)`, now)
	exec(`INSERT INTO ssh_credentials(id, label, source, created_at, last_modified_at) VALUES('k1','deploy_key','stored',?,?)`, now, now)
	bind := func(job, kind, name string) {
		exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at)
		      VALUES('job','cronomicon',?,?,?,'seed',?)`, job, kind, name, now)
	}
	bind("prod-job", "secret", "TOKEN")
	bind("dev-job", "secret", "TOKEN")
	bind("prod-job", "key", "deploy_key")
	bind("dev-job", "key", "deploy_key")
	return pool, exec
}
