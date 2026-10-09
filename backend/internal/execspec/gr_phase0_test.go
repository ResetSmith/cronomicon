package execspec

// Phase R0 of 2.4.0 found this by reading; this test reproduces it. It passes
// on the code as it is and names the phase that inverts it. No production code
// changes.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// Two agencies may each own an in-app job of one name (migration 1050 took
// (source, name) uniqueness away from in-app jobs). A run knows which of the
// two it is: runs.job_uid. But ResolveCommand, which is what turns a run into
// the command it executes, is handed the job's NAME and SOURCE and nothing
// else, and both of its callers (the agent's manifest and the local runner)
// hold the run's job_uid and do not pass it. So a run of one agency's job and
// a run of the other's resolve to the SAME body: one of the two is handed the
// other agency's command, and its history looks normal, because the run row
// names the right job.
//
// Phase R5 inverts this: ResolveCommand takes the run's job uid. Until then
// this is a present defect, and it is older than 2.4.0.
func TestGR0_ResolveCommandCannotTellSameNamedInAppJobsApart(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	const (
		bodyA = "echo this is agency A's nightly"
		bodyB = "echo this is agency B's nightly"
	)
	for _, j := range [][2]string{{"uid-nightly-a", bodyA}, {"uid-nightly-b", bodyB}} {
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO jobs(uid, name, source, run_type, command, synced_at)
			 VALUES(?, 'nightly', 'cronomicon', 'bash', ?, '2026-01-01T00:00:00Z')`, j[0], j[1]); err != nil {
			t.Fatalf("seed job %s: %v (two in-app jobs of one name must be insertable)", j[0], err)
		}
	}

	// What dispatch can ask, for a run of EITHER job: the arguments are the
	// same, because the name and the source are all it passes.
	resolve := func() string {
		t.Helper()
		_, body, err := ResolveCommand(ctx, pool, t.TempDir(), "nightly", "cronomicon", "bash")
		if err != nil {
			t.Fatalf("ResolveCommand: %v", err)
		}
		return body
	}
	forRunOfA, forRunOfB := resolve(), resolve()

	if forRunOfA != bodyA && forRunOfA != bodyB {
		t.Fatalf("resolved body %q is neither job's", forRunOfA)
	}
	if forRunOfA != forRunOfB {
		t.Fatalf("the two resolutions differ (%q, %q): the lookup is not even stable", forRunOfA, forRunOfB)
	}
	// One body for both runs: whichever job it belongs to, the other job's run
	// is handed it.
	wrong := "B"
	if forRunOfA == bodyB {
		wrong = "A"
	}
	t.Logf("a run of agency %s's job would execute %q", wrong, forRunOfA)
}
