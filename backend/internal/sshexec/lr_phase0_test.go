package sshexec

import (
	"context"
	"testing"
	"time"
)

// LR Phase 0 — today's behaviour, pinned before Phase B changes it.
//
// The in-app SSH pool claims with its own query, and that query reads neither
// the run's agency snapshot nor any runner membership: it asks for a queued
// shell run with executor='ssh' and nothing else. A run that belongs to an
// agency is therefore dispatched from the server whether or not anything was
// ever placed in that agency — agency isolation is a property of the runner
// claim (runner/poll.go claimRun) only.
//
// This is the 2026-10-06 probe: a scope in agency "finance", a bash run, no
// runners; the resolver sent it to ssh, the row carried agencies_json
// ["finance"], and this pool claimed it.
//
// Phase B inverts it: the pool claims through claimRun as the local runner, so
// the run is claimable only once the local runner serves "finance" (LR-2,
// LR-40). When that lands, this test becomes "not claimed until placed".
func TestLR0_TheSSHPoolClaimsAnAgencyTaggedRunWithNoAgencyCheck(t *testing.T) {
	svc, pool := probeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:fin','finance','` + now + `')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-hosts','cronomicon','` + now + `')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:fin')`,
		`INSERT INTO jobs (name, run_type, command, concurrency_policy, synced_at) VALUES ('ledger','bash','true','Allow','` + now + `')`,
		`INSERT INTO runs (id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		 VALUES ('run-fin','ledger','bash','fin-hosts','h1','queued','tester','manual','ssh','["finance"]','` + now + `')`,
		`INSERT INTO run_agencies (run_id, agency) VALUES ('run-fin','finance')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Nothing serves finance: no runner exists at all, so no membership does.
	var runners, members int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&runners)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runner_agencies`).Scan(&members)
	if runners != 0 || members != 0 {
		t.Fatalf("precondition: runners=%d memberships=%d, want none", runners, members)
	}

	r, err := svc.claim(context.Background())
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if r == nil || r.traceID != "run-fin" {
		t.Fatalf("PIN: the SSH pool did not claim the agency-tagged run (got %+v). "+
			"If the pool now claims through claimRun (LR-40), replace this pin with its inverse.", r)
	}
	var status string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='run-fin'`).Scan(&status)
	if status != "running" {
		t.Errorf("status = %q, want running", status)
	}
}
