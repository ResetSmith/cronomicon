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
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-prod','prod','cronomicon',?)`, now)
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
			agencies := func() []string {
				t.Helper()
				got, err := ListAgencyMembership(ctx, pool, tc.kind)
				if err != nil {
					t.Fatalf("list: %v", err)
				}
				if len(got) != 1 || got[0].ID != tc.id {
					t.Fatalf("membership = %+v, want one row for %s", got, tc.id)
				}
				return got[0].AgencyIDs
			}
			owner := func() string {
				t.Helper()
				if tc.kind == MemberScope {
					return ""
				}
				mt, _ := memberTableFor(tc.kind)
				return entityOwner(ctx, pool, mt.catalog, tc.id)
			}
			// One agency. A repeat of it in the payload is the same agency.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"ag-dss", "ag-dss"}}}, "alice"); err != nil {
				t.Fatalf("set: %v", err)
			}
			if got := agencies(); len(got) != 1 || got[0] != "ag-dss" {
				t.Fatalf("agencies = %v, want ag-dss alone", got)
			}
			// LR-54: for a secret, a variable and a key the agency is the owner.
			if tc.kind != MemberScope && owner() != "ag-dss" {
				t.Errorf("owner after moving to DSS = %q, want ag-dss", owner())
			}
			// LR-7, LR-54: two agencies is refused, and changes nothing.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"ag-nwd", "ag-dss"}}}, "alice"); !errors.Is(err, ErrOneAgency) {
				t.Fatalf("two agencies = %v, want ErrOneAgency", err)
			}
			// An empty set is REFUSED (LR-26). It used to clear the row to "no
			// restriction" — for a secret, usable by every agency — which made a
			// delete the way to take that decision.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id}}, "alice"); !errors.Is(err, ErrAgencyRequired) {
				t.Fatalf("an empty set = %v, want ErrAgencyRequired", err)
			}
			// Global beside another agency is refused too (LR-25).
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"global", "ag-dss"}}}, "alice"); !errors.Is(err, ErrGlobalMixed) {
				t.Fatalf("Global with another agency = %v, want ErrGlobalMixed", err)
			}
			if got := agencies(); len(got) != 1 || got[0] != "ag-dss" {
				t.Fatalf("a refused write changed membership: %v", got)
			}
			// A move: the other agency, in one write, owner and all. Until 2.3.0
			// an owned row could not leave its owner; it had to be re-created.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"ag-nwd"}}}, "alice"); err != nil {
				t.Fatalf("move to NWD: %v", err)
			}
			if got := agencies(); len(got) != 1 || got[0] != "ag-nwd" {
				t.Fatalf("agencies after the move = %v, want ag-nwd alone", got)
			}
			if tc.kind != MemberScope && owner() != "ag-nwd" {
				t.Errorf("owner after the move = %q, want ag-nwd", owner())
			}
			// Naming Global is how a row becomes Global's.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"global"}}}, "alice"); err != nil {
				t.Fatalf("move to Global: %v", err)
			}
			if got := agencies(); len(got) != 1 || got[0] != "global" {
				t.Errorf("membership after the move to Global: %v", got)
			}
			if tc.kind != MemberScope && owner() != "global" {
				t.Errorf("owner after the move to Global = %q, want global", owner())
			}
		})
	}
}

// LR-54 — a move must not land on a name the target agency already owns: two
// rows of one agency with one name (and scope) is the ambiguity the resolver
// refuses to pick between. That holds within a table and across the secret and
// variable tables, which share one namespace per owner.
func TestMovingARowOntoANameItsNewAgencyOwnsIsRefused(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	exec := func(q string, a ...any) {
		t.Helper()
		if _, err := pool.Exec(q, a...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const ts = "2026-01-01T00:00:00Z"
	// DSS owns a secret, a variable and a key; Global holds one of each under
	// the same names, and a secret named like DSS's variable.
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('dss-s', 'TOKEN', 'prod', 'stored', ?, 'ag-dss')`, ts)
	exec(`INSERT INTO secret_agencies VALUES ('dss-s', 'ag-dss')`)
	exec(`INSERT INTO env_vars (id, key, scope, value, created_at, owner_agency) VALUES ('dss-v', 'REGION', 'prod', 'v', ?, 'ag-dss')`, ts)
	exec(`INSERT INTO env_var_agencies VALUES ('dss-v', 'ag-dss')`)
	exec(`INSERT INTO ssh_credentials (id, label, source, created_at, owner_agency) VALUES ('dss-k', 'deploy', 'stored', ?, 'ag-dss')`, ts)
	exec(`INSERT INTO ssh_credential_agencies VALUES ('dss-k', 'ag-dss')`)
	exec(`INSERT INTO secrets (id, key, scope, source, created_at) VALUES ('g-s', 'TOKEN', 'prod', 'stored', ?), ('g-s2', 'REGION', 'prod', 'stored', ?), ('g-s3', 'TOKEN', 'staging', 'stored', ?)`, ts, ts, ts)
	exec(`INSERT INTO ssh_credentials (id, label, source, created_at) VALUES ('g-k', 'deploy', 'stored', ?)`, ts)

	for _, c := range []struct {
		name string
		kind MemberKind
		id   string
	}{
		{"a secret onto DSS's secret of that key and scope", MemberSecret, "g-s"},
		{"a secret onto DSS's VARIABLE of that key and scope", MemberSecret, "g-s2"},
		{"a key onto DSS's key of that label", MemberSSHCredential, "g-k"},
	} {
		err := SetAgencyMembership(ctx, pool, c.kind, []AgencyMembership{{ID: c.id, AgencyIDs: []string{"ag-dss"}}}, "alice")
		if !errors.Is(err, ErrOwnerConflict) {
			t.Errorf("moving %s = %v, want ErrOwnerConflict", c.name, err)
		}
		mt, _ := memberTableFor(c.kind)
		if got := entityOwner(ctx, pool, mt.catalog, c.id); got != "global" {
			t.Errorf("%s: a refused move changed the owner to %q", c.name, got)
		}
	}
	// The same key in ANOTHER scope is a different row to the resolver: it moves.
	if err := SetAgencyMembership(ctx, pool, MemberSecret, []AgencyMembership{{ID: "g-s3", AgencyIDs: []string{"ag-dss"}}}, "alice"); err != nil {
		t.Errorf("moving a secret of the same key in another scope: %v", err)
	}
	// And the agency-shaped editor refuses the same collision.
	if _, err := SetAgencyMembers(ctx, pool, "ag-dss", []AgencyMemberRef{
		{Kind: "secret", ID: "dss-s"}, {Kind: "secret", ID: "g-s3"}, {Kind: "env-var", ID: "dss-v"},
		{Kind: "ssh-credential", ID: "dss-k"}, {Kind: "secret", ID: "g-s"},
	}, "alice"); !errors.Is(err, ErrOwnerConflict) {
		t.Errorf("adding a colliding secret through the agency editor = %v, want ErrOwnerConflict", err)
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
	// s1 is where it was born: in Global, and nowhere else.
	untouched := func() bool {
		got, _ := ListAgencyMembership(ctx, pool, MemberSecret)
		return len(got) == 1 && got[0].ID == "s1" && len(got[0].AgencyIDs) == 1 && got[0].AgencyIDs[0] == "global"
	}
	if !untouched() {
		t.Errorf("a rejected batch still wrote rows — validation must precede every write")
	}

	err = SetAgencyMembership(ctx, pool, MemberSecret,
		[]AgencyMembership{{ID: "s1", AgencyIDs: []string{"ag-ghost"}}}, "alice")
	if !errors.Is(err, ErrUnknownAgency) {
		t.Fatalf("err = %v, want ErrUnknownAgency", err)
	}
	if !untouched() {
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
	// "No agency" on this route is Global (LR-26): the old row goes and the scope
	// is Global's, never nobody's.
	if _, err := SetScopeAgency(ctx, pool, "sc-prod", nil, "alice"); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got := joined(); len(got) != 1 || got[0] != "global" {
		t.Fatalf("after clearing, membership = %v, want [global]", got)
	}

	// A scope from before 2.3.0 may still be in several agencies (LR-7: nothing
	// removes them automatically). Setting its agency settles it on the one named.
	if _, err := pool.Exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-prod'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-prod', 'ag-dss'), ('sc-prod', 'ag-nwd')`); err != nil {
		t.Fatal(err)
	}
	if len(joined()) != 2 {
		t.Fatalf("fixture: scope membership = %v, want the two legacy agencies", joined())
	}
	nwd := "ag-nwd"
	if _, err := SetScopeAgency(ctx, pool, "sc-prod", &nwd, "alice"); err != nil {
		t.Fatalf("SetScopeAgency on a scope in two agencies: %v", err)
	}
	if got := joined(); len(got) != 1 || got[0] != "ag-nwd" {
		t.Errorf("scope membership = %v, want ag-nwd alone", got)
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
			// Moving the member elsewhere makes it deletable again — the guard must
			// not be a one-way latch.
			if err := SetAgencyMembership(ctx, pool, tc.kind,
				[]AgencyMembership{{ID: tc.id, AgencyIDs: []string{"global"}}}, "alice"); err != nil {
				t.Fatalf("move to Global: %v", err)
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
	// Global is a column like any other: "what belongs to Global" is a question
	// the grid answers.
	if len(m.Agencies) != 3 {
		t.Fatalf("columns = %d, want 3 (DSS, Global, NWD)", len(m.Agencies))
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
	// A row nobody assigned is Global's, and says so: there is no empty set.
	if got := byKey["env-var REGION"].AgencyIDs; len(got) != 1 || got[0] != "global" {
		t.Errorf("unassigned row agencyIds = %v, want [global]", got)
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

// LR-80 — an agency owns a Vault-backed row only when the row's path is one the
// agency may name. A row several agencies shared is Global-owned; when all but
// one give it up the one that is left becomes its owner, but not of a Vault path
// outside its prefixes: another agency removing itself must not be a way to
// hand an agency a path it was never assigned.
func TestNarrowingASharedVaultRowDoesNotGiveAwayAPathItsNewOwnerMayNotName(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, prefix, wantOwner string
	}{
		{"the remaining agency may name the path", "secret/data/shared", "ag-dss"},
		{"it may not", "secret/data/dss-only", "global"},
		{"it has no prefix at all", "", "global"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := membershipDB(t)
			exec := func(q string, a ...any) {
				t.Helper()
				if _, err := pool.Exec(q, a...); err != nil {
					t.Fatalf("seed: %v\n%s", err, q)
				}
			}
			exec(`INSERT INTO secrets (id, key, source, vault_ref, created_at) VALUES ('sv', 'SHARED_VAULT', 'vault', 'secret/data/shared/db#k', 't')`)
			exec(`DELETE FROM secret_agencies WHERE secret_id = 'sv'`)
			exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('sv', 'ag-dss'), ('sv', 'ag-nwd')`)
			if c.prefix != "" {
				if _, err := SetVaultPrefixes(ctx, pool, "ag-dss", []string{c.prefix}, "root"); err != nil {
					t.Fatalf("SetVaultPrefixes: %v", err)
				}
			}
			// NWD gives up its share.
			if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", nil, "nwd-admin"); err != nil {
				t.Fatalf("SetAgencyMembers: %v", err)
			}
			if got := entityOwner(ctx, pool, "secrets", "sv"); got != c.wantOwner {
				t.Errorf("owner after NWD left = %q, want %q", got, c.wantOwner)
			}
			var members string
			_ = pool.QueryRow(`SELECT group_concat(agency_id) FROM secret_agencies WHERE secret_id = 'sv'`).Scan(&members)
			if members != "ag-dss" {
				t.Errorf("members after NWD left = %q, want ag-dss alone (the removal itself is NWD's to make)", members)
			}
		})
	}
}
