package scheduler

import (
	"context"
	"testing"
)

// R2-3 — the gate holds across producers now that the key is the job's uid.
//
// RX-25's lesson is that the producers must AGREE, not merely each be correct
// in isolation: a producer composing the key differently neither queues behind
// the others nor collides with them, so the job silently runs twice. These
// tests take the two paths that can fire the same job simultaneously and assert
// they still meet at the gate — the property the re-key could plausibly break.

func TestGateHoldsBetweenCronFireAndReactor(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedQueueJob(t, pool, "guarded", "Forbid")
	if _, err := pool.Exec(`UPDATE jobs SET uid='uid-guarded' WHERE name='guarded' AND source='git'`); err != nil {
		t.Fatalf("set uid: %v", err)
	}

	// A cron fire lands first, holding the gate under the uid key.
	if err := EnqueueRun(ctx, pool, EnqueueParams{
		JobName: "guarded", JobSource: "git", RunType: "bash",
		TriggerKind: "scheduled", TriggeredBy: "scheduler",
		ConcurrencyKey: "uid-guarded", Policy: "Forbid",
	}); err != nil {
		t.Fatalf("cron fire: %v", err)
	}

	// The reactor now builds its own params for the same job. The key it computes
	// must be the one already held, or the Forbid check below is meaningless.
	s := New(pool, quietLog(), nil)
	params, err := s.buildReactionJobParams(ctx, "git", "guarded", "", map[string]string{})
	if err != nil {
		t.Fatalf("buildReactionJobParams: %v", err)
	}
	if params.ConcurrencyKey != "uid-guarded" {
		t.Fatalf("reactor key = %q, want uid-guarded — the producers disagree (RX-25)", params.ConcurrencyKey)
	}
	conflict, err := CheckForbid(ctx, pool, params.ConcurrencyKey)
	if err != nil {
		t.Fatalf("CheckForbid: %v", err)
	}
	if !conflict {
		t.Error("the reactor's key did not see the cron fire's in-flight run — the gate is open")
	}
}

// TestGateFallsBackWithoutUID pins the fallback arm: a job with no uid still
// gates, on the pre-R2-3 composition. Without this a uid-less job would compute
// an EMPTY key, drop out of the partial unique index entirely, and quietly
// behave as though its policy were Allow.
func TestGateFallsBackWithoutUID(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedQueueJob(t, pool, "legacy", "Forbid")
	if _, err := pool.Exec(`UPDATE jobs SET uid = NULL WHERE name='legacy' AND source='git'`); err != nil {
		t.Fatalf("clear uid: %v", err)
	}

	s := New(pool, quietLog(), nil)
	params, err := s.buildReactionJobParams(ctx, "git", "legacy", "", map[string]string{})
	if err != nil {
		t.Fatalf("buildReactionJobParams: %v", err)
	}
	if params.ConcurrencyKey != "git/legacy" {
		t.Errorf("uid-less job key = %q, want git/legacy", params.ConcurrencyKey)
	}
}

// TestScheduleJoinSurvivesNullOwnerUID guards the reload join's fallback arm.
// A schedule entry that drops out of that join does not error — it simply never
// fires again, which is the quietest possible failure and the reason the join
// kept its name arm.
func TestScheduleJoinSurvivesNullOwnerUID(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	seedQueueJob(t, pool, "sched-me", "Allow")
	if _, err := pool.Exec(
		`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, owner_uid)
		 VALUES('git','job','sched-me','default','* * * * *', NULL)`); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	s := New(pool, quietLog(), nil)
	if err := s.reloadJobs(ctx); err != nil {
		t.Fatalf("reloadJobs: %v", err)
	}
	if n := len(s.cr.Entries()); n == 0 {
		t.Error("a schedule entry with a NULL owner_uid registered no cron entry — " +
			"the join's name fallback arm is missing and this work silently stopped running")
	}
}
