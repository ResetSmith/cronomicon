package settings

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// Phase 2 of the agencies plan: the membership tables exist and are
// writable, the delete guard counts them, and NOTHING dispatches or resolves
// against them yet. That last property is what makes the phase reversible.

func membershipDB(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := db.Open(filepath.Join(t.TempDir(), "membership.db"))
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
	exec(`INSERT INTO secrets(id, key, source, created_at) VALUES('s1','DB_PASS','stored',?)`, now)
	exec(`INSERT INTO env_vars(id, key, value, created_at) VALUES('v1','REGION','us-east',?)`, now)
	exec(`INSERT INTO ssh_credentials(id, label, source, created_at, last_modified_at) VALUES('k1','deploy_key','stored',?,?)`, now, now)
	return pool
}

// TestSetAgencyMembershipAllKinds — one generic store over four entity types; each
// must round-trip, replace-per-row, dedupe, and clear.
func TestSetAgencyMembershipAllKinds(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()

	for _, tc := range []struct {
		kind MemberKind
		id   string
	}{
		{MemberScope, "sc-prod"},
		{MemberSecret, "s1"},
		{MemberEnvVar, "v1"},
		{MemberSSHCredential, "k1"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			// Duplicates in the payload must not violate the composite PK.
			err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"ag-nwd", "ag-dss", "ag-dss"}}}, "alice")
			if err != nil {
				t.Fatalf("set: %v", err)
			}
			got, err := ListAgencyMembership(ctx, pool, tc.kind)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(got) != 1 || got[0].ID != tc.id || len(got[0].AgencyIDs) != 2 {
				t.Fatalf("membership = %+v, want one row with 2 agencies", got)
			}
			// Sorted, so a re-save produces an identical row and the audit detail is
			// stable rather than depending on client ordering.
			if got[0].AgencyIDs[0] != "ag-dss" || got[0].AgencyIDs[1] != "ag-nwd" {
				t.Errorf("agency ids not sorted: %v", got[0].AgencyIDs)
			}
			// An empty set CLEARS — the "no restriction" state, not a no-op.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id}}, "alice"); err != nil {
				t.Fatalf("clear: %v", err)
			}
			if got, _ := ListAgencyMembership(ctx, pool, tc.kind); len(got) != 0 {
				t.Errorf("membership not cleared: %+v", got)
			}
		})
	}
}

// TestSetAgencyMembershipFailsClosed — every id is validated BEFORE any write, so
// a bad id in a bulk matrix save cannot leave the first half applied.
func TestSetAgencyMembershipFailsClosed(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()

	err := SetAgencyMembership(ctx, pool, MemberSecret, []AgencyMembership{
		{ID: "s1", AgencyIDs: []string{"ag-dss"}},
		{ID: "does-not-exist", AgencyIDs: []string{"ag-dss"}},
	}, "alice")
	if !errors.Is(err, ErrUnknownMember) {
		t.Fatalf("err = %v, want ErrUnknownMember", err)
	}
	if got, _ := ListAgencyMembership(ctx, pool, MemberSecret); len(got) != 0 {
		t.Errorf("a rejected batch still wrote %d rows — validation must precede every write", len(got))
	}

	err = SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-ghost"}}}, "alice")
	if !errors.Is(err, ErrUnknownAgency) {
		t.Fatalf("err = %v, want ErrUnknownAgency", err)
	}
	if got, _ := ListAgencyMembership(ctx, pool, MemberSecret); len(got) != 0 {
		t.Errorf("unknown agency still wrote membership")
	}
}

// TestScopeAgencyBindingWritesTheJoinTable — SetScopeAgency is the 1:1 affordance
// the Scopes tab has always used; after migration 700 dropped scopes.agency_id it
// writes scope_agencies and nothing else. The dual-write this test used to assert
// (T2.5) went away with the column it mirrored.
func TestScopeAgencyBindingWritesTheJoinTable(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	joined := func() []string {
		t.Helper()
		got, err := ListAgencyMembership(ctx, pool, MemberScope)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, m := range got {
			if m.ID == "sc-prod" {
				return m.AgencyIDs
			}
		}
		return nil
	}

	aid := "ag-dss"
	if _, err := SetScopeAgency(ctx, pool, "sc-prod", &aid, "alice"); err != nil {
		t.Fatalf("SetScopeAgency: %v", err)
	}
	if len(joined()) != 1 || joined()[0] != "ag-dss" {
		t.Fatalf("binding did not reach scope_agencies: %v", joined())
	}
	// Clearing removes the row rather than leaving a stale one.
	if _, err := SetScopeAgency(ctx, pool, "sc-prod", nil, "alice"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(joined()) != 0 {
		t.Fatalf("clear left membership behind: %v", joined())
	}

	// The N:M setter is the only way to express more than one, and it must not be
	// truncated by anything the 1:1 endpoint left behind.
	if err := SetAgencyMembership(ctx, pool, MemberScope,
		[]AgencyMembership{{ID: "sc-prod", AgencyIDs: []string{"ag-nwd", "ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("SetAgencyMembership: %v", err)
	}
	if len(joined()) != 2 {
		t.Errorf("scope membership = %v, want both agencies", joined())
	}
}

// TestDeleteAgencyGuardCountsMembership (T2.8) is the case the pre-Phase-2 guard
// would MISS: an agency whose only members are a SECRET and a KEY. Those tables
// cascade, so without this the delete succeeds and silently drops an
// access-control fact — no 409, nothing to notice.
func TestDeleteAgencyGuardCountsMembership(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind MemberKind
		id   string
	}{
		{"secret member", MemberSecret, "s1"},
		{"variable member", MemberEnvVar, "v1"},
		{"ssh key member", MemberSSHCredential, "k1"},
		{"scope member", MemberScope, "sc-prod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := membershipDB(t)
			ctx := context.Background()
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
				t.Fatalf("assign: %v", err)
			}
			_, err := DeleteAgency(ctx, pool, "ag-dss", "alice")
			if !errors.Is(err, ErrAgencyInUse) {
				t.Fatalf("delete with a %s = %v, want ErrAgencyInUse", tc.name, err)
			}
			// Clearing membership makes it deletable again — the guard must not be a
			// one-way latch.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id}}, "alice"); err != nil {
				t.Fatalf("clear: %v", err)
			}
			ok, err := DeleteAgency(ctx, pool, "ag-dss", "alice")
			if err != nil || !ok {
				t.Fatalf("delete after clearing membership = (%v, %v), want (true, nil)", ok, err)
			}
		})
	}
}

// TestMembershipIsAudited (T2.7) — membership decides which runs can reach which
// secrets, so it is an access-control fact and belongs in change_log next to the
// agency CRUD it sits beside.
func TestMembershipIsAudited(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "alice@example.com"); err != nil {
		t.Fatalf("set: %v", err)
	}
	var actor, action, target, details string
	err := pool.QueryRow(`SELECT actor, action, target, COALESCE(details,'') FROM change_log
	     WHERE category='Agencies' ORDER BY at DESC LIMIT 1`).Scan(&actor, &action, &target, &details)
	if err != nil {
		t.Fatalf("no audit row written for a membership change: %v", err)
	}
	if actor != "alice@example.com" || action != "membership-updated" {
		t.Errorf("audit row = actor %q action %q, want alice@example.com / membership-updated", actor, action)
	}
	if details == "" {
		t.Errorf("audit detail is empty — a membership change must record WHAT changed")
	}
}

// TestBuildAgencyMatrixIncludesUnrestrictedRows (T4.1). The matrix must list rows
// with NO membership, because an empty row is not missing information — it says
// "unrestricted, reachable from everywhere", which is the fact an operator needs
// before assigning membership that would narrow it. A grid that only showed
// assigned rows would hide exactly what the operator is about to break.
func TestBuildAgencyMatrixIncludesUnrestrictedRows(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("assign: %v", err)
	}

	m, err := BuildAgencyMatrix(ctx, pool)
	if err != nil {
		t.Fatalf("BuildAgencyMatrix: %v", err)
	}
	if len(m.Agencies) != 2 {
		t.Fatalf("columns = %d, want 2", len(m.Agencies))
	}
	byKey := map[string]MatrixRow{}
	for _, r := range m.Rows {
		byKey[r.Kind+" "+r.Name] = r
	}
	// Every kind is represented, including the ones with no membership at all.
	for _, want := range []string{"scope prod", "secret DB_PASS", "env-var REGION", "ssh-credential deploy_key"} {
		if _, ok := byKey[want]; !ok {
			t.Errorf("matrix is missing %q — a row with no membership must still appear", want)
		}
	}
	if got := byKey["secret DB_PASS"].AgencyIDs; len(got) != 1 || got[0] != "ag-dss" {
		t.Errorf("assigned row agencyIds = %v, want [ag-dss]", got)
	}
	// The unassigned rows must carry an EMPTY slice, not null — a client rendering
	// "unrestricted" should not have to special-case a nil.
	if got := byKey["env-var REGION"].AgencyIDs; got == nil || len(got) != 0 {
		t.Errorf("unassigned row agencyIds = %v, want an empty (non-nil) slice", got)
	}
}

// TestBuildAgencyDetailReportsTheQueueTrap (T4.2/T4.3) — the number that matters is
// onlineRunners: zero means every run targeting this agency queues forever, and
// until this view existed that was visible only on one run's status line at a time.
func TestBuildAgencyDetailReportsTheQueueTrap(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	const now = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := SetAgencyMembership(ctx, pool, MemberScope,
		[]AgencyMembership{{ID: "sc-prod", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("assign scope: %v", err)
	}
	if err := SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Fatalf("assign secret: %v", err)
	}
	// Two runs waiting on DSS, and one on another agency that must not be counted.
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES('r1','j','bash','queued','t','manual',?,'["DSS"]')`, now)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('r1','DSS')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES('r2','j','bash','queued','t','manual',?,'["DSS"]')`, now)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('r2','DSS')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES('r3','j','bash','queued','t','manual',?,'["NWD"]')`, now)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('r3','NWD')`)
	// A run that already ran must not inflate the waiting count.
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES('r4','j','bash','success','t','manual',?,'["DSS"]')`, now)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('r4','DSS')`)

	d, err := BuildAgencyDetail(ctx, pool, "ag-dss")
	if err != nil || d == nil {
		t.Fatalf("BuildAgencyDetail = (%v, %v)", d, err)
	}
	if d.OnlineRunners != 0 {
		t.Errorf("onlineRunners = %d, want 0 — this is the queue-forever trap", d.OnlineRunners)
	}
	if d.QueuedRuns != 2 {
		t.Errorf("queuedRuns = %d, want 2 (the other agency's run and the finished one must not count)", d.QueuedRuns)
	}
	kinds := map[string]int{}
	for _, m := range d.Members {
		kinds[m.Kind]++
	}
	if kinds["scope"] != 1 || kinds["secret"] != 1 {
		t.Errorf("members by kind = %v, want one scope and one secret", kinds)
	}

	// Bring an online runner into the agency: the trap clears.
	exec(`INSERT INTO runners(id,name,status,registered_at,created_at) VALUES('rn','r','online',?,?)`, now, now)
	exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('rn','ag-dss')`)
	d, err = BuildAgencyDetail(ctx, pool, "ag-dss")
	if err != nil {
		t.Fatalf("BuildAgencyDetail: %v", err)
	}
	if d.OnlineRunners != 1 {
		t.Errorf("onlineRunners = %d, want 1", d.OnlineRunners)
	}

	// An unknown agency is (nil, nil) so the handler can 404 rather than 500.
	missing, err := BuildAgencyDetail(ctx, pool, "nope")
	if err != nil || missing != nil {
		t.Errorf("BuildAgencyDetail(unknown) = (%v, %v), want (nil, nil)", missing, err)
	}
}
