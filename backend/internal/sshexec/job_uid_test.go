package sshexec

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
)

// The local runner's half of present defect 4, for the timeout it enforces: a
// run of one of two jobs of a name was given the other job's timeout. (The
// command it runs is execspec.ResolveCommand, which has its own test.)
func TestTheLocalRunnerReadsItsOwnJobsTimeout(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "ssh.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := db.Migrate(pool); err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct {
		uid, name string
		secs      int
	}{{"uid-a", "nightly", 30}, {"uid-b", "nightly", 900}, {"uid-w", "weekly", 60}} {
		if _, err := pool.Exec(`INSERT INTO jobs(uid, name, source, run_type, command, timeout_seconds, synced_at)
			VALUES (?, ?, 'cronomicon', 'bash', 'true', ?, 't')`, j.uid, j.name, j.secs); err != nil {
			t.Fatalf("seed %s: %v", j.uid, err)
		}
	}
	s := &Service{db: pool}
	ctx := context.Background()
	for uid, want := range map[string]time.Duration{"uid-a": 30 * time.Second, "uid-b": 900 * time.Second} {
		if got := s.jobTimeout(ctx, uid, "nightly", "cronomicon"); got != want {
			t.Errorf("the timeout of a run of %s is %s, want its own job's, %s", uid, got, want)
		}
	}
	// A run of a job that has gone has no timeout: not another job's of its name.
	if got := s.jobTimeout(ctx, "uid-gone", "nightly", "cronomicon"); got != 0 {
		t.Errorf("the timeout of a run of a job that no longer exists is %s, want none", got)
	}
	// A run that records no uid is still found by its name.
	if got := s.jobTimeout(ctx, "", "weekly", "cronomicon"); got != 60*time.Second {
		t.Errorf("the timeout of a run with no uid is %s, want the job's of its name, 1m0s", got)
	}
}
