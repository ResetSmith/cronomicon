package workflow

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// CA-10 (Phase B) — resolveJobDef is the single read point that threads the
// job-spec "connect as" identity into the workflow engine's hand-built child-run
// INSERT (the TG-2 site). RP-7 — the gate there is IdentityCapableRunType, so an
// ansible step carries its identity (delivered as extra-vars) while a terraform
// step still cannot freeze one onto a run nothing would apply it to.
func TestResolveJobDefSSHIdentity(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "wf-identity.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	e := New(pool, discardLog())
	ctx := context.Background()

	seed := func(name, runType, sshUser, sshCred string) {
		t.Helper()
		if _, err := pool.Exec(
			`INSERT INTO jobs (uid, name, source, run_type, command, concurrency_policy, enabled, ssh_user, ssh_credential, synced_at)VALUES ('uid-'||?, ?, 'git', ?, 'echo hi', 'Allow', 1, ?, ?, 't')`, name, name, runType, sshUser, sshCred); err != nil {
			t.Fatalf("seed %q: %v", name, err)
		}
	}
	seed("bash-id", "bash", "deploy", "prod-key")
	seed("ansible-id", "ansible", "deploy", "prod-key")
	seed("tf-id", "terraform", "deploy", "prod-key")

	_, jd, ok := e.resolveJobDef(ctx, StepRef{Name: "bash-id"}, "git")
	if !ok {
		t.Fatal("bash-id did not resolve")
	}
	if jd.sshUser != "deploy" || jd.sshCred != "prod-key" {
		t.Errorf("bash jobDef identity = (%q,%q), want (deploy,prod-key)", jd.sshUser, jd.sshCred)
	}

	_, jd, ok = e.resolveJobDef(ctx, StepRef{Name: "ansible-id"}, "git")
	if !ok {
		t.Fatal("ansible-id did not resolve")
	}
	if jd.sshUser != "deploy" || jd.sshCred != "prod-key" {
		t.Errorf("ansible jobDef identity = (%q,%q), want (deploy,prod-key) — RP-7", jd.sshUser, jd.sshCred)
	}

	_, jd, ok = e.resolveJobDef(ctx, StepRef{Name: "tf-id"}, "git")
	if !ok {
		t.Fatal("tf-id did not resolve")
	}
	if jd.sshUser != "" || jd.sshCred != "" {
		t.Errorf("terraform jobDef identity = (%q,%q), want empty (RP-Q2)", jd.sshUser, jd.sshCred)
	}
}
