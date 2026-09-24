package settings

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// The T2.12 pre-flight report. Its whole job is to be trustworthy BEFORE Phase 3
// flips the predicates, so the properties under test are: it is empty when the
// tightening has no teeth, it flags exactly the bindings that would break, and it
// never guesses at the cases it cannot evaluate.

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
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-prod','prod','amadeus',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-dev','dev','amadeus',?)`, now)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc-prod','ag-dss')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc-dev','ag-nwd')`)
	exec(`INSERT INTO jobs(source, name, run_type, command, scope, synced_at) VALUES('amadeus','prod-job','bash','true','prod',?)`, now)
	exec(`INSERT INTO jobs(source, name, run_type, command, scope, synced_at) VALUES('amadeus','dev-job','bash','true','dev',?)`, now)
	exec(`INSERT INTO secrets(id, key, source, created_at) VALUES('s-global','TOKEN','stored',?)`, now)
	exec(`INSERT INTO ssh_credentials(id, label, source, created_at, last_modified_at) VALUES('k1','deploy_key','stored',?,?)`, now, now)
	bind := func(job, kind, name string) {
		exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at)
		      VALUES('job','amadeus',?,?,?,'seed',?)`, job, kind, name, now)
	}
	bind("prod-job", "secret", "TOKEN")
	bind("dev-job", "secret", "TOKEN")
	bind("prod-job", "key", "deploy_key")
	bind("dev-job", "key", "deploy_key")
	return pool, exec
}

// TestPreflightEmptyOnFreshMigration is the property that makes migration 670 safe
// to deploy: the backfill assigns no membership that narrows anything, so the
// report has nothing to flag. membershipAssigned is what tells the reader that an
// empty findings list means "no teeth yet", not "Phase 3 is safe".
func TestPreflightEmptyOnFreshMigration(t *testing.T) {
	pool, _ := preflightDB(t)
	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rep.ReferenceFindings) != 0 || len(rep.KeyFindings) != 0 {
		t.Fatalf("fresh database produced findings: %+v / %+v", rep.ReferenceFindings, rep.KeyFindings)
	}
	if rep.JobBindingsChecked != 4 {
		t.Errorf("jobBindingsChecked = %d, want 4 — an empty report over no data is not the same answer", rep.JobBindingsChecked)
	}
	// Only the two scope rows are assigned; no secret/var/key membership exists.
	if rep.MembershipAssigned != 2 {
		t.Errorf("membershipAssigned = %d, want 2", rep.MembershipAssigned)
	}
}

// TestPreflightFlagsAGQ1 — once a GLOBAL secret is given membership, every job
// outside that agency loses it. This is the AG-Q1(b) half.
func TestPreflightFlagsAGQ1(t *testing.T) {
	pool, exec := preflightDB(t)
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-global','ag-dss')`)

	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rep.ReferenceFindings) != 1 {
		t.Fatalf("referenceFindings = %+v, want exactly the dev-job binding", rep.ReferenceFindings)
	}
	f := rep.ReferenceFindings[0]
	if f.JobName != "dev-job" {
		t.Errorf("flagged %q; prod-job is IN ag-dss and must not be flagged", f.JobName)
	}
	if f.Reference != "CRONOMICON_SECRET_TOKEN" {
		t.Errorf("reference = %q", f.Reference)
	}
	if len(f.RowAgencies) != 1 || f.RowAgencies[0] != "DSS" {
		t.Errorf("rowAgencies = %v, want [DSS] (NAMES, not ids — an operator reads this)", f.RowAgencies)
	}
	if len(f.JobAgencies) != 1 || f.JobAgencies[0] != "NWD" {
		t.Errorf("jobAgencies = %v, want [NWD]", f.JobAgencies)
	}
	if f.Reason == "" {
		t.Error("finding has no reason")
	}
	// The key bindings are untouched by AG-Q1(b) and must stay out of this half.
	if len(rep.KeyFindings) != 0 {
		t.Errorf("keyFindings = %+v, want none — the key has no membership yet", rep.KeyFindings)
	}
}

// TestPreflightFlagsAGQ5 — the SSH-key tightening, reported SEPARATELY so it can be
// accepted or rejected on its own. §7.1 calls this out as the change that breaks
// bindings which work today.
func TestPreflightFlagsAGQ5(t *testing.T) {
	pool, exec := preflightDB(t)
	exec(`INSERT INTO ssh_credential_agencies(credential_id, agency_id) VALUES('k1','ag-dss')`)

	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rep.KeyFindings) != 1 || rep.KeyFindings[0].JobName != "dev-job" {
		t.Fatalf("keyFindings = %+v, want exactly the dev-job key binding", rep.KeyFindings)
	}
	if rep.KeyFindings[0].Reference != "CRONOMICON_KEY_deploy_key" {
		t.Errorf("reference = %q", rep.KeyFindings[0].Reference)
	}
	if len(rep.ReferenceFindings) != 0 {
		t.Errorf("the key tightening leaked into referenceFindings: %+v", rep.ReferenceFindings)
	}
}

// TestPreflightGeneralPoolJobIntersectsNothing — a job whose scope belongs to no
// agency has an empty set, which intersects nothing. That is a real finding, and
// the reason has to say so rather than printing an empty list at the operator.
func TestPreflightGeneralPoolJobIntersectsNothing(t *testing.T) {
	pool, exec := preflightDB(t)
	exec(`DELETE FROM scope_agencies WHERE scope_id='sc-dev'`)
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-global','ag-dss')`)

	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(rep.ReferenceFindings) != 1 {
		t.Fatalf("want one finding, got %+v", rep.ReferenceFindings)
	}
	f := rep.ReferenceFindings[0]
	if len(f.JobAgencies) != 0 {
		t.Errorf("jobAgencies = %v, want empty", f.JobAgencies)
	}
	if f.Reason == "" || f.Reason[0:3] != "the" {
		t.Errorf("reason = %q, want a sentence naming the empty-intersection cause", f.Reason)
	}
}

// TestPreflightCountsScriptBindingsRatherThanGuessing — a script has no scope of
// its own, so its bindings have no static answer. Reporting a guess would be worse
// than reporting the count.
func TestPreflightCountsScriptBindingsRatherThanGuessing(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO scripts(name, run_type, script, content_hash, synced_at) VALUES('deploy','bash','true','sha256:x',?)`, now)
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at)
	      VALUES('script','','deploy','secret','TOKEN','seed',?)`, now)
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-global','ag-dss')`)

	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if rep.ScriptBindingsUnevaluated != 1 {
		t.Errorf("scriptBindingsUnevaluated = %d, want 1", rep.ScriptBindingsUnevaluated)
	}
	for _, f := range rep.ReferenceFindings {
		if f.JobName == "deploy" {
			t.Errorf("a script binding was reported as a job finding: %+v", f)
		}
	}
}

// TestPreflightHonorsScopeExactRow — the report must resolve a reference through
// the SAME scope predicate dispatch uses, or it describes a system that does not
// exist. Here a prod-specific row shadows the global one for prod-job only.
func TestPreflightHonorsScopeExactRow(t *testing.T) {
	pool, exec := preflightDB(t)
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-prod','TOKEN','prod','stored',?)`, now)
	// The prod-specific row is in NWD — the WRONG agency for prod-job, whose scope is
	// in DSS. The global row stays unrestricted.
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-prod','ag-nwd')`)

	rep, err := AgencyPreflight(context.Background(), pool)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	// prod-job resolves TOKEN to the scope-exact prod row (in NWD) and is flagged;
	// dev-job falls back to the unrestricted global row and is not. A report that
	// ignored the scope predicate would get both of these backwards.
	if len(rep.ReferenceFindings) != 1 || rep.ReferenceFindings[0].JobName != "prod-job" {
		t.Fatalf("findings = %+v, want exactly prod-job (which resolves to the scope-exact row)", rep.ReferenceFindings)
	}
}
