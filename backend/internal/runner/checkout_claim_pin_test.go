package runner

import (
	"context"
	"database/sql"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/scheduler"
)

// TestGR0_ACheckoutRunIsClaimedByAnAgentThatDoesNotAllowCheckout pins what the
// released code does (read while grounding 2.4.0 Phase R5; reproduced here on
// 2026-10-12, not changed). A checkout run can only be run by an agent started
// with -allow-checkout, which advertises the token "checkout". Nothing asks for
// that token at the claim: the run of a project job records no requirement, so
// any agent with the run type takes it, and one that does not allow checkout
// then refuses it and the run fails. On a scope served by both kinds of agent
// the same job passes or fails by which agent polls first.
//
// A become password is held for a capable agent in exactly this way (the
// "become-file" token, written into the run's requirements at enqueue). Whether
// a checkout run should be is the owner's to decide: this test is inverted by
// that change.
func TestGR0_ACheckoutRunIsClaimedByAnAgentThatDoesNotAllowCheckout(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	if _, err := svc.db.ExecContext(ctx,
		`UPDATE git_repos SET last_sha = 'deadbeefdeadbeefdeadbeefdeadbeefdeadbeef' WHERE id = 'global'`); err != nil {
		t.Fatalf("seed the last synced commit: %v", err)
	}
	if _, err := svc.db.ExecContext(ctx, `
		INSERT INTO jobs (uid, name, run_type, script_path, project_root, concurrency_policy, synced_at)
		VALUES ('uid-proj-job', 'proj-job', 'ansible', 'scripts/proj/site.yml', 'scripts/proj', 'Allow', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed a project job: %v", err)
	}
	trace, err := scheduler.EnqueueRunWithID(ctx, svc.db, scheduler.EnqueueParams{
		JobName: "proj-job", RunType: "ansible", TriggerKind: "manual", TriggeredBy: "a@b.c",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// It is a checkout run, and it asks for nothing.
	var sha, requires sql.NullString
	if err := svc.db.QueryRowContext(ctx,
		`SELECT checkout_sha, requires_json FROM runs WHERE id = ?`, trace).Scan(&sha, &requires); err != nil {
		t.Fatal(err)
	}
	if sha.String == "" {
		t.Fatalf("the run of a project job pinned no commit: it is not a checkout run, and this test pins nothing")
	}
	if requires.Valid && requires.String != "[]" {
		t.Fatalf("PRESENT DEFECT CHANGED: the checkout run records requirements %q; it recorded none", requires.String)
	}

	// An agent with Ansible and WITHOUT -allow-checkout takes it.
	insertRunner(t, svc, "r-plain", "r-plain", "online", []string{"ansible"})
	got, err := svc.claimRun(ctx, "r-plain", []string{"ansible"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.TraceID != trace {
		t.Fatalf("PRESENT DEFECT CHANGED: an agent that does not allow checkout no longer claims a checkout run (claimed %+v). "+
			"If checkout became a claim requirement, invert this test.", got)
	}
}
