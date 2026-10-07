package settings

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// RB-22 — the agency-scoped setter. Its reason to exist is write isolation: a
// SetAgencyMembers call touches only rows belonging to ITS agency, so concurrent
// administration of two departments cannot interact. These tests pin that
// property directly, plus the guards inherited from the entity-centric setter.

func rb22DB(t *testing.T) *sql.DB {
	t.Helper()
	pool := membershipDB(t)
	if _, err := pool.Exec(`INSERT INTO runners(id,name,status,capabilities,registered_at,created_at)
		VALUES('r1','runner-one','online','["bash"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	return pool
}

func refs(pairs ...[2]string) []AgencyMemberRef {
	out := make([]AgencyMemberRef, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, AgencyMemberRef{Kind: p[0], ID: p[1]})
	}
	return out
}

// TestSetAgencyMembersRoundTrip: all five kinds in one save, visible through
// BuildAgencyDetail, and a second save with fewer members removes exactly the
// difference.
func TestSetAgencyMembersRoundTrip(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	delta, err := SetAgencyMembers(ctx, pool, "ag-dss", refs(
		[2]string{"scope", "sc-prod"}, [2]string{"secret", "s1"}, [2]string{"env-var", "v1"},
		[2]string{"ssh-credential", "k1"}, [2]string{"runner", "r1"},
	), "t@example.com")
	if err != nil {
		t.Fatalf("SetAgencyMembers: %v", err)
	}
	if len(delta.Added) != 5 || len(delta.Removed) != 0 {
		t.Fatalf("delta = +%d/-%d, want +5/-0", len(delta.Added), len(delta.Removed))
	}
	d, err := BuildAgencyDetail(ctx, pool, "ag-dss")
	if err != nil || d == nil {
		t.Fatalf("BuildAgencyDetail: %v", err)
	}
	if len(d.Members) != 5 {
		t.Fatalf("members = %+v, want all five kinds", d.Members)
	}

	// A member cannot simply be dropped from its only agency. A scope would be
	// left in none (LR-26); a variable, secret or key is OWNED by its agency
	// since it joined it (LR-54), and leaves by being moved, not removed.
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs(
		[2]string{"secret", "s1"}, [2]string{"env-var", "v1"}, [2]string{"ssh-credential", "k1"}, [2]string{"runner", "r1"},
	), "t@example.com"); !errors.Is(err, ErrAgencyRequired) {
		t.Fatalf("a save that strands the scope = %v, want ErrAgencyRequired", err)
	}
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs(
		[2]string{"scope", "sc-prod"}, [2]string{"secret", "s1"}, [2]string{"ssh-credential", "k1"}, [2]string{"runner", "r1"},
	), "t@example.com"); !errors.Is(err, ErrOwnerRemoval) {
		t.Fatalf("a save that drops an owned variable = %v, want ErrOwnerRemoval", err)
	}
	for table, id := range map[string]string{"secrets": "s1", "env_vars": "v1", "ssh_credentials": "k1"} {
		if got := entityOwner(ctx, pool, table, id); got != "ag-dss" {
			t.Errorf("%s %s owner = %q after joining DSS from Global, want ag-dss", table, id, got)
		}
	}
	// Nor can they be given a SECOND agency (LR-7, LR-54): adding one that is
	// another agency's is a move, and a move is made on the row itself.
	for _, m := range [][2]string{{"scope", "sc-prod"}, {"secret", "s1"}, {"env-var", "v1"}, {"ssh-credential", "k1"}} {
		if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", refs(m), "t@example.com"); !errors.Is(err, ErrOneAgency) {
			t.Errorf("adding DSS's %s to NWD as well = %v, want ErrOneAgency", m[0], err)
		}
	}
	var second int
	_ = pool.QueryRow(`SELECT (SELECT COUNT(*) FROM scope_agencies WHERE agency_id='ag-nwd') + (SELECT COUNT(*) FROM secret_agencies WHERE agency_id='ag-nwd')
	                        + (SELECT COUNT(*) FROM env_var_agencies WHERE agency_id='ag-nwd') + (SELECT COUNT(*) FROM ssh_credential_agencies WHERE agency_id='ag-nwd')`).Scan(&second)
	if second != 0 {
		t.Fatalf("a refused add wrote %d rows into the second agency", second)
	}
	// A runner's serve list is not this rule's: it may be added to a second
	// agency here, and then removed from the first. Only the difference goes.
	if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", refs([2]string{"runner", "r1"}), "t@example.com"); err != nil {
		t.Fatalf("a runner serving a second agency: %v", err)
	}
	delta, err = SetAgencyMembers(ctx, pool, "ag-dss", refs(
		[2]string{"scope", "sc-prod"}, [2]string{"secret", "s1"}, [2]string{"env-var", "v1"}, [2]string{"ssh-credential", "k1"},
	), "t@example.com")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if len(delta.Added) != 0 || len(delta.Removed) != 1 || delta.Removed[0].Kind != "runner" {
		t.Fatalf("delta = +%d/-%d (%+v), want only the runner removed", len(delta.Added), len(delta.Removed), delta.Removed)
	}
}

// TestSetAgencyMembersIsolation is the reason the endpoint exists. An entity in
// TWO agencies is saved out of one; its membership in the other must be
// untouched. Under a read-modify-write through the entity-centric setter this is
// exactly the row a concurrent editor loses.
//
// Nothing puts a secret in two agencies since 2.3.0, but an upgraded
// installation has them: what several agencies shared is Global-owned with each
// as a member (migration 1220). Saving it out of all but one is how it is
// settled, and the one that is left becomes its owner.
func TestSetAgencyMembersIsolation(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	for _, q := range []string{
		`DELETE FROM secret_agencies WHERE secret_id = 's1'`,
		`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s1', 'ag-dss'), ('s1', 'ag-nwd')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed the pre-2.3.0 shape: %v", err)
		}
	}
	if got := entityOwner(ctx, pool, "secrets", "s1"); got != "global" {
		t.Fatalf("fixture: owner = %q, want global", got)
	}

	// Save ag-dss WITHOUT the secret: it leaves DSS.
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs(), "t@example.com"); err != nil {
		t.Fatalf("SetAgencyMembers: %v", err)
	}

	var nwd, dss int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s1' AND agency_id='ag-nwd'`).Scan(&nwd)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s1' AND agency_id='ag-dss'`).Scan(&dss)
	if dss != 0 {
		t.Error("the secret was not removed from the saved agency")
	}
	if nwd != 1 {
		t.Error("saving ONE agency disturbed the entity's membership in ANOTHER — " +
			"the isolation this endpoint exists to provide")
	}
	if got := entityOwner(ctx, pool, "secrets", "s1"); got != "ag-nwd" {
		t.Errorf("owner = %q after it was left in NWD alone, want ag-nwd: a row in one agency is that agency's", got)
	}
}

// TestSetAgencyMembersOwnerRemoval — RA-15 restated on the inverted axis: the
// owning agency cannot be edited out of its own entity's membership.
func TestSetAgencyMembersOwnerRemoval(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	if _, err := pool.Exec(`UPDATE secrets SET owner_agency='ag-dss' WHERE id='s1'`); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "t"); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	_, err := SetAgencyMembers(ctx, pool, "ag-dss", refs(), "t@example.com")
	if !errors.Is(err, ErrOwnerRemoval) {
		t.Fatalf("removing an owned entity from its owner = %v, want ErrOwnerRemoval — "+
			"a row its own department cannot reach is never what the operator meant", err)
	}
	// And nothing was written: the guard runs before the transaction.
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s1' AND agency_id='ag-dss'`).Scan(&n)
	if n != 1 {
		t.Error("the refused save still modified membership")
	}
}

// TestSetAgencyMembersValidation: unknown agency is 404-shaped, unknown entity and
// unknown kind are 422-shaped, and validation is fail-closed — no partial write.
func TestSetAgencyMembersValidation(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	if _, err := SetAgencyMembers(ctx, pool, "ag-nope", refs(), "t"); !errors.Is(err, ErrUnknownAgency) {
		t.Errorf("unknown agency = %v, want ErrUnknownAgency", err)
	}
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss",
		refs([2]string{"secret", "s1"}, [2]string{"secret", "s-nope"}), "t"); !errors.Is(err, ErrUnknownMember) {
		t.Errorf("unknown entity = %v, want ErrUnknownMember", err)
	}
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss",
		refs([2]string{"volcano", "s1"}), "t"); !errors.Is(err, ErrUnknownMember) {
		t.Errorf("unknown kind = %v, want ErrUnknownMember", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies WHERE agency_id <> 'global'`).Scan(&n)
	if n != 0 {
		t.Error("a refused batch left a partial write — validation must complete before any row moves")
	}
}

// TestSetAgencyMembersNoOpWritesNoAudit: a save that changes nothing writes no
// change_log row. An audit trail of "nothing changed" buries the rows that matter.
func TestSetAgencyMembersNoOpWritesNoAudit(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs([2]string{"secret", "s1"}), "t@example.com"); err != nil {
		t.Fatalf("first save: %v", err)
	}
	var before int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&before)

	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", refs([2]string{"secret", "s1"}), "t@example.com"); err != nil {
		t.Fatalf("no-op save: %v", err)
	}
	var after int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log`).Scan(&after)
	if after != before {
		t.Errorf("a no-op save wrote %d audit rows", after-before)
	}
}
