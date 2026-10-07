package seed

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/sshexec"
)

func TestSeedPopulatesEveryView(t *testing.T) {
	pool, err := db.Open(filepath.Join(t.TempDir(), "seed.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	if err := db.Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	if err := Seed(ctx, pool, log); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Every table a view reads from must be non-empty.
	for _, tbl := range []string{
		"scopes", "scope_hosts", "access_grants",
		"recent_logins", "jobs", "workflows", "runs", "workflow_runs",
		"activity", "change_log", "schedule_pushes", "git_sync_events",
		"runners", "env_vars", "secrets", "ssh_hosts", "bastions", "ssh_credentials",
		"alert_destinations", "alert_config", "settings",
	} {
		var n int
		if err := pool.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		if n == 0 {
			t.Errorf("table %s is empty after seed", tbl)
		}
	}

	// Singletons present.
	for _, tbl := range []string{"notification_config", "gitlab_config", "git_sync_state"} {
		var n int
		_ = pool.QueryRow("SELECT COUNT(*) FROM " + tbl + " WHERE id=1").Scan(&n)
		if n != 1 {
			t.Errorf("singleton %s missing", tbl)
		}
	}

	// Every row belongs to an agency, and to the right one (migration 1220): a
	// query that returns a row names something the seed left in a state the
	// product does not create.
	for what, q := range map[string]string{
		"a scope, runner, secret, variable or key in no agency": `
			SELECT id FROM scopes WHERE id NOT IN (SELECT scope_id FROM scope_agencies)
			UNION ALL SELECT id FROM runners WHERE id NOT IN (SELECT runner_id FROM runner_agencies)
			UNION ALL SELECT id FROM secrets WHERE id NOT IN (SELECT secret_id FROM secret_agencies)
			UNION ALL SELECT id FROM env_vars WHERE id NOT IN (SELECT env_var_id FROM env_var_agencies)
			UNION ALL SELECT id FROM ssh_credentials WHERE id NOT IN (SELECT credential_id FROM ssh_credential_agencies)`,
		"a secret, variable or key whose owner is not its one agency": `
			SELECT s.id FROM secrets s JOIN secret_agencies m ON m.secret_id = s.id WHERE m.agency_id <> s.owner_agency
			UNION ALL SELECT v.id FROM env_vars v JOIN env_var_agencies m ON m.env_var_id = v.id WHERE m.agency_id <> v.owner_agency
			UNION ALL SELECT c.id FROM ssh_credentials c JOIN ssh_credential_agencies m ON m.credential_id = c.id WHERE m.agency_id <> c.owner_agency`,
		"a run whose snapshot is not its scope's agencies (Global for none)": `
			SELECT r.id FROM runs r
			 WHERE r.agencies_json <> COALESCE((
			       SELECT json_group_array(a.name) FROM scopes s
			         JOIN scope_agencies sa ON sa.scope_id = s.id JOIN agencies a ON a.id = sa.agency_id
			        WHERE s.name = r.scope HAVING COUNT(*) > 0), '["Global"]')`,
		"a waiting run that is not indexed for the claim": `
			SELECT r.id FROM runs r, json_each(r.agencies_json) je
			 WHERE r.status IN ('queued', 'running')
			   AND NOT EXISTS (SELECT 1 FROM run_agencies ra WHERE ra.run_id = r.id AND ra.agency = je.value)`,
	} {
		var id string
		if err := pool.QueryRow(q + ` LIMIT 1`).Scan(&id); err == nil {
			t.Errorf("the seed left %s: %s", what, id)
		}
	}
	// The demo fleet can take the demo work: every agency that owns a scope has
	// an online runner, and Global has a runner of its own. (Until 2.3.0 every
	// seeded runner was in no agency and every seeded scope was in one.)
	var unserved int
	if err := pool.QueryRow(`
		SELECT COUNT(*) FROM (SELECT DISTINCT agency_id FROM scope_agencies) sa
		 WHERE NOT EXISTS (SELECT 1 FROM runner_agencies ra JOIN runners rn ON rn.id = ra.runner_id
		                    WHERE ra.agency_id = sa.agency_id AND rn.status = 'online')`).Scan(&unserved); err != nil || unserved != 0 {
		t.Errorf("%d agencies that own a scope have no online runner (%v)", unserved, err)
	}
	var globalRunners int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE agency_id = 'global'`).Scan(&globalRunners)
	if globalRunners == 0 {
		t.Error("no seeded runner serves Global, so no seeded job without a scope can ever run")
	}

	// A fresh demo starts with an empty inbox: every notice is a condition an
	// upgrade can leave behind or that someone broke, and the seed must be in
	// neither state — a scope in two agencies, a job aimed outside its scope, a
	// record naming a key its owner may not use, a row in no agency.
	notices.SetRecordKeyNameCheck(sshexec.BastionKeyNameFindings)
	t.Cleanup(func() { notices.SetRecordKeyNameCheck(nil) })
	if err := notices.RunChecks(ctx, pool); err != nil {
		t.Fatalf("notices checks on the seeded database: %v", err)
	}
	open, err := notices.ListOpen(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range open {
		t.Errorf("the seed raises a %s notice: %s", n.Kind, n.Detail)
	}

	// Re-seeding is a no-op (idempotent guard).
	if err := Seed(ctx, pool, log); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	var jobs int
	_ = pool.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobs)
	if jobs != 13 {
		t.Errorf("re-seed changed job count: got %d, want 13", jobs)
	}
}
