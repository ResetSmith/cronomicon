package sshexec

import (
	"context"
	"testing"
	"time"
)

// LR Phase 0 found, on 2026-10-06, that the in-app SSH pool claimed with a query
// of its own that read neither the run's agency snapshot nor any runner
// membership: a scope in agency "finance", a bash run, no runners; the row
// carried agencies_json ["finance"], and the pool claimed it. Agency isolation
// was a property of the runner claim only. That was pinned here as a test.
//
// Phase B inverted it, and this is the inverse (LR-2, LR-40): the server claims
// through the one claim as the local runner, so an agency's run is the server's
// to take only once the local runner serves that agency — and then it is.
func TestTheLocalRunnerTakesAnAgencysRunOnlyOnceItServesThatAgency(t *testing.T) {
	svc, pool := probeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:fin','finance','` + now + `')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-hosts','cronomicon','` + now + `')`,
		`DELETE FROM scope_agencies WHERE scope_id = 'sc:fin'`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:fin')`,
		`INSERT INTO jobs (name, run_type, command, concurrency_policy, synced_at) VALUES ('ledger','bash','true','Allow','` + now + `')`,
		`INSERT INTO runs (id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		 VALUES ('run-fin','ledger','bash','fin-hosts','h1','queued','tester','manual','runner','["finance"]','` + now + `')`,
		`INSERT INTO run_agencies (run_id, agency) VALUES ('run-fin','finance')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// The local runner exists and serves Global, as it does from creation.
	// Finance's run is not Global's.
	r, err := claimAsLocal(t, svc, pool)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if r != nil {
		t.Fatalf("the local runner took finance's run while serving only Global: %+v", r)
	}
	var status string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='run-fin'`).Scan(&status)
	if status != "queued" {
		t.Fatalf("status = %q, want queued", status)
	}

	// A global administrator puts finance on its serve list.
	if _, err := pool.Exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, 'ag:fin')`, svc.localID); err != nil {
		t.Fatal(err)
	}
	r, err = claimAsLocal(t, svc, pool)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if r == nil || r.traceID != "run-fin" {
		t.Fatalf("serving finance, the local runner did not take finance's run (got %+v)", r)
	}
	var runnerID string
	_ = pool.QueryRow(`SELECT status, COALESCE(runner_id, '') FROM runs WHERE id='run-fin'`).Scan(&status, &runnerID)
	if status != "running" || runnerID != svc.localID {
		t.Errorf("status = %q, runner = %q; want running, claimed by the local runner's row", status, runnerID)
	}
	// The claim took a slot on its row, as it does for an agent; finishing the
	// run gives it back.
	var load int
	_ = pool.QueryRow(`SELECT load FROM runners WHERE id = ?`, svc.localID).Scan(&load)
	if load != 1 {
		t.Errorf("load after one claim = %d, want 1", load)
	}
	svc.finalize(context.Background(), *r, "success", nil)
	_ = pool.QueryRow(`SELECT load FROM runners WHERE id = ?`, svc.localID).Scan(&load)
	if load != 0 {
		t.Errorf("load after the run finished = %d, want 0", load)
	}
}
