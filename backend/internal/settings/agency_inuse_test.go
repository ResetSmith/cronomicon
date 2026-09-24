package settings

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAgencyInUseNamesTheRealBlocker (RA-Q22, rev 7) fences the defect that made
// the delete guard misleading rather than merely vague: DeleteAgency counts FOUR
// independent reference classes, and the API rendered every one of them as
// "agency is still referenced by one or more scopes".
//
// An operator retiring a department would audit scopes — the one thing that was
// already clean — and never learn that an owned secret was the holdout. The fence
// is therefore not "an error was returned" (the old test already covered that) but
// "the error names the class that actually blocked it, and NOT the ones that did
// not".
func TestAgencyInUseNamesTheRealBlocker(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		seed    string
		want    string // must appear in the rendered blockers
		notWant string // must NOT — the misattribution this test exists to catch
	}{
		{
			name:    "owned secret",
			seed:    `UPDATE secrets SET owner_agency='ag-dss' WHERE id='s1'`,
			want:    "owned secret",
			notWant: "scope",
		},
		{
			name:    "owned variable",
			seed:    `UPDATE env_vars SET owner_agency='ag-dss' WHERE id='v1'`,
			want:    "owned variable",
			notWant: "scope",
		},
		{
			name:    "owned ssh credential",
			seed:    `UPDATE ssh_credentials SET owner_agency='ag-dss' WHERE id='k1'`,
			want:    "owned SSH credential",
			notWant: "scope",
		},
		{
			name: "runner",
			seed: `INSERT INTO runners(id, name, status, registered_at, created_at)
			         VALUES('r1','runner-1','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z');
			       INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r1','ag-dss')`,
			want:    "runner",
			notWant: "owned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := membershipDB(t)
			if _, err := pool.Exec(tc.seed); err != nil {
				t.Fatalf("seed: %v", err)
			}

			_, err := DeleteAgency(ctx, pool, "ag-dss", "alice")
			if !errors.Is(err, ErrAgencyInUse) {
				t.Fatalf("delete = %v, want ErrAgencyInUse (the sentinel must survive wrapping)", err)
			}

			var inUse *AgencyInUseError
			if !errors.As(err, &inUse) {
				t.Fatalf("delete error does not unwrap to *AgencyInUseError: %v", err)
			}
			got := strings.Join(inUse.Blockers(), "; ")
			if !strings.Contains(got, tc.want) {
				t.Errorf("blockers = %q, want it to name %q", got, tc.want)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("blockers = %q, must NOT name %q — that is the misattribution this guards", got, tc.notWant)
			}
		})
	}
}

// TestAgencyInUseOwnershipStatesTheRemedyIsDestructive — ownership is the one
// blocker with no clearing path while owner transfer is deferred (RA-Q22). The
// other three classes name an edit; this one must warn that the row has to be
// re-created, and that a stored secret's value dies with it. An operator who
// deletes the secret to unblock the delete, having been told only "clear this
// first", loses a credential nobody may hold a copy of.
func TestAgencyInUseOwnershipStatesTheRemedyIsDestructive(t *testing.T) {
	pool := membershipDB(t)
	if _, err := pool.Exec(`UPDATE secrets SET owner_agency='ag-dss' WHERE id='s1'`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := DeleteAgency(context.Background(), pool, "ag-dss", "alice")
	var inUse *AgencyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("want *AgencyInUseError, got %v", err)
	}
	if inUse.Owned() != 1 {
		t.Errorf("Owned() = %d, want 1", inUse.Owned())
	}
	got := strings.Join(inUse.Blockers(), "; ")
	for _, want := range []string{"transfer is not available", "REVEAL THE VALUE FIRST", "destroys it"} {
		if !strings.Contains(got, want) {
			t.Errorf("blockers = %q, want it to contain %q", got, want)
		}
	}
}

// TestAgencyInUseCountsEveryClassAtOnce — the counts are independent, so an agency
// blocked four ways must report four blockers, not the first one found. A guard
// that short-circuits sends the operator through as many delete attempts as there
// are reference classes.
func TestAgencyInUseCountsEveryClassAtOnce(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	for _, q := range []string{
		`INSERT INTO runners(id, name, status, registered_at, created_at)
		   VALUES('r1','runner-1','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`,
		`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r1','ag-dss')`,
		`UPDATE secrets SET owner_agency='ag-dss' WHERE id='s1'`,
		`UPDATE env_vars SET owner_agency='ag-dss' WHERE id='v1'`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	// Scope references live in scope_agencies since migration 700 — not a column on
	// scopes — so a scope blocker is seeded as membership like any other kind.
	if err := SetAgencyMembership(ctx, pool, MemberScope,
		[]AgencyMembership{{ID: "sc-prod", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("assign scope membership: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("assign secret membership: %v", err)
	}

	_, err := DeleteAgency(ctx, pool, "ag-dss", "alice")
	var inUse *AgencyInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("want *AgencyInUseError, got %v", err)
	}
	if inUse.Runners != 1 || inUse.Secrets != 1 || inUse.EnvVars != 1 {
		t.Errorf("counts = runners:%d secrets:%d envvars:%d, want 1 each",
			inUse.Runners, inUse.Secrets, inUse.EnvVars)
	}
	if n := inUse.Members[MemberScope]; n != 1 {
		t.Errorf("scope memberships = %d, want 1", n)
	}
	if n := inUse.Members[MemberSecret]; n != 1 {
		t.Errorf("secret memberships = %d, want 1", n)
	}
	if got := len(inUse.Blockers()); got < 5 {
		t.Errorf("Blockers() returned %d phrases, want >= 5 (one per non-zero class): %v",
			got, inUse.Blockers())
	}
}
