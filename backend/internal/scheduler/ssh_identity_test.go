package scheduler

import (
	"context"
	"database/sql"
	"testing"
)

// CA-10 (the ssh-user plan Phase B) — fire()'s per-fire read carries
// the job-spec "connect as" identity onto the run, exactly like the TG-1
// target_host pin, gated to ssh-family run types.

func seedIdentityJobRow(t *testing.T, pool *sql.DB, name, runType, sshUser, sshCred string) {
	t.Helper()
	if _, err := pool.ExecContext(context.Background(),
		`INSERT INTO jobs (uid, name, run_type, concurrency_policy, enabled, ssh_user, ssh_credential, synced_at)VALUES ('uid-'||?, ?, ?, 'Allow', 1, ?, ?, 't')`, name, name, runType, sshUser, sshCred); err != nil {
		t.Fatalf("seed identity job %q: %v", name, err)
	}
}

func TestFirePersistsJobSSHIdentity(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedIdentityJobRow(t, pool, "idjob", "bash", "deploy", "prod-key")

	s := New(pool, quietLog(), nil)
	s.fire("git", "idjob", "", "bash", "prod", "Allow", "", "default", "")

	var sshUser, sshCred sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE job_name='idjob'`).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.String != "deploy" || sshCred.String != "prod-key" {
		t.Errorf("frozen identity = (%q,%q), want (deploy,prod-key)", sshUser.String, sshCred.String)
	}
}

// A job with no identity must enqueue with NULL columns — not empty strings —
// like every other optional snapshot column.
func TestFireNoJobSSHIdentityStaysNull(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedJobRow(t, pool, "plainjob", 1)

	s := New(pool, quietLog(), nil)
	s.fire("git", "plainjob", "", "bash", "prod", "Allow", "", "default", "")

	var sshUser, sshCred sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE job_name='plainjob'`).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.Valid || sshCred.Valid {
		t.Errorf("identity columns = (%v,%v), want NULLs", sshUser, sshCred)
	}
}

// RP-7 — a scheduled ANSIBLE job now folds its stored identity like any
// ssh-family job: the dispatch path delivers it as connection extra-vars. This
// is the scheduled-run half of the parity work — a fix applied only at the
// manual trigger would leave scheduled runs connecting as someone else.
func TestFireAnsibleJobSSHIdentityFolded(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedIdentityJobRow(t, pool, "ansjob", "ansible", "deploy", "prod-key")

	s := New(pool, quietLog(), nil)
	s.fire("git", "ansjob", "", "ansible", "prod", "Allow", "", "default", "")

	var sshUser, sshCred sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE job_name='ansjob'`).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.String != "deploy" || sshCred.String != "prod-key" {
		t.Errorf("ansible run identity = (%q,%q), want (deploy,prod-key)", sshUser.String, sshCred.String)
	}
}

// RP-Q2 — terraform stays excluded: a value that reached the row via Git sync
// (which warns but never blocks) must be dropped at the fold rather than frozen
// onto a run no dispatch path would apply it to.
func TestFireTerraformJobSSHIdentityNotFolded(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedIdentityJobRow(t, pool, "tfjob", "terraform", "deploy", "prod-key")

	s := New(pool, quietLog(), nil)
	s.fire("git", "tfjob", "", "terraform", "prod", "Allow", "", "default", "")

	var sshUser, sshCred sql.NullString
	if err := pool.QueryRowContext(ctx,
		`SELECT ssh_user, ssh_credential FROM runs WHERE job_name='tfjob'`).Scan(&sshUser, &sshCred); err != nil {
		t.Fatalf("fetch run: %v", err)
	}
	if sshUser.Valid || sshCred.Valid {
		t.Errorf("terraform run identity = (%v,%v), want NULLs (not folded)", sshUser, sshCred)
	}
}
