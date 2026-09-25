package settings

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// rbacFixture builds a database with a deliberately awkward access configuration:
// a group nobody granted, a group granted in the WRONG CASE, an unscoped job with
// a schedule and a parked run, and an unmembered secret. Every surviving RB-4
// section has something to find.
//
// v0.57.8 (RB-19): the fixture no longer writes ad_group_mappings or
// scope_restrictions — both tables are dropped. Access comes from access_grants,
// which is what login has actually read since v0.56.5.
func rbacFixture(t *testing.T) *sql.DB {
	t.Helper()
	database := newTestDB(t)
	ctx := context.Background()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("fixture %q: %v", q, err)
		}
	}

	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES
		('s-tax','tax','cronomicon','2026-01-01T00:00:00Z'),
		('s-audit','tax-audit','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('a-tax','Tax','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s-tax','a-tax'),('s-audit','a-tax')`)

	// Grants. g-typo is stored in a DIFFERENT CASE from the group Erin actually
	// presents: login matches ad_group exactly, so this grant silently confers
	// nothing — the case the report exists to surface.
	exec(`INSERT INTO access_grants (id,ad_group,role,agency_id,all_scopes,created_by,created_at) VALUES
		('g1','sg-ops','operator','a-tax',0,'admin','2026-01-01T00:00:00Z'),
		('g2','sg-viewers','viewer','a-tax',0,'admin','2026-01-01T00:00:00Z'),
		('g-typo','sg-typo','operator','a-tax',0,'admin','2026-01-01T00:00:00Z')`)

	// Users:
	//   Alice — granted via sg-ops.
	//   Carol — granted via sg-viewers; creates the parked run below.
	//   Dan   — a group named by no grant at all.
	//   Erin  — a group whose grant exists but in the WRONG CASE, so she resolves
	//           to nothing at login. A case-folding report would hide both her and
	//           the broken grant, which is why she is in the fixture.
	exec(`INSERT INTO recent_logins (email,display_name,groups,first_seen_at,last_login_at) VALUES
		('alice@example.com','Alice','["sg-ops"]','2026-01-01T00:00:00Z','2026-08-01T10:00:00Z'),
		('carol@example.com','Carol','["sg-viewers"]','2026-01-01T00:00:00Z','2026-08-02T10:00:00Z'),
		('dan@example.com','Dan','["sg-nowhere"]','2026-01-01T00:00:00Z','2026-08-03T10:00:00Z'),
		('erin@example.com','Erin','["SG-Typo"]','2026-01-01T00:00:00Z','2026-08-03T11:00:00Z')`)

	// Jobs: one scoped, one unscoped. The unscoped one has a schedule and a parked
	// run created by Carol, whose viewer role does not carry triggerJobs.
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled) VALUES
		('scoped-job','git','bash','tax',1),
		('restart-service','git','bash',NULL,1)`)
	exec(`INSERT INTO definition_schedules (owner_kind,owner_source,owner_name,name,cron,position)
		VALUES ('job','git','restart-service','nightly','0 2 * * *',0)`)
	exec(`INSERT INTO pending_runs (id,kind,name,source,scope,run_at,scheduled_by,created_at,status)
		VALUES ('p1','job','restart-service','git',NULL,'2026-09-01T17:00:00Z','carol@example.com','2026-08-02T10:00:00Z','pending')`)

	// A secret with no agency membership (RB-Q14) and one with membership.
	exec(`INSERT INTO secrets (id,key,scope,source,created_at,last_modified_by) VALUES
		('sec-1','GLOBAL_TOKEN',NULL,'stored','2026-01-01T00:00:00Z','admin@example.com'),
		('sec-2','TAX_TOKEN','tax','stored','2026-01-01T00:00:00Z','admin@example.com')`)
	exec(`INSERT INTO secret_agencies (secret_id,agency_id) VALUES ('sec-2','a-tax')`)

	return database
}

// newTestDB opens an in-memory database with the full migration set applied.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return database
}

func TestRbacPreflightFindsEverySection(t *testing.T) {
	database := rbacFixture(t)
	rep, err := RbacPreflight(context.Background(), database)
	if err != nil {
		t.Fatalf("RbacPreflight: %v", err)
	}

	// ── Denominators: an empty report over no data must be distinguishable ──
	if rep.UsersEvaluated != 4 {
		t.Errorf("UsersEvaluated = %d, want 4", rep.UsersEvaluated)
	}
	if rep.GrantsEvaluated != 3 {
		t.Errorf("GrantsEvaluated = %d, want 3 — the denominator that replaced "+
			"MappingsEvaluated when the legacy tables were dropped", rep.GrantsEvaluated)
	}

	// ── Ungranted groups, INCLUDING the case-mismatched grant ──
	// sg-typo is granted, but Erin presents SG-Typo. Login matches ad_group
	// exactly, so that grant confers nothing and the group is effectively
	// ungranted. A report that folded case would credit Erin with operator and
	// hide the broken grant — the exact double failure this assertion prevents.
	want := []string{"SG-Typo", "sg-nowhere"}
	if len(rep.UngrantedGroups) != 2 ||
		rep.UngrantedGroups[0] != want[0] || rep.UngrantedGroups[1] != want[1] {
		t.Errorf("UngrantedGroups = %v, want %v — a case-mismatched grant confers nothing "+
			"at login and must be surfaced, not silently resolved", rep.UngrantedGroups, want)
	}
	// The granted groups must NOT appear: a report that flags working config is
	// noise, and noise is how the real finding gets skipped.
	for _, g := range rep.UngrantedGroups {
		if g == "sg-ops" || g == "sg-viewers" {
			t.Errorf("UngrantedGroups included the granted group %q", g)
		}
	}

	// ── RB-Q14: the unmembered secret, and only that one ──
	if len(rep.EmptyMembershipEntities) != 1 {
		t.Fatalf("EmptyMembershipEntities = %+v, want exactly the unmembered secret", rep.EmptyMembershipEntities)
	}
	if rep.EmptyMembershipEntities[0].Key != "GLOBAL_TOKEN" {
		t.Errorf("EmptyMembershipEntities[0].Key = %q, want GLOBAL_TOKEN",
			rep.EmptyMembershipEntities[0].Key)
	}

	// ── RB-26: the unscoped job, its share, its schedule, its parked run ──
	if len(rep.UnscopedJobs) != 1 || rep.UnscopedJobs[0].Name != "restart-service" {
		t.Fatalf("UnscopedJobs = %+v, want just restart-service", rep.UnscopedJobs)
	}
	if rep.TotalJobs != 2 {
		t.Errorf("TotalJobs = %d, want 2", rep.TotalJobs)
	}
	if rep.UnscopedJobShare != 50 {
		t.Errorf("UnscopedJobShare = %d, want 50", rep.UnscopedJobShare)
	}
	if len(rep.UnscopedSchedules) != 1 {
		t.Errorf("UnscopedSchedules = %+v, want the nightly entry on restart-service", rep.UnscopedSchedules)
	}
	if len(rep.PendingUnbound) != 1 {
		t.Errorf("PendingUnbound = %+v, want the parked run with no frozen scope", rep.PendingUnbound)
	}
	// Carol created the parked run and her viewer grant does not carry triggerJobs
	// — RB-Q12 fires it anyway, which is why it must appear on the hand-sweep list.
	if len(rep.PendingRevoked) != 1 {
		t.Fatalf("PendingRevoked = %+v, want the run created by a user without the verb",
			rep.PendingRevoked)
	}
	if !strings.Contains(rep.PendingRevoked[0].Detail, "carol@example.com") {
		t.Errorf("PendingRevoked detail must name the creator; got %q", rep.PendingRevoked[0].Detail)
	}
}

// TestRbacPreflightResolvesCreatorFromGrants pins the half of PendingRevoked that
// moved off the legacy tables: the creator's verb is decided by access_grants, so
// a parked run by someone WITH the verb must not be flagged.
func TestRbacPreflightResolvesCreatorFromGrants(t *testing.T) {
	database := rbacFixture(t)
	ctx := context.Background()

	// Alice holds operator (triggerJobs) via a grant. Her parked run is bound to a
	// scope, so it is neither unbound nor revoked.
	if _, err := database.ExecContext(ctx,
		`INSERT INTO pending_runs (id,kind,name,source,scope,run_at,scheduled_by,created_at,status)
		 VALUES ('p2','job','scoped-job','git','tax','2026-09-02T17:00:00Z','alice@example.com','2026-08-02T10:00:00Z','pending')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rep, err := RbacPreflight(ctx, database)
	if err != nil {
		t.Fatalf("RbacPreflight: %v", err)
	}
	for _, f := range rep.PendingRevoked {
		if strings.Contains(f.Detail, "alice@example.com") {
			t.Errorf("a run created by a user who HOLDS triggerJobs via a grant was flagged "+
				"as revoked: %+v — creator verbs must resolve from access_grants", f)
		}
	}
	for _, f := range rep.PendingUnbound {
		if strings.Contains(f.Detail, "alice@example.com") {
			t.Errorf("a scope-bound parked run was reported unbound: %+v", f)
		}
	}
}

// TestRbacPreflightEmptyOnFreshDatabase pins the documented "empty is expected"
// behavior: no panic, no nil slices (which would serialize as JSON null and break
// a `.map()` in the SPA), and zeroed denominators.
func TestRbacPreflightEmptyOnFreshDatabase(t *testing.T) {
	rep, err := RbacPreflight(context.Background(), newTestDB(t))
	if err != nil {
		t.Fatalf("RbacPreflight on a fresh database: %v", err)
	}
	if rep.UsersEvaluated != 0 || rep.GrantsEvaluated != 0 {
		t.Errorf("fresh database reported %d users / %d grants, want 0/0",
			rep.UsersEvaluated, rep.GrantsEvaluated)
	}
	if rep.UnscopedJobShare != 0 {
		t.Errorf("UnscopedJobShare = %d on an empty catalog, want 0 (no divide-by-zero)", rep.UnscopedJobShare)
	}
	// Every slice must be non-nil so the JSON is [] rather than null.
	if rep.UngrantedGroups == nil || rep.UnscopedJobs == nil || rep.UnscopedSchedules == nil ||
		rep.PendingUnbound == nil || rep.PendingRevoked == nil || rep.EmptyMembershipEntities == nil {
		t.Error("a report slice was nil; it must serialize as [] so the SPA can map over it")
	}
}
