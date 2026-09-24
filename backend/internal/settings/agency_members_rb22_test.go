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

	// Shrink to two. Only the difference is removed.
	delta, err = SetAgencyMembers(ctx, pool, "ag-dss", refs(
		[2]string{"secret", "s1"}, [2]string{"runner", "r1"},
	), "t@example.com")
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if len(delta.Added) != 0 || len(delta.Removed) != 3 {
		t.Fatalf("delta = +%d/-%d, want +0/-3", len(delta.Added), len(delta.Removed))
	}
}

// TestSetAgencyMembersIsolation is the reason the endpoint exists. An entity in
// TWO agencies is saved out of one; its membership in the other must be
// untouched. Under a read-modify-write through the entity-centric setter this is
// exactly the row a concurrent editor loses.
func TestSetAgencyMembersIsolation(t *testing.T) {
	pool := rb22DB(t)
	ctx := context.Background()

	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss", "ag-nwd"}}}, "t"); err != nil {
		t.Fatalf("seed membership: %v", err)
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
	_ = pool.QueryRow(`SELECT COUNT(*) FROM secret_agencies`).Scan(&n)
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
