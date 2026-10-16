package execspec

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// Until this test was inverted (present defect 4, found by reading in 2.4.0's
// Phase R0 and reproduced on 2026-10-09 by
// TestGR0_ResolveCommandCannotTellSameNamedInAppJobsApart; in the released
// 2.2.x) ResolveCommand, which is what turns a run into the command it
// executes, was handed the job's NAME and SOURCE and nothing else, although
// both of its callers (the agent's manifest and the local runner) held the
// run's job_uid. Two agencies may each own an in-app job of one name
// (migration 1050 took (source, name) uniqueness away from in-app jobs), so a
// run of one agency's job and a run of the other's resolved to the SAME body:
// one of the two was handed the other agency's command, and its history looked
// normal, because the run row named the right job.
//
// Now it takes the run's job uid, and each run resolves to its own job's body.
func TestResolveCommandTellsSameNamedJobsApart(t *testing.T) {
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
	for _, j := range [][3]string{
		{"uid-nightly-a", "nightly", bodyA}, {"uid-nightly-b", "nightly", bodyB},
		{"uid-weekly", "weekly", "echo the only weekly"},
	} {
		if _, err := pool.ExecContext(ctx,
			`INSERT INTO jobs(uid, name, source, run_type, command, synced_at)
			 VALUES(?, ?, 'cronomicon', 'bash', ?, '2026-01-01T00:00:00Z')`, j[0], j[1], j[2]); err != nil {
			t.Fatalf("seed job %s: %v (two in-app jobs of one name must be insertable)", j[0], err)
		}
	}
	resolve := func(uid, name string) (string, error) {
		_, body, err := ResolveCommand(ctx, pool, OneCloneDir(t.TempDir()), JobRef{UID: uid, Name: name, Source: "cronomicon"}, Pin{}, "bash")
		return body, err
	}

	// A run of each job, as dispatch asks: the same name, its own uid.
	for uid, want := range map[string]string{"uid-nightly-a": bodyA, "uid-nightly-b": bodyB} {
		if got, err := resolve(uid, "nightly"); err != nil || got != want {
			t.Errorf("a run of %s resolves to %q (%v), want its own job's body %q", uid, got, err, want)
		}
	}

	// A run of a job that has gone is not handed another job of its name.
	if got, err := resolve("uid-nightly-gone", "nightly"); !errors.Is(err, ErrJobGone) {
		t.Errorf("a run of a job that no longer exists resolves to %q (%v), want ErrJobGone", got, err)
	}

	// A run that records no uid: refused when two jobs hold its name, and the
	// one job's when one does.
	var amb *AmbiguousJobError
	if got, err := resolve("", "nightly"); !errors.As(err, &amb) || amb.Name != "nightly" {
		t.Errorf("a run with no uid, of a name two jobs hold, resolves to %q (%v), want the refusal to pick", got, err)
	}
	if got, err := resolve("", "weekly"); err != nil || got != "echo the only weekly" {
		t.Errorf("a run with no uid, of a name one job holds, resolves to %q (%v), want that job's body", got, err)
	}
}
