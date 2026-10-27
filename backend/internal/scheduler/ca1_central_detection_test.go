package scheduler

import (
	"context"
	"testing"
)

// 2.5.3 CA1 (found by the phase's review): a definition of the built-in
// agency's never runs, and its skip is recorded once per episode
// (dedupeEpisodeReason), so the missed-run detector must not expect its fires.
// Watched, it read the second day's de-duplicated skip as a fire nobody
// explained and paged "did not run", which re-opened the episode for the third
// day: an alert every other day for a leftover nobody had moved yet. An
// agency's job and workflow on the same schedule are still watched.
func TestCA1ADefinitionOfCentralsIsNotWatchedForMissedFires(t *testing.T) {
	pool := mustPool(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seedOpsAgency(t, pool)
	// Central's job and workflow are born with no owner (an upgrade's
	// leftovers); Ops's are an agency's.
	exec(`INSERT INTO jobs (uid, name, source, run_type, enabled, synced_at) VALUES ('j-central', 'nightly', 'cronomicon', 'bash', 1, 't')`)
	exec(`INSERT INTO jobs (uid, name, source, run_type, enabled, synced_at, owner_agency) VALUES ('j-ops', 'nightly', 'cronomicon', 'bash', 1, 't', ?)`, opsAgency)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at) VALUES ('w-central', 'flow', 'cronomicon', '[]', 1, 't')`)
	exec(`INSERT INTO workflows (uid, name, source, steps, enabled, synced_at, owner_agency) VALUES ('w-ops', 'flow', 'cronomicon', '[]', 1, 't', ?)`, opsAgency)
	exec(`INSERT INTO definition_schedules (owner_source, owner_kind, owner_name, name, cron, position, owner_uid) VALUES
	        ('cronomicon', 'job',      'nightly', 'at-two', '0 2 * * *', 0, 'j-central'),
	        ('cronomicon', 'job',      'nightly', 'at-two', '0 2 * * *', 0, 'j-ops'),
	        ('cronomicon', 'workflow', 'flow',    'at-two', '0 2 * * *', 0, 'w-central'),
	        ('cronomicon', 'workflow', 'flow',    'at-two', '0 2 * * *', 0, 'w-ops')`)
	for _, uid := range []string{"j-central", "w-central"} {
		var owner string
		if err := pool.QueryRowContext(ctx, `SELECT owner_agency FROM jobs WHERE uid = ?1 UNION ALL SELECT owner_agency FROM workflows WHERE uid = ?1`, uid).Scan(&owner); err != nil || owner != "global" {
			t.Fatalf("%s is %q's (%v), want the built-in agency's", uid, owner, err)
		}
	}

	s := New(pool, quietLog(), nil)
	entries, err := s.scheduleEntriesForDetection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	watched := map[string]bool{}
	for _, e := range entries {
		watched[e.ownerUID] = true
	}
	for _, uid := range []string{"j-central", "w-central"} {
		if watched[uid] {
			t.Errorf("%s, the built-in agency's, is watched for missed fires", uid)
		}
	}
	for _, uid := range []string{"j-ops", "w-ops"} {
		if !watched[uid] {
			t.Errorf("%s, an agency's, is not watched for missed fires", uid)
		}
	}
}
