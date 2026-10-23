package settings

import (
	"context"
	"testing"
)

// 2.5.1 SG-12: a secret or SSH key that changes hands is written in its own
// category, once in the agency it left and once in the one it reached.
func TestAMoveIsWrittenOnBothSides(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	if err := SetAgencyMembership(ctx, pool, MemberSecret, []AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "root@x"); err != nil {
		t.Fatalf("give to DSS: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberSecret, []AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-nwd"}}}, "root@x"); err != nil {
		t.Fatalf("move to NWD: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberSSHCredential, []AgencyMembership{{ID: "k1", AgencyIDs: []string{"ag-nwd"}}}, "root@x"); err != nil {
		t.Fatalf("give the key to NWD: %v", err)
	}
	count := func(agency, category, details string) int {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action='moved' AND agency_id=? AND category=? AND details=?`,
			agency, category, details).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, c := range []struct{ agency, category, details string }{
		{"global", "Secrets", "id=s1 to=ag-dss"},
		{"ag-dss", "Secrets", "id=s1 from=global"},
		{"ag-dss", "Secrets", "id=s1 to=ag-nwd"},
		{"ag-nwd", "Secrets", "id=s1 from=ag-dss"},
		{"global", "SSH Keys", "id=k1 to=ag-nwd"},
		{"ag-nwd", "SSH Keys", "id=k1 from=global"},
	} {
		if n := count(c.agency, c.category, c.details); n != 1 {
			t.Errorf("%+v: %d row(s), want 1", c, n)
		}
	}
	// A save that changes no owner writes no moved row.
	if err := SetAgencyMembership(ctx, pool, MemberSecret, []AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-nwd"}}}, "root@x"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action='moved' AND category='Secrets'`).Scan(&n)
	if n != 4 {
		t.Errorf("%d secret moved rows after a no-op save, want 4", n)
	}
}
