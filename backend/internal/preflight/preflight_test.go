package preflight_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/preflight"
)

func seeded(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "pf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:FIN','Finance','t'),('ag:TAX','Tax','t')`,
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES
		   ('g1','fin-admins','admin','ag:FIN',0,'t'),
		   ('g2','fin-viewers','viewer','ag:FIN',0,'t'),
		   ('g3','tax-approvers','approver','ag:TAX',0,'t')`,
		`INSERT INTO recent_logins (email, display_name, groups, first_seen_at, last_login_at)
		   VALUES ('fin@example.com','Fin','["fin-admins"]','t','t')`,
		`INSERT INTO service_accounts (id, name, token_hash, role, agency_id, all_scopes, created_by, created_at) VALUES
		   ('s1','fin-bot','h1','operator','ag:FIN',0,'fin@example.com','t'),
		   ('s2','tax-bot','h2','operator','ag:TAX',0,'fin@example.com','t'),
		   ('s3','all-bot','h3','admin',NULL,1,'fin@example.com','t')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	return pool
}

// An installation run by agency-scoped admins: nobody is a global
// administrator, and the report must say so and name who loses what.
func TestReportForAnInstallationWithNoGlobalAdmin(t *testing.T) {
	pool := seeded(t)
	rep, err := preflight.Build(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasGlobalAdmin() {
		t.Errorf("an admin of one agency is not a global administrator: %v", rep.GlobalAdminGroups)
	}
	// The viewer loses nothing and must not be listed; the admin and the
	// approver each lose something.
	got := map[string]int{}
	for _, g := range rep.AgencyGrants {
		got[g.ADGroup] = len(g.Loses)
	}
	if got["fin-admins"] == 0 || got["tax-approvers"] == 0 {
		t.Errorf("fin-admins and tax-approvers both lose abilities, got %v", got)
	}
	if _, listed := got["fin-viewers"]; listed {
		t.Errorf("a viewer loses nothing and must not be listed")
	}
	// fin-bot is the creator's own agency and a role they hold; the other two
	// could not be minted under GC-5.
	flagged := map[string]bool{}
	for _, s := range rep.ServiceAccounts {
		flagged[s.Name] = true
	}
	if flagged["fin-bot"] || !flagged["tax-bot"] || !flagged["all-bot"] {
		t.Errorf("service accounts flagged = %v, want tax-bot and all-bot only", flagged)
	}

	var b strings.Builder
	rep.Write(&b)
	for _, want := range []string{"NO GLOBAL ADMINISTRATOR", "cronomicon grant-admin", "fin-admins", "tax-bot", "every agency"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report text is missing %q:\n%s", want, b.String())
		}
	}
}

// With an all-agencies admin the report is not an alarm, and a creator who is
// one may mint anything.
func TestReportWithAGlobalAdmin(t *testing.T) {
	pool := seeded(t)
	for _, q := range []string{
		`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g9','root','admin',NULL,1,'t')`,
		`UPDATE recent_logins SET groups = '["root"]' WHERE email = 'fin@example.com'`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := preflight.Build(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.HasGlobalAdmin() || len(rep.GlobalAdminGroups) != 1 || rep.GlobalAdminGroups[0] != "root" {
		t.Errorf("global admins = %v, want [root]", rep.GlobalAdminGroups)
	}
	if len(rep.ServiceAccounts) != 0 {
		t.Errorf("a global administrator may mint any account, got %v", rep.ServiceAccounts)
	}
	if rep.Quiet() {
		t.Error("agency-scoped grants still lose abilities, so the report is not quiet")
	}
}

// A viewer on every agency is not a global administrator, however far it reads.
func TestAnAllAgenciesViewerIsNotAGlobalAdmin(t *testing.T) {
	pool := seeded(t)
	if _, err := pool.Exec(`INSERT INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_at) VALUES ('g9','watchers','viewer',NULL,1,'t')`); err != nil {
		t.Fatal(err)
	}
	rep, err := preflight.Build(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HasGlobalAdmin() {
		t.Errorf("an all-agencies viewer counted as a global administrator: %v", rep.GlobalAdminGroups)
	}
}
