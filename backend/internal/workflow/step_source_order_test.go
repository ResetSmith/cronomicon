package workflow_test

import (
	"testing"

	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// R2-3 — the A11 precedence, now shared between execution and authorization.
//
// The bug this export fixes: compose-time scope checks resolved a step with
// `ORDER BY source LIMIT 1` (alphabetical — 'cronomicon' always won) while the
// engine resolves by this order. For a GIT-source workflow the two disagree, so
// the actor was authorized against one job's scope and a different, same-named
// job executed. Same-name-across-sources is legal today, which makes that a
// live gap rather than a future one.
func TestStepSourceOrder(t *testing.T) {
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	if got := workflow.StepSourceOrder("", "cronomicon"); !eq(got, []string{"cronomicon", "git"}) {
		t.Errorf("cronomicon workflow order = %v, want [cronomicon git]", got)
	}
	// The case alphabetical resolution got wrong.
	if got := workflow.StepSourceOrder("", "git"); !eq(got, []string{"git", "cronomicon"}) {
		t.Errorf("git workflow order = %v, want [git cronomicon] — alphabetical order would say the opposite", got)
	}
	// An explicit per-step override is absolute: no fallback to the other pool.
	if got := workflow.StepSourceOrder("git", "cronomicon"); !eq(got, []string{"git"}) {
		t.Errorf("overridden order = %v, want [git] only", got)
	}
}
