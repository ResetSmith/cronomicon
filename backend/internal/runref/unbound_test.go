package runref

import (
	"context"
	"strings"
	"testing"
)

// RA-24 — the unbound-run probe (the runas-update plan §11.1b).
//
// The property under test is PRECISION, not merely refusal. The plan's original
// wording ("a job with declared bindings triggered with no effective scope ⇒ 422")
// would refuse jobs that work perfectly well today — anything binding a global row.
// A guard that over-refuses gets switched off, so the tests that matter most here
// are the ones asserting what is NOT blocked.

func bind(t *testing.T, f *ownerFixture, owner Owner, bindings ...Binding) {
	t.Helper()
	if err := ReplaceBindings(context.Background(), f.pool, owner, bindings, "seed"); err != nil {
		t.Fatalf("replace bindings: %v", err)
	}
}

// TestUnboundBlockedOnlyByOwnedRows is the headline: an owned row blocks an unbound
// run because it resolves for nobody; a global row does not, because it resolves
// for everybody. Both jobs "declare reference bindings" — the blunt version of this
// rule would have refused them both.
func TestUnboundBlockedOnlyByOwnedRows(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-owned", "DEPT_PASSWORD", "", "ag-a", "owned")
	f.ownedSecret(t, "s-global", "SHARED_TOKEN", "", "", "global")

	ownedJob := Owner{Kind: "job", Source: "git", Name: "dept-job"}
	globalJob := Owner{Kind: "job", Source: "git", Name: "shared-job"}
	bind(t, f, ownedJob, Binding{Kind: KindSecret, Name: "DEPT_PASSWORD"})
	bind(t, f, globalJob, Binding{Kind: KindSecret, Name: "SHARED_TOKEN"})

	ctx := context.Background()

	blocked, err := UnboundRunBlocked(ctx, f.pool, []Owner{ownedJob}, "", nil)
	if err != nil {
		t.Fatalf("owned probe: %v", err)
	}
	if len(blocked) != 1 || blocked[0].Name != "DEPT_PASSWORD" {
		t.Errorf("owned binding on an unbound run = %v, want it blocked", blocked)
	}

	blocked, err = UnboundRunBlocked(ctx, f.pool, []Owner{globalJob}, "", nil)
	if err != nil {
		t.Fatalf("global probe: %v", err)
	}
	if len(blocked) != 0 {
		t.Errorf("GLOBAL binding on an unbound run = %v, want it allowed — over-refusing "+
			"here breaks jobs that work today", blocked)
	}
}

// TestBoundRunIsNotThisGuardsBusiness — the probe must no-op the moment a scope is
// bound, both because the failure it predicts cannot happen and because a bound run
// with a missing row is deliberately left to dispatch (the row is often created
// minutes later by someone else).
func TestBoundRunIsNotThisGuardsBusiness(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-owned", "DEPT_PASSWORD", "prod", "ag-a", "owned")
	job := Owner{Kind: "job", Source: "git", Name: "dept-job"}
	bind(t, f, job, Binding{Kind: KindSecret, Name: "DEPT_PASSWORD"})

	blocked, err := UnboundRunBlocked(context.Background(), f.pool,
		[]Owner{job}, "prod", []string{"TeamA"})
	if err != nil {
		t.Fatalf("bound probe: %v", err)
	}
	if len(blocked) != 0 {
		t.Errorf("bound run = %v blocked, want none — the owned row resolves for TeamA", blocked)
	}
}

// TestUnboundProbeCoversScriptBindings — dispatch resolves the job's bindings AND
// its script's (collectReferenceBindings). A probe that checked only the job would
// wave through a run that dies at dispatch for a script-declared reference: the
// exact failure this guard exists to pre-empt, reproduced by the guard itself.
func TestUnboundProbeCoversScriptBindings(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-owned", "DEPT_PASSWORD", "", "ag-a", "owned")

	job := Owner{Kind: "job", Source: "git", Name: "plain-job"}
	script := Owner{Kind: "script", Name: "play"}
	bind(t, f, script, Binding{Kind: KindSecret, Name: "DEPT_PASSWORD"})

	// Job alone: nothing declared, nothing blocked.
	blocked, err := UnboundRunBlocked(context.Background(), f.pool, []Owner{job}, "", nil)
	if err != nil {
		t.Fatalf("job-only probe: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("job with no bindings = %v, want none", blocked)
	}

	// The owner set RunOwners builds for a run whose job references that script.
	owners := RunOwners("git", "plain-job", "", "play")
	if len(owners) != 2 {
		t.Fatalf("RunOwners = %v, want job + script", owners)
	}
	// R2F-1: the script owner never carries a uid, whatever the job's identity is.
	if uid := RunOwners("git", "plain-job", "uid-plain-job", "play")[1].UIDKey(); uid != "" {
		t.Errorf("script owner UIDKey = %q, want empty", uid)
	}
	blocked, err = UnboundRunBlocked(context.Background(), f.pool, owners, "", nil)
	if err != nil {
		t.Fatalf("job+script probe: %v", err)
	}
	if len(blocked) != 1 {
		t.Errorf("script-declared owned binding = %v, want it blocked", blocked)
	}
}

// TestUnboundProbeTreatsAmbiguityAsBlocking — two departments owning one key is a
// dispatch-time fail-closed (RA-17). It is unusable for the run either way, so the
// probe must count it rather than let a resolution ERROR read as "fine".
func TestUnboundProbeTreatsAmbiguityAsBlocking(t *testing.T) {
	f := newOwnerFixture(t)
	f.ownedSecret(t, "s-a", "BECOME_PASSWORD", "prod", "ag-a", "a-pw")
	f.ownedSecret(t, "s-b", "BECOME_PASSWORD", "prod", "ag-b", "b-pw")
	job := Owner{Kind: "job", Source: "git", Name: "spanning"}
	bind(t, f, job, Binding{Kind: KindSecret, Name: "BECOME_PASSWORD"})

	// Bound to a scope whose snapshot spans BOTH owners: resolution is ambiguous.
	blocked, err := UnresolvableBindings(context.Background(), f.pool,
		[]Owner{job}, "prod", []string{"TeamA", "TeamB"})
	if err != nil {
		t.Fatalf("ambiguity probe returned an error rather than a verdict: %v", err)
	}
	if len(blocked) != 1 {
		t.Errorf("ambiguous binding = %v, want it counted as unusable", blocked)
	}
}

// TestUnboundRefusalNamesTheReference — the sentence must identify WHICH reference
// and state the remedy, and it is shared by three surfaces (trigger 422, scheduler
// skip record, workflow step failure) so they cannot describe one problem three
// ways.
func TestUnboundRefusalNamesTheReference(t *testing.T) {
	if got := UnboundRefusal(nil); got != "" {
		t.Errorf("no blockers = %q, want empty", got)
	}
	one := UnboundRefusal([]Binding{{Kind: KindSecret, Name: "DEPT_PASSWORD"}})
	for _, want := range []string{"DEPT_PASSWORD", "bind a scope"} {
		if !strings.Contains(one, want) {
			t.Errorf("refusal %q missing %q", one, want)
		}
	}
	if strings.Contains(one, "more)") {
		t.Errorf("single blocker should not claim there are more: %q", one)
	}
	two := UnboundRefusal([]Binding{
		{Kind: KindSecret, Name: "A"}, {Kind: KindVar, Name: "B"},
	})
	if !strings.Contains(two, "1 more") {
		t.Errorf("two blockers = %q, want it to count the rest", two)
	}
}
