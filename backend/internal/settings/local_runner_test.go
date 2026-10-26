package settings

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
)

// The local runner's row and switch (LR-17, LR-38 to LR-46).

func TestEnsureLocalRunnerCreatesTheRowOnceAndSeedsOnce(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()

	if id, _ := LocalRunnerID(ctx, pool); id != "" {
		t.Fatalf("a freshly migrated database already has a local runner (%s): the row is the server's to write at start", id)
	}
	// First start of an upgraded install where the SSH executor was ON with 7 slots.
	id, seeded, err := EnsureLocalRunner(ctx, pool, true, 7)
	if err != nil || id == "" || !seeded {
		t.Fatalf("first ensure = %q, seeded %v, err %v", id, seeded, err)
	}
	var kind, status, owner, caps string
	var conc int
	var inject bool
	if err := pool.QueryRow(`SELECT kind, status, owner_agency, capabilities, max_concurrent, allow_secret_injection FROM runners WHERE id = ?`, id).
		Scan(&kind, &status, &owner, &caps, &conc, &inject); err != nil {
		t.Fatal(err)
	}
	if kind != RunnerKindServer || status != "offline" || owner != "global" || conc != 7 || !inject {
		t.Errorf("row = kind %q status %q owner %q conc %d inject %v; want server, offline, global, 7, true", kind, status, owner, conc, inject)
	}
	if caps != `["bash","perl","powershell","python"]` {
		t.Errorf("capabilities = %s, want the four shell types", caps)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"global"}) {
		t.Errorf("it is born serving %v, want Global", got)
	}
	// The STORED switch: since 2.5.3 an unassigned local runner is off
	// whatever it says (CA-9), and the seed is what the switch was written.
	if on, err := localRunnerSwitch(ctx, pool); err != nil || !on {
		t.Errorf("the switch was seeded %v (err %v), want on: an upgrade arrives in the state it was running in", on, err)
	}

	// Every later start is a no-op, whatever the environment says by then: the
	// app's own values are the truth, and the old names are ignored.
	id2, seeded2, err := EnsureLocalRunner(ctx, pool, false, 99)
	if err != nil || id2 != id || seeded2 {
		t.Fatalf("second ensure = %q, seeded %v, err %v; want the same row and no reseed", id2, seeded2, err)
	}
	if on, _ := localRunnerSwitch(ctx, pool); !on {
		t.Error("a later start turned the local runner off from the environment")
	}
	if err := pool.QueryRow(`SELECT max_concurrent FROM runners WHERE id = ?`, id).Scan(&conc); err != nil || conc != 7 {
		t.Errorf("a later start changed its concurrency to %d", conc)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE kind = 'server'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("%d local runners, want one", n)
	}
	// And the schema holds that: a second row of the kind is refused.
	if _, err := pool.Exec(`INSERT INTO runners (id, name, kind, status, registered_at, created_at)
	                        VALUES ('r-second', 'second', 'server', 'offline', 't', 't')`); err == nil {
		t.Error("a second local runner was inserted")
	}
}

func TestLocalRunnerIsOffByDefaultAndTheHostCanForbidIt(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	// A new install: the SSH executor was never enabled.
	if _, _, err := EnsureLocalRunner(ctx, pool, false, 0); err != nil {
		t.Fatal(err)
	}
	if on, _ := LocalRunnerEnabled(ctx, pool, false); on {
		t.Error("a new install's local runner is on")
	}
	lr, err := GetLocalRunner(ctx, pool, false)
	if err != nil || lr.Enabled || lr.Forbidden || lr.MaxConcurrent != 4 || lr.Status != "offline" {
		t.Errorf("card = %+v (err %v), want off, not forbidden, 4 at once, offline", lr, err)
	}

	on := true
	giveLocalRunnerTo(t, pool, "ag-lr") // unassigned is off and is not turned on (2.5.3, CA-9)
	if _, err := SetLocalRunner(ctx, pool, false, &on, nil, "root@example.com"); err != nil {
		t.Fatalf("turning it on: %v", err)
	}
	if got, _ := LocalRunnerEnabled(ctx, pool, false); !got {
		t.Fatal("the switch did not turn on")
	}
	// LR-17: the host's "forbid" wins over the stored setting, and the app
	// cannot turn it on.
	if got, _ := LocalRunnerEnabled(ctx, pool, true); got {
		t.Error("the local runner reads as on although the host forbids it")
	}
	if lr, _ := GetLocalRunner(ctx, pool, true); lr == nil || lr.Enabled || !lr.Forbidden {
		t.Errorf("card under forbid = %+v, want off and forbidden", lr)
	}
	if _, err := SetLocalRunner(ctx, pool, true, &on, nil, "root@example.com"); !errors.Is(err, ErrLocalRunnerForbidden) {
		t.Errorf("turning it on under forbid = %v, want ErrLocalRunnerForbidden", err)
	}
	// Turning it OFF is always allowed, forbidden or not.
	off := false
	if _, err := SetLocalRunner(ctx, pool, true, &off, nil, "root@example.com"); err != nil {
		t.Errorf("turning it off under forbid: %v", err)
	}
}

func TestSetLocalRunnerIsAuditedAndBounded(t *testing.T) {
	pool := membershipDB(t)
	ctx := context.Background()
	if _, _, err := EnsureLocalRunner(ctx, pool, false, 4); err != nil {
		t.Fatal(err)
	}
	giveLocalRunnerTo(t, pool, "ag-lr") // unassigned is off and is not turned on (2.5.3, CA-9)
	// The installation's rows (a change of the switch is written for the
	// built-in agency and for the local runner's agency since 2.5.3), not the
	// assignment's own.
	rows := func() []string {
		t.Helper()
		rs, err := pool.Query(`SELECT actor || '|' || summary FROM activity
		  WHERE runner_name = ? AND agency_id = 'global' AND summary NOT LIKE 'local runner given to%' ORDER BY id`, LocalRunnerName)
		if err != nil {
			t.Fatal(err)
		}
		defer rs.Close()
		var out []string
		for rs.Next() {
			var s string
			_ = rs.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	on, off := true, false
	eight, zero, huge := 8, 0, 65
	if _, err := SetLocalRunner(ctx, pool, false, &on, &eight, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	got := rows()
	if len(got) != 2 ||
		got[0] != "alice@example.com|local runner turned on: this server now holds SSH keys and connects to job targets" ||
		got[1] != "alice@example.com|local runner concurrency set to 8 (was 4)" {
		t.Errorf("activity after turning on with 8 slots = %v", got)
	}
	// Saying what is already so writes nothing.
	if _, err := SetLocalRunner(ctx, pool, false, &on, &eight, "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := rows(); len(got) != 2 {
		t.Errorf("an unchanged save wrote a row: %v", got)
	}
	if _, err := SetLocalRunner(ctx, pool, false, &off, nil, "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if got := rows(); len(got) != 3 || got[2] != "bob@example.com|local runner turned off: runs it is running finish, and it claims no more" {
		t.Errorf("activity after turning off = %v", got)
	}
	for _, bad := range []*int{&zero, &huge} {
		if _, err := SetLocalRunner(ctx, pool, false, nil, bad, "alice@example.com"); !errors.Is(err, ErrValidation) {
			t.Errorf("concurrency %d = %v, want ErrValidation", *bad, err)
		}
	}
	if lr, _ := GetLocalRunner(ctx, pool, false); lr.MaxConcurrent != 8 {
		t.Errorf("a refused concurrency changed it to %d", lr.MaxConcurrent)
	}
}

// The local runner is Global's and serves Global only (§0a, IN-3). Until §0a
// it was the ONE runner with a serve list (MA-11, MA-14): a global
// administrator gave it any non-empty set. Its owner never changes, and it is
// never a legacy placement.
// Unassigned (2.5.3, CA-9: born the built-in agency's), its serve list is
// that agency alone: it serves its owner and no other, and changes agency by
// SetLocalRunnerOwner only (CA-8). Until 2.5.3 this was "it serves Global
// only", the same rows.
func TestAnUnassignedLocalRunnerServesItsOwnerOnly(t *testing.T) {
	pool := placementDB(t) // agencies ag-dss, ag-nwd; agents r-dss, r-global, r-legacy
	ctx := context.Background()
	id, _, err := EnsureLocalRunner(ctx, pool, false, 4)
	if err != nil {
		t.Fatal(err)
	}
	set := func(ids ...string) error {
		return SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: id, AgencyIDs: ids}}, "root@example.com")
	}
	for _, refused := range [][]string{
		{"ag-dss", "global"}, // Global beside an agency
		{"ag-dss", "ag-nwd"}, // agencies
		{"ag-nwd"},           // one agency
	} {
		if err := set(refused...); !errors.Is(err, ErrLocalRunnerGlobalOnly) {
			t.Errorf("serve list %v: %v, want ErrLocalRunnerGlobalOnly", refused, err)
		}
		if got := serveList(t, pool, id); !slices.Equal(got, []string{"global"}) {
			t.Fatalf("after a refused %v it serves %v", refused, got)
		}
	}
	if err := set("global"); err != nil {
		t.Errorf("serve list [global]: %v", err)
	}
	if err := set(); !errors.Is(err, ErrAgencyRequired) {
		t.Errorf("an empty serve list = %v, want ErrAgencyRequired", err)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"global"}) {
		t.Errorf("a refused write changed its serve list to %v", got)
	}
	// The same through an agency's member list: NWD cannot take it on (§0a;
	// until then it served NWD beside Global from here).
	if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", refs([2]string{"runner", "r-legacy"}, [2]string{"runner", id}), "root@example.com"); !errors.Is(err, ErrLocalRunnerGlobalOnly) {
		t.Errorf("adding the local runner to an agency's members: %v, want ErrLocalRunnerGlobalOnly", err)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"global"}) {
		t.Errorf("after a refused join it serves %v, want Global alone", got)
	}
	// An agent still cannot hold Global beside an agency: the rule is the local runner's alone.
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: "r-global", AgencyIDs: []string{"global", "ag-dss"}}}, "root@example.com"); err == nil {
		t.Error("an agent was given Global beside an agency")
	}
	// Its owner is Global and stays Global.
	if err := SetRunnerOwner(ctx, pool, id, "ag-dss", "root@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
		t.Errorf("handing the local runner to the one agency it serves = %v, want ErrOwnerChangeRefused", err)
	}
	if o := ownerOf(t, pool, id); o != "global" {
		t.Errorf("the local runner's owner is %q", o)
	}
}

// giveLocalRunnerTo gives the local runner to agency (made if it is not
// there): since 2.5.3 an unassigned local runner is off (CA-9).
func giveLocalRunnerTo(t *testing.T, pool *sql.DB, agency string) {
	t.Helper()
	if _, err := pool.Exec(`INSERT OR IGNORE INTO agencies (id, name, created_at) VALUES (?, ?, 't')`, agency, "Agency "+agency); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetLocalRunnerOwner(context.Background(), pool, agency, "root@example.com"); err != nil {
		t.Fatalf("give the local runner to %s: %v", agency, err)
	}
}
