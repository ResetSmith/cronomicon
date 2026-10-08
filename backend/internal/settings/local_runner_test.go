package settings

import (
	"context"
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
	if on, err := LocalRunnerEnabled(ctx, pool, false); err != nil || !on {
		t.Errorf("the switch was seeded %v (err %v), want on: an upgrade arrives in the state it was running in", on, err)
	}

	// Every later start is a no-op, whatever the environment says by then: the
	// app's own values are the truth, and the old names are ignored.
	id2, seeded2, err := EnsureLocalRunner(ctx, pool, false, 99)
	if err != nil || id2 != id || seeded2 {
		t.Fatalf("second ensure = %q, seeded %v, err %v; want the same row and no reseed", id2, seeded2, err)
	}
	if on, _ := LocalRunnerEnabled(ctx, pool, false); !on {
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
	rows := func() []string {
		t.Helper()
		rs, err := pool.Query(`SELECT actor || '|' || summary FROM activity WHERE runner_name = ? ORDER BY id`, LocalRunnerName)
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

// MA-11, MA-14: the local runner is the ONE runner with a serve list. A global
// administrator gives it any non-empty set — Global beside agencies included,
// which the schema refuses for an agent — and takes agencies away again. Its
// owner never changes, and it is never a legacy placement.
func TestTheLocalRunnerServesWhateverItIsGiven(t *testing.T) {
	pool := placementDB(t) // agencies ag-dss, ag-nwd; agents r-dss, r-global, r-legacy
	ctx := context.Background()
	id, _, err := EnsureLocalRunner(ctx, pool, false, 4)
	if err != nil {
		t.Fatal(err)
	}
	set := func(ids ...string) error {
		return SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: id, AgencyIDs: ids}}, "root@example.com")
	}
	for _, want := range [][]string{
		{"ag-dss", "global"},           // Global beside an agency
		{"ag-dss", "ag-nwd", "global"}, // widened
		{"ag-nwd"},                     // narrowed, and out of Global
		{"global"},                     // and back
	} {
		if err := set(want...); err != nil {
			t.Fatalf("serve list %v: %v", want, err)
		}
		if got := serveList(t, pool, id); !slices.Equal(got, want) {
			t.Fatalf("after setting %v it serves %v", want, got)
		}
	}
	if err := set(); !errors.Is(err, ErrAgencyRequired) {
		t.Errorf("an empty serve list = %v, want ErrAgencyRequired", err)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"global"}) {
		t.Errorf("a refused write changed its serve list to %v", got)
	}
	// The same through an agency's member list: NWD takes it on, beside Global.
	if _, err := SetAgencyMembers(ctx, pool, "ag-nwd", refs([2]string{"runner", "r-legacy"}, [2]string{"runner", id}), "root@example.com"); err != nil {
		t.Fatalf("adding the local runner to an agency's members: %v", err)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"ag-nwd", "global"}) {
		t.Errorf("after joining NWD it serves %v, want NWD and Global", got)
	}
	// And Global takes it back the same way, while it still serves NWD.
	if err := set("ag-nwd"); err != nil {
		t.Fatal(err)
	}
	global, err := BuildAgencyDetail(ctx, pool, "global")
	if err != nil || global == nil {
		t.Fatalf("Global's members: %v", err)
	}
	members := []AgencyMemberRef{{Kind: "runner", ID: id}}
	for _, m := range global.Members {
		members = append(members, AgencyMemberRef{Kind: m.Kind, ID: m.ID})
	}
	if _, err := SetAgencyMembers(ctx, pool, "global", members, "root@example.com"); err != nil {
		t.Fatalf("adding the local runner back to Global's members: %v", err)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"ag-nwd", "global"}) {
		t.Errorf("after rejoining Global it serves %v, want NWD and Global", got)
	}
	// An agent still cannot hold Global beside an agency: the rule is the local runner's alone.
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: "r-global", AgencyIDs: []string{"global", "ag-dss"}}}, "root@example.com"); err == nil {
		t.Error("an agent was given Global beside an agency")
	}
	// Its owner is Global and stays Global, whatever it serves.
	if err := set("ag-dss"); err != nil {
		t.Fatal(err)
	}
	if err := SetRunnerOwner(ctx, pool, id, "ag-dss", "root@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
		t.Errorf("handing the local runner to the one agency it serves = %v, want ErrOwnerChangeRefused", err)
	}
	if o := ownerOf(t, pool, id); o != "global" {
		t.Errorf("the local runner's owner is %q", o)
	}
}
