package sshexec

import (
	"context"
	"testing"
	"time"
)

// 2.5.1 SG2.2: a key the local runner connects a run with is recorded as the
// run's use of it — as the host's or the bastion's — once per run; a probe
// (Test connection) records nothing.
func TestTheLocalRunnerRecordsTheKeysARunConnectsWith(t *testing.T) {
	svc, ids, _ := guardFixture(t)
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := svc.db.Exec(`INSERT INTO runs(id, job_name, job_uid, run_type, status, triggered_by, trigger_kind, created_at, runner_name)
		VALUES('run-k','deploy','job-uid-k','bash','running','ops@x','manual',?, 'local')`, now); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	fin := keyGuard{checked: true, scope: "fin-prod", agencies: []string{"finance"}}
	count := func(via string) int {
		var n int
		if err := svc.db.QueryRow(`SELECT COUNT(*) FROM secret_uses WHERE trace_id='run-k' AND via=? AND row_id=?`, via, ids["fin"]).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// A probe: no run on the context, nothing recorded.
	if _, err := loadSigner(context.Background(), svc.db, svc.cfg, svc.sec, ids["fin"], "", fin); err != nil {
		t.Fatalf("probe load: %v", err)
	}
	if n := count("host_auth"); n != 0 {
		t.Fatalf("a probe recorded %d use(s)", n)
	}

	run := withKeyUse(context.Background(), "run-k", "host_auth")
	for i := 0; i < 3; i++ { // three hosts, one key: one use
		if _, err := loadSigner(run, svc.db, svc.cfg, svc.sec, ids["fin"], "", fin); err != nil {
			t.Fatalf("host load: %v", err)
		}
	}
	if n := count("host_auth"); n != 1 {
		t.Fatalf("host key recorded %d time(s), want 1", n)
	}
	// By NAME (a host record's auth_key_env_var), through the checked path.
	if _, err := loadSigner(withKeyVia(run, "bastion_auth"), svc.db, svc.cfg, svc.sec, "", "CRONOMICON_KEY_finkey", fin); err != nil {
		t.Fatalf("bastion load by name: %v", err)
	}
	if n := count("bastion_auth"); n != 1 {
		t.Fatalf("bastion key recorded %d time(s), want 1", n)
	}
	var owner, name, job string
	if err := svc.db.QueryRow(`SELECT owner_agency, ref_name, job_uid FROM secret_uses WHERE trace_id='run-k' AND via='host_auth'`).Scan(&owner, &name, &job); err != nil {
		t.Fatal(err)
	}
	if owner != "ag:fin" || name != "finkey" || job != "job-uid-k" {
		t.Errorf("recorded owner %q name %q job %q", owner, name, job)
	}
	// A key the run may not use is refused before anything is recorded.
	if _, err := loadSigner(run, svc.db, svc.cfg, svc.sec, ids["tax"], "", fin); err == nil {
		t.Fatal("TAX's key loaded for a FIN run")
	}
	var taxUses int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM secret_uses WHERE row_id=?`, ids["tax"]).Scan(&taxUses)
	if taxUses != 0 {
		t.Errorf("a refused key was recorded as used")
	}
}
