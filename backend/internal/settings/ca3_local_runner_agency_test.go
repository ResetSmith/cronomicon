package settings

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// 2.5.3 CA3: the local runner belongs to one agency, given by a global
// administrator, or to none, which is off (CA-8 to CA-10).

func TestCA3TheLocalRunnerIsGivenToOneAgencyOrNone(t *testing.T) {
	pool := placementDB(t) // agencies ag-dss, ag-nwd
	ctx := context.Background()
	id, _, err := EnsureLocalRunner(ctx, pool, true, 4) // seeded on, as an upgrade from an install where it was
	if err != nil {
		t.Fatal(err)
	}

	// Born the built-in agency's: unassigned, so off whatever the switch says.
	if on, err := LocalRunnerEnabled(ctx, pool, false); err != nil || on {
		t.Fatalf("an unassigned local runner with its switch stored on is on = %v (%v), want off", on, err)
	}
	if left, err := LocalRunnerUnassignedButSwitchedOn(ctx, pool); err != nil || !left {
		t.Errorf("the state an upgrade arrives in is not reported (%v, %v)", left, err)
	}
	on := true
	if _, err := SetLocalRunner(ctx, pool, false, &on, nil, "root@example.com"); !errors.Is(err, ErrLocalRunnerUnassigned) {
		t.Errorf("turning on an unassigned local runner = %v, want ErrLocalRunnerUnassigned", err)
	}
	lr, err := GetLocalRunner(ctx, pool, false)
	if err != nil || lr.Owner != nil || lr.Enabled {
		t.Fatalf("the card of an unassigned local runner = %+v (%v), want no owner and off", lr, err)
	}

	// Given to DSS: owned by it, serving it alone, and on.
	if _, changed, err := SetLocalRunnerOwner(ctx, pool, "ag-dss", "root@example.com"); err != nil || !changed {
		t.Fatalf("giving it to DSS = %v (changed %v)", err, changed)
	}
	if o := ownerOf(t, pool, id); o != "ag-dss" {
		t.Errorf("its owner is %q, want ag-dss", o)
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"ag-dss"}) {
		t.Errorf("it serves %v, want DSS alone", got)
	}
	// Given, it is OFF: none of DSS's hosts' keys is approved for it yet, and
	// on it would claim DSS's waiting runs and fail them on their keys.
	if on, err := LocalRunnerEnabled(ctx, pool, false); err != nil || on {
		t.Errorf("given to DSS with its switch stored on, it is on = %v (%v), want turned off", on, err)
	}
	if stored, _ := localRunnerSwitch(ctx, pool); stored {
		t.Errorf("the change of agency left the stored switch on")
	}
	if _, err := SetLocalRunner(ctx, pool, false, &on, nil, "root@example.com"); err != nil {
		t.Fatalf("turning it on once given: %v", err)
	}
	if on, err := LocalRunnerEnabled(ctx, pool, false); err != nil || !on {
		t.Errorf("given and turned on, it is on = %v (%v)", on, err)
	}
	if left, _ := LocalRunnerUnassignedButSwitchedOn(ctx, pool); left {
		t.Errorf("an assigned local runner is still reported unassigned")
	}
	if lr, _ = GetLocalRunner(ctx, pool, false); lr.Owner == nil || lr.Owner.ID != "ag-dss" {
		t.Errorf("the card names owner %+v, want DSS", lr.Owner)
	}
	// Its serve list is its owner, and nothing else.
	if err := SetRunnerAgencies(ctx, pool, []RunnerAgencies{{RunnerID: id, AgencyIDs: []string{"ag-nwd"}}}, "root@example.com"); !errors.Is(err, ErrLocalRunnerGlobalOnly) {
		t.Errorf("serving another agency than its owner = %v, want refused", err)
	}
	if _, _, err := SetLocalRunnerOwner(ctx, pool, "ag-nowhere", "root@example.com"); !errors.Is(err, ErrUnknownAgency) {
		t.Errorf("giving it to an agency that is not there = %v, want ErrUnknownAgency", err)
	}

	// A host-key decision, a pending key, a scan request and a binding of
	// DSS's: what DSS approved is not NWD's to inherit.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	exec(`INSERT INTO scopes (id, name, source, created_at) VALUES ('sc-dss', 'dss-hosts', 'cronomicon', 't')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc-dss'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc-dss', 'ag-dss')`)
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_at) VALUES ('sc-dss', ?, 'Local runner', 't')`, id)
	exec(`INSERT INTO host_key_ledger (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line, decision, source, actor, decided_at)
	      VALUES ('b1', ?, 'Local runner', 'db1', 'ssh-ed25519', 'SHA256:x', 'db1 ssh-ed25519 AAAA', 'approved', 'scan', 'dss-admin', 't')`, id)
	exec(`INSERT INTO host_key_scan_targets (runner_id, target, requested_at) VALUES (?, 'db2', 't')`, id)
	exec(`INSERT INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at)
	      VALUES ('pk-1', ?, 'db3', 'ssh-ed25519', 'SHA256:y', 'db3 ssh-ed25519 BBBB', 't')`, id)

	// Refused while a scope is bound to it (the review of CA3): deleting the
	// binding would open DSS's scope to every DSS runner. DSS unbinds first.
	if _, _, err := SetLocalRunnerOwner(ctx, pool, "ag-nwd", "root@example.com"); err == nil {
		t.Fatal("giving it to NWD while a DSS scope is bound to it was not refused")
	} else if bound, ok := err.(*LocalRunnerBoundError); !ok || !slices.Equal(bound.Scopes, []string{"dss-hosts"}) {
		t.Errorf("the refusal = %v, want LocalRunnerBoundError naming dss-hosts", err)
	}
	exec(`DELETE FROM scope_runners WHERE runner_id = ?`, id)

	// Refused while it runs a run.
	exec(`INSERT INTO runs (id, job_name, run_type, status, triggered_by, trigger_kind, created_at, runner_id)
	      VALUES ('r-on', 'x', 'bash', 'running', 'u', 'manual', 't', ?)`, id)
	if _, _, err := SetLocalRunnerOwner(ctx, pool, "ag-nwd", "root@example.com"); !errors.Is(err, ErrLocalRunnerBusy) {
		t.Errorf("giving it to NWD while it runs a run = %v, want ErrLocalRunnerBusy", err)
	}
	if o := ownerOf(t, pool, id); o != "ag-dss" {
		t.Errorf("the refused change moved it to %q", o)
	}
	exec(`UPDATE runs SET status = 'success' WHERE id = 'r-on'`)

	if _, changed, err := SetLocalRunnerOwner(ctx, pool, "ag-nwd", "root@example.com"); err != nil || !changed {
		t.Fatalf("giving it to NWD = %v (changed %v)", err, changed)
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := pool.QueryRowContext(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`,
		`SELECT COUNT(*) FROM pending_host_keys WHERE runner_id = ?`,
		`SELECT COUNT(*) FROM host_key_scan_targets WHERE runner_id = ?`,
	} {
		if n := count(q); n != 0 {
			t.Errorf("after the change of agency, %s = %d, want 0", q, n)
		}
	}
	if got := serveList(t, pool, id); !slices.Equal(got, []string{"ag-nwd"}) {
		t.Errorf("it serves %v, want NWD alone", got)
	}
	for _, agency := range []string{"global", "ag-dss", "ag-nwd"} {
		var n int
		if err := pool.QueryRowContext(ctx, `SELECT COUNT(*) FROM activity WHERE agency_id = ?
		                                       AND summary LIKE 'local runner given to NWD (it was DSS''s)%'`, agency).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("the change from DSS to NWD is recorded %d times for %s, want once", n, agency)
		}
	}

	// Taken back: no agency, and off.
	if _, _, err := SetLocalRunnerOwner(ctx, pool, "", "root@example.com"); err != nil {
		t.Fatal(err)
	}
	if o := ownerOf(t, pool, id); o != "global" {
		t.Errorf("unassigned, its owner is %q, want the built-in agency's", o)
	}
	if on, _ := LocalRunnerEnabled(ctx, pool, false); on {
		t.Errorf("unassigned again, it is on")
	}
	// An agent's owner change is unchanged: still refused for the local runner.
	if err := SetRunnerOwner(ctx, pool, id, "ag-dss", "root@example.com"); !errors.Is(err, ErrOwnerChangeRefused) {
		t.Errorf("SetRunnerOwner on the local runner = %v, want ErrOwnerChangeRefused", err)
	}
}
