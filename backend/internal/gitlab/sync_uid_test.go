package gitlab

import (
	"context"
	"database/sql"
	"testing"
)

// TestDefinitionUidsSurviveSync — AF-4a's load-bearing guarantee: a definition's
// uid is assigned when the row is first seen and NEVER re-minted. Every sync
// sends a fresh db.NewID() as the INSERT value, so the only thing standing
// between a re-sync and a silent identity change is uid's omission from the
// ON CONFLICT DO UPDATE SET — exactly the preservation-by-omission mechanism
// tags use, guarded exactly the same way. A regression that "completes the
// pattern" by adding uid=excluded.uid is caught here for all three catalogs.
func TestDefinitionUidsSurviveSync(t *testing.T) {
	pool := mustOpenDB(t)
	ctx := context.Background()
	svc := &Service{cloneDir: t.TempDir()}

	inTx := func(fn func(tx *sql.Tx) error) {
		t.Helper()
		tx, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := fn(tx); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	uidOf := func(table, name string) string {
		t.Helper()
		var uid string
		if err := pool.QueryRow(`SELECT uid FROM `+table+` WHERE source='git' AND name=?`, name).Scan(&uid); err != nil {
			t.Fatalf("read %s uid: %v", table, err)
		}
		if uid == "" {
			t.Fatalf("%s row has an empty uid — the sync INSERT stopped assigning one", table)
		}
		return uid
	}

	// ── Jobs ──────────────────────────────────────────────────────────────────
	upsertJob := func(desc, now string) {
		j := JobYAML{}
		j.Metadata.Name = "uid-job"
		j.Spec.RunType = "bash"
		j.Spec.Scope = "Prod"
		j.Spec.Description = desc
		inTx(func(tx *sql.Tx) error { return svc.upsertJobs(ctx, tx, []JobYAML{j}, nil, nil, now, "sha") })
	}
	upsertJob("v1", "t1")
	first := uidOf("jobs", "uid-job")
	upsertJob("v2", "t2")
	if got := uidOf("jobs", "uid-job"); got != first {
		t.Errorf("job uid re-minted by a re-sync: %q → %q", first, got)
	}

	// ── Workflows ─────────────────────────────────────────────────────────────
	upsertWf := func(desc, now string) {
		wf := WorkflowYAML{}
		wf.Metadata.Name = "uid-wf"
		wf.Spec.Description = desc
		wf.Spec.Steps = []any{}
		inTx(func(tx *sql.Tx) error { return svc.upsertWorkflows(ctx, tx, []WorkflowYAML{wf}, nil, now, "sha") })
	}
	upsertWf("v1", "t1")
	first = uidOf("workflows", "uid-wf")
	upsertWf("v2", "t2")
	if got := uidOf("workflows", "uid-wf"); got != first {
		t.Errorf("workflow uid re-minted by a re-sync: %q → %q", first, got)
	}

	// ── Schedules ─────────────────────────────────────────────────────────────
	upsertSched := func(desc, now string) {
		rs := resolvedSchedule{cron: "0 0 2 * * *", description: desc, contentHash: "h-" + now}
		inTx(func(tx *sql.Tx) error {
			return svc.upsertSchedules(ctx, tx, map[string]resolvedSchedule{"uid-sched": rs}, now, "sha")
		})
	}
	upsertSched("v1", "t1")
	first = uidOf("schedules", "uid-sched")
	upsertSched("v2", "t2")
	if got := uidOf("schedules", "uid-sched"); got != first {
		t.Errorf("schedule uid re-minted by a re-sync: %q → %q", first, got)
	}
}
