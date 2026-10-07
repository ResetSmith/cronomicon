package sshexec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/execspec"
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

// A scope in one agency still RESOLVES a manually authored host record that
// names another agency's key: execspec.HostByName filters imported rows by
// scope and takes a manual row (scope_id NULL) for every scope, and never
// reads the run's agency. This is the §2.4 item "Host records and keys".
//
// What it can no longer do is connect with that key. v2.2.3 closed that half:
// the SSH executor loads every key through the agency-checked resolver with the
// run's agencies (keyGuard), so the key below is refused for a finance run.
//
// Phase G2 inverts the half pinned here: host records gain an owner and
// HostByName an owner filter (LR-69, LR-70), so a scope resolves only its own
// agency's records and Global's.
func TestLR0_AScopeResolvesAManualHostRecordThatNamesAnotherAgencysKey(t *testing.T) {
	svc, pool := probeFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	_, taxPEM := newKey(t)
	credID := createCred(t, svc, "taxkey", taxPEM)
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:fin','finance','` + now + `'), ('ag:tax','tax','` + now + `')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-prod','cronomicon','` + now + `')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:fin')`,
		// The key is TAX's, by owner and by membership.
		`UPDATE ssh_credentials SET owner_agency='ag:tax' WHERE id='` + credID + `'`,
		`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('` + credID + `','ag:tax')`,
		// A manual record: source cronomicon, no scope. It names TAX's key.
		`INSERT INTO ssh_hosts (id, hostname, address, port, username, auth_credential_id, source, created_at)
		 VALUES ('h-db01','db01','192.0.2.50',22,'deploy','` + credID + `','cronomicon','` + now + `')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// FIN's scope resolves the record and is handed TAX's credential id.
	targets, err := execspec.ResolveTargets(ctx, pool, "fin-prod", "db01", nil)
	if err != nil || len(targets) != 1 {
		t.Fatalf("ResolveTargets = %v, %v; want one target", targets, err)
	}
	if targets[0].AuthCredentialID != credID {
		t.Fatalf("PIN: FIN's scope did not resolve the record that names TAX's key (credential %q). "+
			"If HostByName now filters by owner (LR-70), replace this pin with its inverse.", targets[0].AuthCredentialID)
	}

	// Since v2.2.3 a finance run cannot connect with it.
	fin := keyGuard{checked: true, scope: "fin-prod", agencies: []string{"finance"}}
	if _, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, targets[0].AuthCredentialID, "", fin); !errors.Is(err, errKeyNotUsable) {
		t.Errorf("a finance run loaded TAX's key through the record (err = %v); the v2.2.3 key guard is gone", err)
	}
}
