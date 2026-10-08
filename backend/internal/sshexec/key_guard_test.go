package sshexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// keyGuard (v2.2.3): the key a run connects with must be one its agency may
// use, by credential id and by name alike. Before this the SSH executor loaded
// whatever a host record named.

// guardFixture seeds two agencies, FIN's scope, and a key of each kind:
// TAX's, FIN's and a shared one. It returns the three credential ids and the
// public key of each, to tell signers apart.
func guardFixture(t *testing.T) (svc *Service, ids, pubs map[string]string) {
	t.Helper()
	svc, pool := probeFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO agencies (id,name,created_at) VALUES ('ag:fin','finance','` + now + `'), ('ag:tax','tax','` + now + `')`,
		`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:fin','fin-prod','cronomicon','` + now + `'), ('sc:lone','unowned','cronomicon','` + now + `')`,
		`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('sc:fin','ag:fin')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	ids, pubs = map[string]string{}, map[string]string{}
	for _, k := range []struct{ name, label, agency string }{
		{"tax", "taxkey", "ag:tax"}, {"fin", "finkey", "ag:fin"}, {"shared", "sharedkey", ""},
		// The same label held by FIN and as a shared key: FIN's own must win for FIN.
		{"fin-dup", "deploy", "ag:fin"}, {"shared-dup", "deploy", ""},
	} {
		signer, pem := newKey(t)
		// The store's uniqueness is (label, owner_agency): give the owner first.
		id := createCred(t, svc, k.label+"_"+strings.ReplaceAll(k.name, "-", "_"), pem)
		// A shared key is Global's (migration 1220); an owner is never empty.
		owner := k.agency
		if owner == "" {
			owner = "global"
		}
		if _, err := pool.Exec(`UPDATE ssh_credentials SET label = ?, owner_agency = ? WHERE id = ?`, k.label, owner, id); err != nil {
			t.Fatal(err)
		}
		if k.agency != "" {
			if _, err := pool.Exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?,?)`, id, k.agency); err != nil {
				t.Fatal(err)
			}
		}
		ids[k.name], pubs[k.name] = id, string(signer.PublicKey().Marshal())
	}
	return svc, ids, pubs
}

func TestKeyGuardChecksAKeyNamedByID(t *testing.T) {
	svc, ids, pubs := guardFixture(t)
	ctx := context.Background()
	fin := keyGuard{checked: true, scope: "fin-prod", agencies: []string{"finance"}}
	load := func(g keyGuard, cred string) (string, error) {
		s, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, cred, "", g)
		if err != nil {
			return "", err
		}
		return string(s.PublicKey().Marshal()), nil
	}

	if _, err := load(fin, ids["tax"]); !errors.Is(err, errKeyNotUsable) {
		t.Errorf("a finance run loading TAX's key by id: err = %v, want errKeyNotUsable", err)
	}
	// A missing id and another agency's key read the same.
	if _, err := load(fin, "no-such-credential"); !errors.Is(err, errKeyNotUsable) {
		t.Errorf("a finance run loading a missing id: err = %v, want errKeyNotUsable", err)
	}
	for _, own := range []string{"fin", "shared"} {
		if got, err := load(fin, ids[own]); err != nil || got != pubs[own] {
			t.Errorf("a finance run loading the %s key: err = %v", own, err)
		}
	}
	// A snapshot with no agency at all (a finished pre-2.3.0 row, never a new
	// run) may use Global's keys only.
	none := keyGuard{checked: true}
	if _, err := load(none, ids["fin"]); !errors.Is(err, errKeyNotUsable) {
		t.Errorf("a run with no agency loading FIN's key: err = %v, want errKeyNotUsable", err)
	}
	if _, err := load(none, ids["shared"]); err != nil {
		t.Errorf("a run with no agency loading the shared key: %v", err)
	}
	// Unchecked (a bastion, a manually authored record's probe) is as before.
	if _, err := load(keyGuard{}, ids["tax"]); err != nil {
		t.Errorf("an unchecked load of TAX's key: %v", err)
	}
}

func TestKeyGuardChecksAKeyNamedByName(t *testing.T) {
	svc, _, pubs := guardFixture(t)
	ctx := context.Background()
	fin := keyGuard{checked: true, scope: "fin-prod", agencies: []string{"finance"}}
	load := func(g keyGuard, name string) (string, error) {
		s, err := loadSigner(ctx, svc.db, svc.cfg, svc.sec, "", name, g)
		if err != nil {
			return "", err
		}
		return string(s.PublicKey().Marshal()), nil
	}

	// The inventory line an agency administrator can write for their own scope:
	// cronomicon_auth_key_env_var=CRONOMICON_KEY_taxkey.
	if _, err := load(fin, "CRONOMICON_KEY_taxkey"); !errors.Is(err, errKeyNotUsable) {
		t.Errorf("a finance run naming TAX's key: err = %v, want errKeyNotUsable", err)
	}
	if got, err := load(fin, "CRONOMICON_KEY_finkey"); err != nil || got != pubs["fin"] {
		t.Errorf("a finance run naming its own key: err = %v", err)
	}
	// One label, FIN's and a shared one: FIN's run gets FIN's; TAX's run gets the
	// shared one. `LIMIT 1` gave whichever row came first to both.
	if got, err := load(fin, "CRONOMICON_KEY_deploy"); err != nil || got != pubs["fin-dup"] {
		t.Errorf("a finance run naming `deploy` did not get FIN's own (err = %v)", err)
	}
	tax := keyGuard{checked: true, scope: "tax-prod", agencies: []string{"tax"}}
	if got, err := load(tax, "CRONOMICON_KEY_deploy"); err != nil || got != pubs["shared-dup"] {
		t.Errorf("a tax run naming `deploy` did not get the shared key (err = %v)", err)
	}

	// A private key kept as a variable is scoped: another scope's is not this run's.
	_, pem := newKey(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := svc.db.Exec(`INSERT INTO env_vars (id, key, scope, value, created_at) VALUES ('v-tax','TAX_PEM','tax-prod',?,?), ('v-fin','FIN_PEM','fin-prod',?,?)`,
		pem, now, pem, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TAX_PEM", "CRONOMICON_VAR_TAX_PEM"} {
		if _, err := load(fin, name); !errors.Is(err, errKeyNotUsable) {
			t.Errorf("a fin-prod run naming %s (tax-prod's variable): err = %v, want errKeyNotUsable", name, err)
		}
	}
	for _, name := range []string{"FIN_PEM", "CRONOMICON_VAR_FIN_PEM"} {
		if _, err := load(fin, name); err != nil {
			t.Errorf("a fin-prod run naming %s (its own scope's variable): %v", name, err)
		}
	}
}

// The guard a "Test connection" gets: a record imported for a scope is checked
// against that scope's agencies; a manually authored one (a global
// administrator's) is not.
func TestHostKeyGuardFollowsTheRecordsScope(t *testing.T) {
	svc, _, _ := guardFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := svc.db.Exec(`INSERT INTO ssh_hosts (id, hostname, port, source, scope_id, created_at) VALUES
		('h-fin','fin01',22,'cronomicon','sc:fin',?), ('h-lone','lone01',22,'cronomicon','sc:lone',?), ('h-manual','db01',22,'cronomicon',NULL,?)`,
		now, now, now); err != nil {
		t.Fatal(err)
	}
	g, err := hostKeyGuard(ctx, svc.db, "h-fin")
	if err != nil || !g.checked || g.scope != "fin-prod" || len(g.agencies) != 1 || g.agencies[0] != "finance" {
		t.Errorf("guard for FIN's host = %+v, %v", g, err)
	}
	// A scope nobody assigned is Global's (migration 1220): checked, as Global —
	// which may use Global's (shared) keys and no department's.
	if g, err := hostKeyGuard(ctx, svc.db, "h-lone"); err != nil || !g.checked || len(g.agencies) != 1 || g.agencies[0] != "Global" {
		t.Errorf("guard for a Global scope's host = %+v, %v", g, err)
	}
	// A record written by hand has an owner since 2.3.0 (LR-69) — Global, for one
	// that predates it — and its key is checked against that owner (LR-72). It was
	// unchecked while such a record belonged to nobody.
	if g, err := hostKeyGuard(ctx, svc.db, "h-manual"); err != nil || !g.checked || g.scope != "" || len(g.agencies) != 1 || g.agencies[0] != "Global" {
		t.Errorf("guard for a Global hand-written record = %+v, %v; want checked as Global", g, err)
	}
	if _, err := svc.db.Exec(`INSERT INTO ssh_hosts (id, hostname, port, source, created_at, owner_agency) VALUES ('h-fin-manual','jump01',22,'cronomicon',?,'ag:fin')`, now); err != nil {
		t.Fatal(err)
	}
	if g, err := hostKeyGuard(ctx, svc.db, "h-fin-manual"); err != nil || !g.checked || len(g.agencies) != 1 || g.agencies[0] != "finance" {
		t.Errorf("guard for FIN's hand-written record = %+v, %v; want checked as finance", g, err)
	}
}

// End to end: a finance run whose host record names TAX's key does not connect.
// The run fails for that host, with the reason in its log.
func TestARunDoesNotConnectWithAnotherAgencysKey(t *testing.T) {
	svc, ids, _ := guardFixture(t)
	pool := svc.db
	now := time.Now().UTC().Format(time.RFC3339)
	for _, q := range []string{
		// A manually authored record that names TAX's key, reachable from every scope.
		`INSERT INTO ssh_hosts (id, hostname, address, port, username, auth_credential_id, source, created_at)
		 VALUES ('h-db01','db01','192.0.2.50',22,'deploy','` + ids["tax"] + `','cronomicon','` + now + `')`,
		// db01 is one of the scope's hosts (LR-71: a fixed target is a member).
		`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin','db01')`,
		`INSERT INTO jobs (name, run_type, command, concurrency_policy, synced_at) VALUES ('ledger','bash','true','Allow','` + now + `')`,
		`INSERT INTO runs (id, job_name, run_type, scope, target_host, status, triggered_by, trigger_kind, executor, agencies_json, created_at)
		 VALUES ('run-fin','ledger','bash','fin-prod','db01','queued','tester','manual','runner','["finance"]','` + now + `')`,
		`INSERT INTO run_agencies (run_id, agency) VALUES ('run-fin','finance')`,
	} {
		if _, err := pool.Exec(q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// The run is finance's: the local runner takes it once it serves finance.
	if claimed, err := claimAsLocal(t, svc, pool); err != nil || claimed != nil {
		t.Fatalf("the local runner took finance's run before serving finance: %v, %v", claimed, err)
	}
	if _, err := pool.Exec(`INSERT INTO runner_agencies (runner_id, agency_id)
	                        SELECT ?, id FROM agencies WHERE name = 'finance'`, svc.localID); err != nil {
		t.Fatal(err)
	}
	claimed, err := claimAsLocal(t, svc, pool)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v, %v", claimed, err)
	}
	svc.execute(context.Background(), *claimed)

	var status string
	_ = pool.QueryRow(`SELECT status FROM runs WHERE id='run-fin'`).Scan(&status)
	if status != "failure" {
		t.Errorf("run status = %q, want failure (it must not have connected with TAX's key)", status)
	}
	logBytes, _ := os.ReadFile(filepath.Join(*svc.logDir.Load(), "run-fin.log"))
	if !strings.Contains(string(logBytes), "not one its agency may use") {
		t.Errorf("the run log does not say why:\n%s", logBytes)
	}
}
