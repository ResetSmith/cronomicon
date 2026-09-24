package db

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigrateUpDown verifies the B1 exit criterion: migrations apply cleanly
// up and down against a real SQLite file.
func TestMigrateUpDown(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	v, dirty, err := Status(pool)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if dirty {
		t.Fatal("schema is dirty after up")
	}
	if v != 1150 {
		t.Fatalf("schema version = %d, want 1150", v)
	}

	// Core tables should exist.
	for _, tbl := range []string{"runs", "workflow_runs", "activity", "change_log", "scopes", "secrets", "ssh_credentials", "runners", "scripts", "schedules", "reference_bindings",
		"calendars", "calendar_days",
		"reactions", "reaction_deliveries", "runner_placement_history",
		"roles", "access_grants", "scope_agencies", "secret_agencies", "env_var_agencies", "ssh_credential_agencies", "run_agencies",
		"service_accounts", "definition_revisions", "file_watch_sightings",
		"annotations", "runner_tags"} {
		var name string
		err := pool.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name)
		if err != nil {
			t.Errorf("expected table %q to exist: %v", tbl, err)
		}
	}

	// Readiness must pass on a clean, migrated DB.
	if err := ReadyCheck(pool)(context.Background()); err != nil {
		t.Fatalf("ready check: %v", err)
	}

	// Full down must drop everything without error.
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Down(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down: %v", err)
	}
}

// TestMigrate170DataRoundTrip exercises the v20 Phase-2 table-rebuild migration
// (170) with DATA PRESENT — TestMigrateUpDown only proves the rebuild applies on
// an EMPTY db, which can't catch data loss or a CHECK violation during the
// create→copy→rename (§13.1). It migrates up to 170, seeds git-source rows
// (incl. a paused workflow under the legacy __wf__ sentinel reachable via 030),
// then migrates one step down (170→160) and back up, asserting rows survive and
// the __wf__ sentinel round-trips through the explicit owner_kind column.
func TestMigrate170DataRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "rt.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(170); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 170: %v", err)
	}

	// Seed a git job, a git workflow, a schedule binding, and pause both
	// (the workflow via the new owner_kind='workflow' row).
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO jobs(name, source, run_type, synced_at) VALUES('backup','git','bash','t')`)
	seed(`INSERT INTO workflows(name, source, steps, synced_at) VALUES('pipeline','git','[]','t')`)
	seed(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron) VALUES('git','job','backup','default','* * * * *')`)
	seed(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at) VALUES('git','job','backup','tester','t')`)
	seed(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at) VALUES('git','workflow','pipeline','tester','t')`)

	// Down one step (170→160): the rebuild must preserve the data and recollapse
	// the paused workflow to the legacy '__wf__pipeline' key.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 170→160: %v", err)
	}
	var jobN, wfN, pausedN int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name='backup'`).Scan(&jobN)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE name='pipeline'`).Scan(&wfN)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM paused_jobs WHERE job_name='__wf__pipeline'`).Scan(&pausedN)
	if jobN != 1 || wfN != 1 {
		t.Fatalf("after down: job=%d wf=%d, want 1/1 (data lost in rebuild)", jobN, wfN)
	}
	if pausedN != 1 {
		t.Fatalf("after down: __wf__ sentinel not restored (paused workflow lost)")
	}

	// Back up (160→170): the sentinel must split back into owner_kind='workflow'.
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 160→170: %v", err)
	}
	var wfPaused int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM paused_jobs WHERE owner_kind='workflow' AND name='pipeline'`).Scan(&wfPaused)
	if wfPaused != 1 {
		t.Fatalf("after re-up: workflow pause not restored to owner_kind row")
	}
}

// TestMigrate190SourceRefBackfill proves the D1c backfill (schedule-builder.md)
// links definition_schedules entries whose name matches a first-class schedule and
// leaves entries with no matching catalog row NULL. This is the ratified Phase-1
// name-match approximation (§8 Risk #2, §10 Q1): for pre-190 rows the backfill cannot
// distinguish a true ref-expansion from an inline entry that coincidentally shares a
// catalog name — both get source_ref set. New post-v190 rows carry an EXACT source_ref
// (set by sync/compose at write time), so the approximation only affects the window
// before each owner's next sync/recompose.
func TestMigrate190SourceRefBackfill(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sref.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(180); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 180: %v", err)
	}

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A first-class schedule 'nightly', a job binding that ref-expands it, and an
	// inline-only entry whose name has no matching catalog row.
	seed(`INSERT INTO schedules(name, source, cron, content_hash) VALUES('nightly','git','0 0 2 * * *','sha256:x')`)
	seed(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron) VALUES('git','job','backup','nightly','0 0 2 * * *')`)
	seed(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron) VALUES('git','job','backup','adhoc','*/5 * * * *')`)

	// Up to 190: the backfill runs.
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 180→190: %v", err)
	}
	var refSrc, inlineSrc sql.NullString
	_ = pool.QueryRow(`SELECT source_ref FROM definition_schedules WHERE name='nightly'`).Scan(&refSrc)
	_ = pool.QueryRow(`SELECT source_ref FROM definition_schedules WHERE name='adhoc'`).Scan(&inlineSrc)
	if !refSrc.Valid || refSrc.String != "nightly" {
		t.Errorf("ref entry source_ref = %v, want 'nightly' (backfill should link it)", refSrc)
	}
	if inlineSrc.Valid {
		t.Errorf("inline entry source_ref = %q, want NULL (no matching catalog row)", inlineSrc.String)
	}

	// Down 190→180 drops the column without losing the entries.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 190→180: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='backup'`).Scan(&n)
	if n != 2 {
		t.Fatalf("after down: entries = %d, want 2 (data lost dropping source_ref)", n)
	}
}

// TestMigrate280ScriptTagsRoundTrip exercises migration 280 with DATA PRESENT:
// the down migration is `ALTER TABLE scripts DROP COLUMN tags`, which SQLite
// performs as a table rebuild — TestMigrateUpDown only runs the down on an EMPTY
// scripts table, so it can't catch data loss in that rebuild. This seeds a tagged
// script, steps down (column dropped, row must survive), and steps back up (tags
// returns with its DEFAULT '[]').
func TestMigrate280ScriptTagsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(280); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 280: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, content_hash, synced_at, tags)
	      VALUES('backup','bash','sha256:x','t','["prod","db"]')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Down 280→270: the DROP COLUMN rebuild must preserve the row.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 280→270: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scripts WHERE name='backup'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: scripts row lost in the DROP COLUMN rebuild")
	}
	var hasTags int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scripts') WHERE name='tags'`).Scan(&hasTags)
	if hasTags != 0 {
		t.Errorf("tags column still present after down")
	}

	// Back up 270→280: the column returns with DEFAULT '[]'.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 270→280: %v", err)
	}
	var tags string
	if err := pool.QueryRow(`SELECT tags FROM scripts WHERE name='backup'`).Scan(&tags); err != nil {
		t.Fatalf("read tags after re-up: %v", err)
	}
	if tags != "[]" {
		t.Errorf("after re-up tags = %q, want [] (DEFAULT)", tags)
	}
}

// TestMigrate290TagsRoundTrip exercises migration 290 with DATA PRESENT on both
// columns it adds (schedules.tags + workflows.tags). The down migration is two
// `ALTER TABLE … DROP COLUMN tags`, each a SQLite table rebuild — TestMigrateUpDown
// only runs the down on EMPTY tables, so it can't catch data loss. This seeds a
// tagged schedule + workflow, steps down (columns dropped, rows must survive), and
// steps back up (tags returns with its DEFAULT '[]'). Mirrors TestMigrate280…RoundTrip.
func TestMigrate290TagsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "tags290.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(290); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 290: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO schedules(name, source, cron, content_hash, synced_at, tags)
	      VALUES('nightly','git','0 0 * * *','sha256:s','t','["prod","cron"]')`); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO workflows(name, source, synced_at, tags)
	      VALUES('deploy','git','t','["release"]')`); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	// Down 290→280: the two DROP COLUMN rebuilds must preserve the rows.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 290→280: %v", err)
	}
	var ns, nw int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM schedules WHERE name='nightly'`).Scan(&ns)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflows WHERE name='deploy'`).Scan(&nw)
	if ns != 1 {
		t.Fatalf("after down: schedules row lost in the DROP COLUMN rebuild")
	}
	if nw != 1 {
		t.Fatalf("after down: workflows row lost in the DROP COLUMN rebuild")
	}
	var schedTags, wfTags int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('schedules') WHERE name='tags'`).Scan(&schedTags)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('workflows') WHERE name='tags'`).Scan(&wfTags)
	if schedTags != 0 || wfTags != 0 {
		t.Errorf("tags column still present after down (schedules=%d workflows=%d)", schedTags, wfTags)
	}

	// Back up 280→290: both columns return with DEFAULT '[]'.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 280→290: %v", err)
	}
	var st, wt string
	if err := pool.QueryRow(`SELECT tags FROM schedules WHERE name='nightly'`).Scan(&st); err != nil {
		t.Fatalf("read schedule tags after re-up: %v", err)
	}
	if err := pool.QueryRow(`SELECT tags FROM workflows WHERE name='deploy'`).Scan(&wt); err != nil {
		t.Fatalf("read workflow tags after re-up: %v", err)
	}
	if st != "[]" || wt != "[]" {
		t.Errorf("after re-up tags = (schedules %q, workflows %q), want [] (DEFAULT)", st, wt)
	}
}

// TestMigrate470ConfigTagsRoundTrip exercises migration 470 with DATA PRESENT on
// all three columns it adds (env_vars.tags + secrets.tags + ssh_credentials.tags).
// The down migration is three `ALTER TABLE … DROP COLUMN tags`, each a SQLite
// rebuild — TestMigrateUpDown only runs the down on EMPTY tables, so it can't
// catch data loss. This seeds a tagged row in each table, steps down (columns
// dropped, rows must survive), and steps back up (tags returns with DEFAULT '[]').
// Mirrors TestMigrate280…RoundTrip / TestMigrate290TagsRoundTrip.
func TestMigrate470ConfigTagsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "tags470.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(470); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 470: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO env_vars(id, key, value, created_at, tags)
	      VALUES('ev1','API_URL','https://x','t','["prod","http"]')`); err != nil {
		t.Fatalf("seed env_var: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO secrets(id, key, source, created_at, tags)
	      VALUES('s1','API_KEY','stored','t','["prod"]')`); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO ssh_credentials(id, label, source, tags)
	      VALUES('c1','deploy-key','stored','["prod","deploy"]')`); err != nil {
		t.Fatalf("seed ssh credential: %v", err)
	}

	// Down 470→460: the three DROP COLUMN rebuilds must preserve the rows.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 470→460: %v", err)
	}
	for _, tc := range []struct{ table, id string }{
		{"env_vars", "ev1"}, {"secrets", "s1"}, {"ssh_credentials", "c1"},
	} {
		var n int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM `+tc.table+` WHERE id=?`, tc.id).Scan(&n)
		if n != 1 {
			t.Fatalf("after down: %s row lost in the DROP COLUMN rebuild", tc.table)
		}
		var hasTags int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('` + tc.table + `') WHERE name='tags'`).Scan(&hasTags)
		if hasTags != 0 {
			t.Errorf("%s.tags column still present after down", tc.table)
		}
	}

	// Back up 460→470: the columns return with DEFAULT '[]'.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 460→470: %v", err)
	}
	for _, tc := range []struct{ table, id string }{
		{"env_vars", "ev1"}, {"secrets", "s1"}, {"ssh_credentials", "c1"},
	} {
		var tags string
		if err := pool.QueryRow(`SELECT tags FROM `+tc.table+` WHERE id=?`, tc.id).Scan(&tags); err != nil {
			t.Fatalf("read %s.tags after re-up: %v", tc.table, err)
		}
		if tags != "[]" {
			t.Errorf("after re-up %s.tags = %q, want [] (DEFAULT)", tc.table, tags)
		}
	}
}

// TestMigrate480WorkflowLayoutRoundTrip exercises migration 480 with DATA PRESENT
// on the advisory workflows.layout_json column. The down migration is
// `ALTER TABLE workflows DROP COLUMN layout_json`, a SQLite rebuild that
// TestMigrateUpDown only runs on an EMPTY workflows table, so it can't catch data
// loss. This seeds a laid-out workflow, steps down (column dropped — the row and,
// critically, its `steps` definition must survive the rebuild), and steps back up
// (layout_json returns as NULL). Mirrors TestMigrate470ConfigTagsRoundTrip.
func TestMigrate480WorkflowLayoutRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "layout480.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(480); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 480: %v", err)
	}
	const steps = `[{"type":"job","name":"build"}]`
	if _, err := pool.Exec(`INSERT INTO workflows(name, source, steps, layout_json)
	      VALUES('deploy','amadeus',?,'{"build":{"x":40,"y":80}}')`, steps); err != nil {
		t.Fatalf("seed workflow: %v", err)
	}

	// Down 480→470: the DROP COLUMN rebuild must preserve the row and its steps.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 480→470: %v", err)
	}
	var gotSteps string
	if err := pool.QueryRow(`SELECT steps FROM workflows WHERE source='amadeus' AND name='deploy'`).Scan(&gotSteps); err != nil {
		t.Fatalf("after down: workflow row lost in the DROP COLUMN rebuild: %v", err)
	}
	if gotSteps != steps {
		t.Errorf("after down: steps = %q, want %q (definition must survive the rebuild)", gotSteps, steps)
	}
	var hasLayout int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('workflows') WHERE name='layout_json'`).Scan(&hasLayout)
	if hasLayout != 0 {
		t.Errorf("workflows.layout_json column still present after down")
	}

	// Back up 470→480: the column returns; advisory layout is not restored (NULL).
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 470→480: %v", err)
	}
	var layout sql.NullString
	if err := pool.QueryRow(`SELECT layout_json FROM workflows WHERE source='amadeus' AND name='deploy'`).Scan(&layout); err != nil {
		t.Fatalf("read layout_json after re-up: %v", err)
	}
	if layout.Valid {
		t.Errorf("after re-up layout_json = %q, want NULL (advisory column is not restored)", layout.String)
	}
}

// TestMigrate490PythonRuntypeRoundTrip exercises migration 490 with DATA PRESENT
// on jobs/runs/scripts. The migration widens the run_type CHECK to include
// 'python' by REBUILDING all three tables (create _new → copy → drop → rename);
// TestMigrateUpDown only runs it on empty tables, so it can't catch data loss. This
// seeds a pre-existing 'bash' row in each table, steps UP to 490 (the rebuild must
// preserve every row), confirms 'python' is now insertable, then steps DOWN to 480
// (the narrowing rebuild must again preserve the bash rows and re-reject 'python').
func TestMigrate490PythonRuntypeRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "python490.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(480); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 480: %v", err)
	}
	// Pre-existing bash rows (data present across the rebuild).
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type) VALUES('j-bash','amadeus','bash')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-bash','j-bash','bash','success','tester','manual','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, content_hash, synced_at)
	      VALUES('s-bash','bash','sha256:abc','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
	// Before 490 the CHECK must reject python.
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, content_hash, synced_at)
	      VALUES('s-py','python','sha256:def','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("pre-490: python script insert should have failed the run_type CHECK")
	}

	// Up 480→490: the rebuild must preserve the bash rows AND now accept python.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 480→490: %v", err)
	}
	for _, q := range []string{
		`SELECT run_type FROM jobs WHERE source='amadeus' AND name='j-bash'`,
		`SELECT run_type FROM runs WHERE id='run-bash'`,
		`SELECT run_type FROM scripts WHERE name='s-bash'`,
	} {
		var rt string
		if err := pool.QueryRow(q).Scan(&rt); err != nil {
			t.Fatalf("after up: bash row lost in the rebuild (%s): %v", q, err)
		}
		if rt != "bash" {
			t.Errorf("after up: run_type = %q, want bash", rt)
		}
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type) VALUES('j-py','amadeus','python')`); err != nil {
		t.Fatalf("post-490: python job insert should succeed: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-py','j-py','python','queued','tester','manual','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("post-490: python run insert should succeed: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, content_hash, synced_at)
	      VALUES('s-py','python','sha256:def','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("post-490: python script insert should succeed: %v", err)
	}

	// Remove the python rows so the narrowing down-migration's CHECK is satisfiable.
	pool.Exec(`DELETE FROM runs WHERE run_type='python'`)
	pool.Exec(`DELETE FROM jobs WHERE run_type='python'`)
	pool.Exec(`DELETE FROM scripts WHERE run_type='python'`)

	// Down 490→480: the narrowing rebuild must preserve the bash rows and re-reject python.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 490→480: %v", err)
	}
	var gotRT string
	if err := pool.QueryRow(`SELECT run_type FROM scripts WHERE name='s-bash'`).Scan(&gotRT); err != nil {
		t.Fatalf("after down: bash script lost in the narrowing rebuild: %v", err)
	}
	if gotRT != "bash" {
		t.Errorf("after down: script run_type = %q, want bash", gotRT)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, content_hash, synced_at)
	      VALUES('s-py2','python','sha256:ghi','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("after down: python script insert should be rejected again")
	}
}

// TestMigrate320DropSensitiveLoggingRoundTrip exercises migration 320 with DATA
// PRESENT: the up migration is `ALTER TABLE jobs DROP COLUMN sensitive_logging`,
// which SQLite performs as a table rebuild — TestMigrateUpDown only runs it on an
// EMPTY jobs table, so it can't catch data loss in that rebuild. This migrates to
// 320 with the column already gone, steps down (column returns, rows must survive),
// seeds a row, and steps back up (column dropped, row preserved).
func TestMigrate320DropSensitiveLoggingRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sl320.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(320); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 320: %v", err)
	}
	// Column must be gone at 320.
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='sensitive_logging'`).Scan(&has)
	if has != 0 {
		t.Fatalf("sensitive_logging still present at 320")
	}

	// Down 320→310: the column returns (DEFAULT 0). Seed a row, then re-up.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 320→310: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='sensitive_logging'`).Scan(&has)
	if has != 1 {
		t.Fatalf("sensitive_logging not restored on down")
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, synced_at, sensitive_logging)
	      VALUES('backup','git','bash','t',1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Back up 310→320: the DROP COLUMN rebuild must preserve the row.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 310→320: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name='backup'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after re-up: jobs row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='sensitive_logging'`).Scan(&has)
	if has != 0 {
		t.Errorf("sensitive_logging still present after re-up")
	}
}

// TestMigrate350RoundTrip exercises the 350 scopes inventory column adds with
// DATA PRESENT — the down DROPs four columns (a SQLite table rebuild) that must
// preserve other rows (TestMigrateUpDown only proves the rebuild on an empty db).
func TestMigrate350RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "inv350.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(350); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 350: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='raw_inventory'`).Scan(&has)
	if has != 1 {
		t.Fatalf("raw_inventory column missing at 350")
	}
	if _, err := pool.Exec(`INSERT INTO scopes(id, name, source, created_at, raw_inventory, inventory_format)
	      VALUES('s1','prod','git','t','[web]',?)`, "ini"); err != nil {
		t.Fatalf("seed scope: %v", err)
	}

	// Down 350→340: the four columns DROP (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 350→340: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name='prod'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: scopes row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='raw_inventory'`).Scan(&has)
	if has != 0 {
		t.Fatalf("raw_inventory still present at 340 after down")
	}

	// Back up 340→350: the columns return.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 340→350: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='raw_inventory'`).Scan(&has)
	if has != 1 {
		t.Errorf("raw_inventory not restored after re-up")
	}
}

// TestMigrate390RoundTrip exercises the 390 runners.protocol_version add with
// DATA PRESENT — the down DROPs the column (table rebuild) and must preserve rows.
func TestMigrate390RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "rpv390.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(390); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 390: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='protocol_version'`).Scan(&has)
	if has != 1 {
		t.Fatalf("protocol_version column missing at 390")
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at, protocol_version)
	      VALUES('r1','runner-a','online','t','t',2)`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Down 390→350: protocol_version DROPs (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 390→350: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE name='runner-a'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runners row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='protocol_version'`).Scan(&has)
	if has != 0 {
		t.Fatalf("protocol_version still present at 350 after down")
	}

	// Back up 350→390: the column returns (DEFAULT 1).
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 350→390: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='protocol_version'`).Scan(&has)
	if has != 1 {
		t.Errorf("protocol_version not restored after re-up")
	}
}

// TestMigrate400RoundTrip exercises the 400 ssh_hosts provenance adds with DATA
// PRESENT — the down DROPs three columns (a table rebuild) that must preserve rows.
func TestMigrate400RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "ssh400.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(400); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 400: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='source'`).Scan(&has)
	if has != 1 {
		t.Fatalf("source column missing at 400")
	}
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, port, created_at, source)
	      VALUES('h1','web1.example.com',22,'t','git')`); err != nil {
		t.Fatalf("seed ssh host: %v", err)
	}

	// Down 400→390: the three columns DROP (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 400→390: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE hostname='web1.example.com'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: ssh_hosts row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='source'`).Scan(&has)
	if has != 0 {
		t.Fatalf("source still present at 390 after down")
	}

	// Back up 390→400: the columns return (DEFAULT 'amadeus').
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 390→400: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='source'`).Scan(&has)
	if has != 1 {
		t.Errorf("source not restored after re-up")
	}
}

// TestMigrate430RoundTrip exercises the 430 scopes.agency_id add with DATA
// PRESENT — the down DROPs the column (a SQLite table rebuild) and must preserve
// other rows (TestMigrateUpDown only proves the rebuild on an empty scopes table).
// It also confirms the 420 agencies catalog table is present.
func TestMigrate430RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "agency430.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(430); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 430: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='agency_id'`).Scan(&has)
	if has != 1 {
		t.Fatalf("agency_id column missing at 430")
	}
	// 420 agencies catalog + a scope bound to it (430).
	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('a1','alpha','t')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scopes(id, name, source, created_at, agency_id)
	      VALUES('s1','prod','amadeus','t','a1')`); err != nil {
		t.Fatalf("seed scope: %v", err)
	}

	// Down 430→420: agency_id DROPs (table rebuild). The scope row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 430→420: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scopes WHERE name='prod'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: scopes row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='agency_id'`).Scan(&has)
	if has != 0 {
		t.Fatalf("agency_id still present at 420 after down")
	}

	// Back up 420→430: the column returns.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 420→430: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scopes') WHERE name='agency_id'`).Scan(&has)
	if has != 1 {
		t.Errorf("agency_id not restored after re-up")
	}
}

// TestMigrate440RunnerAgencies verifies the 440 join table applies, accepts a
// (runner, agency) membership row, and drops cleanly. It is a pure CREATE/DROP
// TABLE (no rebuild of an existing table), so no data-present round-trip is needed.
func TestMigrate440RunnerAgencies(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "ra440.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(440); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 440: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at)
	      VALUES('r1','runner-a','online','t','t')`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('a1','alpha','t')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO runner_agencies(runner_id, agency_id) VALUES('r1','a1')`); err != nil {
		t.Fatalf("insert membership: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runner_agencies`).Scan(&n)
	if n != 1 {
		t.Fatalf("runner_agencies count = %d, want 1", n)
	}

	// Down 440→430: the table drops.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 440→430: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='runner_agencies'`).Scan(&has)
	if has != 0 {
		t.Fatalf("runner_agencies still present at 430 after down")
	}
}

// TestMigrate450RoundTrip exercises the 450 runs.agency add with DATA PRESENT —
// the down DROPs the column (a SQLite table rebuild) and must preserve run rows
// (TestMigrateUpDown only proves the rebuild on an empty runs table).
func TestMigrate450RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "runagency450.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(450); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 450: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='agency'`).Scan(&has)
	if has != 1 {
		t.Fatalf("agency column missing at 450")
	}
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agency)
	      VALUES('t1','j','bash','queued','tester','manual','t','alpha')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	// Down 450→440: agency DROPs (table rebuild). The run row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 450→440: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE id='t1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runs row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='agency'`).Scan(&has)
	if has != 0 {
		t.Fatalf("agency still present at 440 after down")
	}

	// Back up 440→450: the column returns.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 440→450: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='agency'`).Scan(&has)
	if has != 1 {
		t.Errorf("agency not restored after re-up")
	}
}

// TestMigrate460RoundTrip exercises the 460 ssh_credentials add with DATA PRESENT —
// the down DROPs the auth_credential_id columns (a table rebuild) and the
// ssh_credentials table, and must preserve referencing host rows.
func TestMigrate460RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sshcred460.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(460); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 460: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='auth_credential_id'`).Scan(&has)
	if has != 1 {
		t.Fatalf("auth_credential_id column missing at 460")
	}
	var hasTbl int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ssh_credentials'`).Scan(&hasTbl)
	if hasTbl != 1 {
		t.Fatalf("ssh_credentials table missing at 460")
	}
	// Seed a credential and a host that references it via the new FK.
	if _, err := pool.Exec(`INSERT INTO ssh_credentials(id, label, source, created_at)
	      VALUES('c1','prod-deploy-ed25519','stored','t')`); err != nil {
		t.Fatalf("seed credential: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO ssh_hosts(id, hostname, port, created_at, source, auth_credential_id)
	      VALUES('h1','web1.example.com',22,'t','amadeus','c1')`); err != nil {
		t.Fatalf("seed ssh host: %v", err)
	}

	// Down 460→450: the FK columns DROP (table rebuild) and ssh_credentials is
	// dropped. The host row must survive the rebuild.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 460→450: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM ssh_hosts WHERE id='h1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: ssh_hosts row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='auth_credential_id'`).Scan(&has)
	if has != 0 {
		t.Fatalf("auth_credential_id still present at 450 after down")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ssh_credentials'`).Scan(&hasTbl)
	if hasTbl != 0 {
		t.Fatalf("ssh_credentials still present at 450 after down")
	}

	// Back up 450→460: the columns and table return.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 450→460: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('ssh_hosts') WHERE name='auth_credential_id'`).Scan(&has)
	if has != 1 {
		t.Errorf("auth_credential_id not restored after re-up")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ssh_credentials'`).Scan(&hasTbl)
	if hasTbl != 1 {
		t.Errorf("ssh_credentials not restored after re-up")
	}
}

// TestMigrate530RoundTrip exercises the 530 runners.resync_requested add with
// DATA PRESENT — the down DROPs the column (table rebuild) and must preserve rows.
func TestMigrate530RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "resync530.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(530); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 530: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='resync_requested'`).Scan(&has)
	if has != 1 {
		t.Fatalf("resync_requested column missing at 530")
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at, resync_requested)
	      VALUES('r1','runner-a','online','t','t',1)`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Down 530→520: resync_requested DROPs (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 530→520: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE name='runner-a'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runners row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='resync_requested'`).Scan(&has)
	if has != 0 {
		t.Fatalf("resync_requested still present at 520 after down")
	}

	// Back up 520→530: the column returns (DEFAULT 0).
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 520→530: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='resync_requested'`).Scan(&has)
	if has != 1 {
		t.Errorf("resync_requested not restored after re-up")
	}
}

// TestMigrate540RoundTrip exercises the 540 registration_tokens single-use
// columns with DATA PRESENT — the down DROPs three columns (table rebuild)
// that must preserve rows.
func TestMigrate540RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "regtok540.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(540); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 540: %v", err)
	}
	var has int
	for _, col := range []string{"label", "used_at", "used_by_runner_id"} {
		_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('registration_tokens') WHERE name=?`, col).Scan(&has)
		if has != 1 {
			t.Fatalf("%s column missing at 540", col)
		}
	}
	if _, err := pool.Exec(`INSERT INTO registration_tokens(token_hash, created_by, created_at, expires_at, label, used_at, used_by_runner_id)
	      VALUES('h1','op','t','t2','web-01','t3','r1')`); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// Down 540→530: the three columns DROP (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 540→530: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM registration_tokens WHERE token_hash='h1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: registration_tokens row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('registration_tokens') WHERE name='label'`).Scan(&has)
	if has != 0 {
		t.Fatalf("label still present at 530 after down")
	}

	// Back up 530→540: the columns return (NULL).
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 530→540: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('registration_tokens') WHERE name='used_by_runner_id'`).Scan(&has)
	if has != 1 {
		t.Errorf("used_by_runner_id not restored after re-up")
	}
}

// TestMigrate550RoundTrip exercises the 550 runners.config_digest add with
// DATA PRESENT — the down DROPs the column (table rebuild) and must preserve rows.
func TestMigrate550RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "digest550.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(550); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 550: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='config_digest'`).Scan(&has)
	if has != 1 {
		t.Fatalf("config_digest column missing at 550")
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at, config_digest)
	      VALUES('r1','runner-a','online','t','t','abc123')`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Down 550→540: config_digest DROPs (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 550→540: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE name='runner-a'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runners row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='config_digest'`).Scan(&has)
	if has != 0 {
		t.Fatalf("config_digest still present at 540 after down")
	}

	// Back up 540→550: the column returns (NULL).
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 540→550: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='config_digest'`).Scan(&has)
	if has != 1 {
		t.Errorf("config_digest not restored after re-up")
	}
}

// TestMigrate560RoundTrip exercises the 560 server-managed-settings columns with
// DATA PRESENT — the down DROPs all three columns and must preserve rows.
func TestMigrate560RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "settings560.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(560); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 560: %v", err)
	}
	for _, col := range []string{"managed_settings", "settings_version", "settings_acked_version"} {
		var has int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name=?`, col).Scan(&has)
		if has != 1 {
			t.Fatalf("%s column missing at 560", col)
		}
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at, managed_settings, settings_version, settings_acked_version)
	      VALUES('r1','runner-a','online','t','t','{"maxConcurrent":8}',3,2)`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}

	// Down 560→550: the three columns DROP (table rebuild). The row must survive.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 560→550: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE name='runner-a'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runners row lost in the DROP COLUMN rebuild")
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='managed_settings'`).Scan(&has)
	if has != 0 {
		t.Fatalf("managed_settings still present at 550 after down")
	}

	// Back up 550→560: the columns return.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 550→560: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runners') WHERE name='settings_version'`).Scan(&has)
	if has != 1 {
		t.Errorf("settings_version not restored after re-up")
	}
}

// TestMigrate570RoundTrip exercises the 570 host-key approval schema (a new
// table + a runners column) with DATA PRESENT — down drops both and must
// preserve rows.
func TestMigrate570RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "hostkeys570.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(570); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 570: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO runners(id, name, status, registered_at, created_at, keyscan_requested)
	      VALUES('r1','runner-a','online','t','t','["web01"]')`); err != nil {
		t.Fatalf("seed runner: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO pending_host_keys(id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at)
	      VALUES('k1','r1','web01','ssh-ed25519','SHA256:abc','web01 ssh-ed25519 AAAA','t')`); err != nil {
		t.Fatalf("seed pending host key: %v", err)
	}

	// Down 570→560: pending_host_keys + keyscan_requested drop; the runners row survives.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 570→560: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runners WHERE name='runner-a'`).Scan(&n)
	if n != 1 {
		t.Fatalf("after down: runners row lost in the rebuild")
	}
	var tbl int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='pending_host_keys'`).Scan(&tbl)
	if tbl != 0 {
		t.Fatalf("pending_host_keys still present at 560 after down")
	}

	// Back up 560→570: the table + column return.
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 560→570: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='pending_host_keys'`).Scan(&tbl)
	if tbl != 1 {
		t.Errorf("pending_host_keys not restored after re-up")
	}
}

// TestScopeStarBackfill630 verifies the A5 preserve-then-tighten backfill
// (migration 630): a role with an explicit allowed=1 grant is left untouched, while
// a role that resolved to empty under the old model (no rows, or only allowed=0 rows)
// gets the "*" (AllScopes) grant so it stays unrestricted post-fix.
func TestScopeStarBackfill630(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "backfill.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	// Migrate to just BEFORE the backfill, then seed a representative matrix.
	if err := m.Migrate(620); err != nil {
		t.Fatalf("migrate to 620: %v", err)
	}
	// viewer: explicitly restricted to prod (allowed=1) → must be untouched.
	// operator: only an allowed=0 row (resolves empty ⇒ unrestricted-of-old) → gets "*".
	// admin, approver: no rows at all → get "*".
	if _, err := pool.Exec(`INSERT INTO scope_restrictions(role,scope,allowed) VALUES('viewer','prod',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(`INSERT INTO scope_restrictions(role,scope,allowed) VALUES('operator','staging',0)`); err != nil {
		t.Fatal(err)
	}

	if err := m.Migrate(630); err != nil {
		t.Fatalf("migrate to 630: %v", err)
	}

	star := func(role string) bool {
		var n int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_restrictions WHERE role=? AND scope='*' AND allowed=1`, role).Scan(&n)
		return n == 1
	}
	for _, role := range []string{"admin", "approver", "operator"} {
		if !star(role) {
			t.Errorf("role %q did not receive the '*' backfill (was empty/allowed=0 pre-migrate)", role)
		}
	}
	if star("viewer") {
		t.Error("viewer had an explicit grant and must NOT receive '*'")
	}
	// viewer's original grant is intact.
	var prod int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_restrictions WHERE role='viewer' AND scope='prod' AND allowed=1`).Scan(&prod)
	if prod != 1 {
		t.Error("viewer's explicit 'prod' grant was disturbed by the backfill")
	}
}

func TestNewTraceIDIsSortable(t *testing.T) {
	a := NewTraceID()
	b := NewTraceID()
	if a == b {
		t.Fatal("trace ids collided")
	}
	if len(a) != 36 {
		t.Fatalf("trace id %q is not a UUID string", a)
	}
	// UUIDv7 is time-ordered: a minted before b should sort <= b.
	if a > b {
		t.Fatalf("UUIDv7 not monotonic: %q > %q", a, b)
	}
}

// TestMigrate640RoundTrip (SU-4): bastions.host_key adds, a seeded bastion row
// survives the DROP COLUMN table rebuild on down, and the column returns on re-up.
func TestMigrate640RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "bastion640.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(640); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 640: %v", err)
	}
	var has int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bastions') WHERE name='host_key'`).Scan(&has)
	if has != 1 {
		t.Fatalf("bastions.host_key missing at 640")
	}
	if _, err := pool.Exec(`INSERT INTO bastions(id, hostname, port, created_at, host_key)
	      VALUES('bk1','jump.example.com',22,'t','ssh-ed25519 AAAAKEY')`); err != nil {
		t.Fatalf("seed bastion: %v", err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 640→630: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM bastions WHERE id='bk1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("bastion row lost in the DROP COLUMN rebuild")
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bastions') WHERE name='host_key'`).Scan(&has)
	if has != 0 {
		t.Fatalf("host_key still present at 630 after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 630→640: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bastions') WHERE name='host_key'`).Scan(&has)
	if has != 1 {
		t.Errorf("host_key not restored after re-up")
	}
}

// TestMigrate641RoundTrip (SU-5): the auth_session_epoch singleton adds seeded at
// epoch 0, is bumpable, and cleanly drops/returns on down/up.
func TestMigrate641RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "epoch641.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(641); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 641: %v", err)
	}
	var epoch int
	if err := pool.QueryRow(`SELECT epoch FROM auth_session_epoch WHERE id=1`).Scan(&epoch); err != nil {
		t.Fatalf("seed row missing: %v", err)
	}
	if epoch != 0 {
		t.Errorf("initial epoch = %d, want 0", epoch)
	}
	if _, err := pool.Exec(`UPDATE auth_session_epoch SET epoch = epoch + 1 WHERE id=1`); err != nil {
		t.Fatalf("bump: %v", err)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 641→640: %v", err)
	}
	var hasTbl int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='auth_session_epoch'`).Scan(&hasTbl)
	if hasTbl != 0 {
		t.Fatalf("auth_session_epoch still present after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 640→641: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='auth_session_epoch'`).Scan(&hasTbl)
	if hasTbl != 1 {
		t.Errorf("auth_session_epoch not restored after re-up")
	}
}

// TestMigrate651RoundTrip (JR-Q6): scripts.prompts_json arrives defaulting to '[]' for
// existing rows, round-trips a declared list, and cleanly drops/returns on down/up.
//
// Note the version jump 641 → 651: 650 is deliberately skipped because the reverted
// PK-1 work stamped real databases at 650, and reusing it would make migrate treat this
// column as already applied on those databases (silently leaving it missing).
func TestMigrate651RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "scriptprompts651.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	// Migrate to the PREVIOUS version and seed a row, so the new column's DEFAULT is
	// exercised on pre-existing data (the real upgrade path).
	if err := m.Migrate(641); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 641: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at)
	     VALUES('legacy','bash','echo hi','sha256:aaa','t')`); err != nil {
		t.Fatalf("seed pre-migration script: %v", err)
	}
	if err := m.Migrate(651); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 651: %v", err)
	}

	// The pre-existing row reads as "declares nothing" rather than NULL.
	var legacy string
	if err := pool.QueryRow(`SELECT prompts_json FROM scripts WHERE name='legacy'`).Scan(&legacy); err != nil {
		t.Fatalf("read backfilled column: %v", err)
	}
	if legacy != "[]" {
		t.Errorf("pre-migration row prompts_json = %q, want %q", legacy, "[]")
	}

	// A declared list round-trips verbatim.
	const declared = `[{"name":"TARGET_ENV","required":true,"options":["dev","prod"]}]`
	if _, err := pool.Exec(`UPDATE scripts SET prompts_json=? WHERE name='legacy'`, declared); err != nil {
		t.Fatalf("write prompts_json: %v", err)
	}
	var got string
	_ = pool.QueryRow(`SELECT prompts_json FROM scripts WHERE name='legacy'`).Scan(&got)
	if got != declared {
		t.Errorf("prompts_json = %q, want %q", got, declared)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 651→641: %v", err)
	}
	var hasCol int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scripts') WHERE name='prompts_json'`).Scan(&hasCol)
	if hasCol != 0 {
		t.Fatalf("scripts.prompts_json still present after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 641→651: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('scripts') WHERE name='prompts_json'`).Scan(&hasCol)
	if hasCol != 1 {
		t.Errorf("scripts.prompts_json not restored after re-up")
	}
}

// TestMigrate660RoundTrip (JR-Q5): jobs.prompt_enforcement defaults every existing job
// to 'warn' (so no deployed automation changes behavior), accepts 'block', REJECTS any
// other value via the CHECK constraint, and cleanly drops/returns on down/up.
func TestMigrate660RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "enforce660.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	// Seed a job BEFORE the migration so the DEFAULT is exercised on existing data —
	// this is the property that makes the feature safe to deploy.
	if err := m.Migrate(651); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 651: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, command, scope)
	     VALUES('legacy','git','bash','echo hi','Prod')`); err != nil {
		t.Fatalf("seed pre-migration job: %v", err)
	}
	if err := m.Migrate(660); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 660: %v", err)
	}

	var mode string
	if err := pool.QueryRow(`SELECT prompt_enforcement FROM jobs WHERE name='legacy'`).Scan(&mode); err != nil {
		t.Fatalf("read backfilled column: %v", err)
	}
	if mode != "warn" {
		t.Errorf("pre-migration job prompt_enforcement = %q, want %q — existing jobs must keep UDV4 semantics", mode, "warn")
	}

	// 'block' is accepted.
	if _, err := pool.Exec(`UPDATE jobs SET prompt_enforcement='block' WHERE name='legacy'`); err != nil {
		t.Fatalf("set block: %v", err)
	}
	// Anything else is rejected by the CHECK — the column is a closed vocabulary, so a
	// typo can't silently disable enforcement on a job an admin meant to protect.
	if _, err := pool.Exec(`UPDATE jobs SET prompt_enforcement='blocc' WHERE name='legacy'`); err == nil {
		t.Error("CHECK constraint did not reject an out-of-vocabulary enforcement value")
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 660→651: %v", err)
	}
	var hasCol int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='prompt_enforcement'`).Scan(&hasCol)
	if hasCol != 0 {
		t.Fatalf("jobs.prompt_enforcement still present after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 651→660: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='prompt_enforcement'`).Scan(&hasCol)
	if hasCol != 1 {
		t.Errorf("jobs.prompt_enforcement not restored after re-up")
	}
}

// TestMigrate670RoundTrip (agencies plan T2.1/T2.2) pins the backfill's governing
// property: it must be BEHAVIOR-PRESERVING. The cases that matter are the ones an
// "obvious" backfill gets wrong — an UNSCOPED secret must get NO membership (an
// empty set is what keeps it visible everywhere under AG-Q1(b)), and SSH keys must
// get none at all (AG-Q5's tightening is Phase 3, behind the T2.12 report).
func TestMigrate670RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "membership670.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	// Seed BEFORE 670 so the backfill runs over existing data — the only thing that
	// makes this migration safe to deploy.
	if err := m.Migrate(660); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 660: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const now = "2026-01-01T00:00:00Z"
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-dss','DSS',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-nwd','NWD',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at, agency_id) VALUES('sc-prod','prod','amadeus',?,'ag-dss')`, now)
	// A scope with NO agency — the general pool. Must produce no membership row.
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc-dev','dev','amadeus',?)`, now)
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-prod','DB_PASS','prod','stored',?)`, now)
	exec(`INSERT INTO secrets(id, key, source, created_at) VALUES('s-global','TOKEN','stored',?)`, now)
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-dev','DEV_PASS','dev','stored',?)`, now)
	// A secret naming a scope that does not exist — the JOIN must simply drop it
	// rather than fail the migration.
	exec(`INSERT INTO secrets(id, key, scope, source, created_at) VALUES('s-orphan','ORPHAN','ghost','stored',?)`, now)
	exec(`INSERT INTO env_vars(id, key, value, scope, created_at) VALUES('v-prod','REGION','us-east','prod',?)`, now)
	exec(`INSERT INTO env_vars(id, key, value, created_at) VALUES('v-global','LOG_LEVEL','info',?)`, now)
	exec(`INSERT INTO ssh_credentials(id, label, source, created_at, last_modified_at)
	      VALUES('k1','deploy_key','stored',?,?)`, now, now)

	if err := m.Migrate(670); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 670: %v", err)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v\n%s", err, q)
		}
		return n
	}
	// Scopes: the 1:1 binding becomes exactly one row; an unbound scope gets none.
	if n := count(`SELECT COUNT(*) FROM scope_agencies`); n != 1 {
		t.Errorf("scope_agencies rows = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc-prod' AND agency_id='ag-dss'`); n != 1 {
		t.Errorf("prod scope did not inherit its agency_id")
	}
	// A SCOPED secret inherits its scope's agency…
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-prod' AND agency_id='ag-dss'`); n != 1 {
		t.Errorf("scoped secret did not inherit its scope's agency")
	}
	// …and everything else gets nothing. An UNSCOPED secret with membership would be
	// a behavior change: it is visible from every scope today, and an EMPTY set is
	// what preserves that under AG-Q1(b).
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-global'`); n != 0 {
		t.Errorf("UNSCOPED secret got membership — that silently narrows a global secret")
	}
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-dev'`); n != 0 {
		t.Errorf("secret in an agency-less scope got membership")
	}
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-orphan'`); n != 0 {
		t.Errorf("secret naming a nonexistent scope got membership")
	}
	if n := count(`SELECT COUNT(*) FROM env_var_agencies WHERE env_var_id='v-prod' AND agency_id='ag-dss'`); n != 1 {
		t.Errorf("scoped variable did not inherit its scope's agency")
	}
	if n := count(`SELECT COUNT(*) FROM env_var_agencies WHERE env_var_id='v-global'`); n != 0 {
		t.Errorf("UNSCOPED variable got membership")
	}
	// AG-Q5 is Phase 3. Backfilling key membership here would enact the tightening a
	// release early — before the T2.12 report an operator is supposed to read first.
	if n := count(`SELECT COUNT(*) FROM ssh_credential_agencies`); n != 0 {
		t.Errorf("ssh_credential_agencies backfilled %d rows, want 0 — AG-Q5 is Phase 3", n)
	}

	// ON DELETE CASCADE on BOTH sides (the runner_agencies shape). Use ag-nwd for
	// the agency side: ag-dss is still referenced by the mig-430 scopes.agency_id FK,
	// which has NO cascade — that is exactly what the T2.8 delete guard exists to
	// turn into a clean 409 rather than an FK error.
	exec(`INSERT INTO ssh_credential_agencies(credential_id, agency_id) VALUES('k1','ag-nwd')`)
	exec(`INSERT INTO secret_agencies(secret_id, agency_id) VALUES('s-global','ag-nwd')`)
	exec(`DELETE FROM ssh_credentials WHERE id='k1'`)
	if n := count(`SELECT COUNT(*) FROM ssh_credential_agencies`); n != 0 {
		t.Errorf("credential delete did not cascade membership")
	}
	exec(`DELETE FROM agencies WHERE id='ag-nwd'`)
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE agency_id='ag-nwd'`); n != 0 {
		t.Errorf("agency delete did not cascade membership")
	}
	// Put s-global back to its backfilled state (no membership) for the re-up check.
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-global'`); n != 0 {
		t.Errorf("s-global membership survived its agency's deletion")
	}

	// down → up. The down must lose only membership rows, never an entity.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 670→660: %v", err)
	}
	if n := count(`SELECT COUNT(*) FROM secrets`); n != 4 {
		t.Fatalf("down migration lost secrets: %d remain, want 4", n)
	}
	var tbls int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
	    ('scope_agencies','secret_agencies','env_var_agencies','ssh_credential_agencies')`).Scan(&tbls)
	if tbls != 0 {
		t.Fatalf("membership tables still present after down: %d", tbls)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 660→670: %v", err)
	}
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN
	    ('scope_agencies','secret_agencies','env_var_agencies','ssh_credential_agencies')`).Scan(&tbls)
	if tbls != 4 {
		t.Errorf("membership tables not restored after re-up: %d", tbls)
	}
	// The backfill is IDEMPOTENT: re-running it over the same data reproduces the
	// same rows rather than colliding on the composite PK.
	if n := count(`SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-prod'`); n != 1 {
		t.Errorf("re-up backfill produced %d rows for s-prod, want 1", n)
	}
}

// TestMigrate680RoundTrip (agencies plan T2.3 / AG-Q2b): runs.agencies_json is
// backfilled from the scalar snapshot, NULL becomes the general-pool empty array,
// and the scalar survives untouched — which is what makes the dual-write release
// safe to roll back.
func TestMigrate680RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "runsagency680.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(670); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 670: %v", err)
	}
	const now = "2026-01-01T00:00:00Z"
	ins := func(id, agency string) {
		t.Helper()
		var a any
		if agency != "" {
			a = agency
		}
		if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agency)
		     VALUES(?,'j','bash','queued','seed','manual',?,?)`, id, now, a); err != nil {
			t.Fatalf("seed run %s: %v", id, err)
		}
	}
	ins("r-dss", "DSS")
	ins("r-general", "")
	// An agency name containing a quote and a backslash — naive string concatenation
	// would emit invalid JSON here, and invalid JSON makes json_each throw INSIDE the
	// claim query, which stops dispatch fleet-wide. json_array() escapes it correctly.
	ins("r-weird", `He said "hi"\z`)

	if err := m.Migrate(680); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 680: %v", err)
	}

	read := func(id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(`SELECT agencies_json FROM runs WHERE id=?`, id).Scan(&s); err != nil {
			t.Fatalf("read agencies_json for %s: %v", id, err)
		}
		return s
	}
	if got := read("r-dss"); got != `["DSS"]` {
		t.Errorf("scalar agency backfill = %q, want [\"DSS\"]", got)
	}
	// A general-pool run must be the EMPTY array, not NULL — every reader (and
	// json_each) then sees a well-formed array with no special case.
	if got := read("r-general"); got != `[]` {
		t.Errorf("general-pool run agencies_json = %q, want []", got)
	}
	// The escaping case must round-trip through json_each intact.
	var extracted string
	if err := pool.QueryRow(`SELECT value FROM runs, json_each(runs.agencies_json) WHERE runs.id='r-weird'`).Scan(&extracted); err != nil {
		t.Fatalf("json_each over the backfilled array failed — this would break claimRun: %v", err)
	}
	if extracted != `He said "hi"\z` {
		t.Errorf("json_each round-trip = %q, want the original name", extracted)
	}
	// The scalar is untouched: a rolled-back server still reads a correct agency.
	var scalar string
	if err := pool.QueryRow(`SELECT agency FROM runs WHERE id='r-dss'`).Scan(&scalar); err != nil || scalar != "DSS" {
		t.Errorf("runs.agency = %q (err %v), want DSS — the dual-write rollback path depends on it", scalar, err)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 680→670: %v", err)
	}
	var hasCol int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('runs') WHERE name='agencies_json'`).Scan(&hasCol)
	if hasCol != 0 {
		t.Fatalf("runs.agencies_json still present after down")
	}
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 3 {
		t.Fatalf("down migration lost runs: %d remain, want 3", runs)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 670→680: %v", err)
	}
	if got := read("r-dss"); got != `["DSS"]` {
		t.Errorf("agencies_json not restored after re-up: %q", got)
	}
}

// TestMigrate700RoundTrip (T3.9): the legacy 1:1 columns are dropped once no reader
// remains, and the down migration re-derives them from the join tables that
// superseded them — lossily, by construction, which is the whole point of the
// change. A scalar cannot hold a set.
func TestMigrate700RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "drop700.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(690); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 690: %v", err)
	}
	const now = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-a','A',?)`, now)
	exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-b','B',?)`, now)
	exec(`INSERT INTO scopes(id, name, source, created_at) VALUES('sc','multi','amadeus',?)`, now)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc','ag-b')`)
	exec(`INSERT INTO scope_agencies(scope_id, agency_id) VALUES('sc','ag-a')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, agencies_json)
	      VALUES('r1','j','bash','queued','t','manual',?,'["B","A"]')`, now)

	if err := m.Migrate(700); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 700: %v", err)
	}
	has := func(table, col string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n); err != nil {
			t.Fatalf("pragma: %v", err)
		}
		return n
	}
	if has("runs", "agency") != 0 {
		t.Error("runs.agency survived migration 700")
	}
	if has("scopes", "agency_id") != 0 {
		t.Error("scopes.agency_id survived migration 700")
	}
	// The membership that replaced them is untouched — the columns were the derived
	// values, not the source.
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc'`).Scan(&n)
	if n != 2 {
		t.Errorf("scope_agencies rows after the drop = %d, want 2", n)
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 700→690: %v", err)
	}
	if has("runs", "agency") != 1 || has("scopes", "agency_id") != 1 {
		t.Fatal("down migration did not restore the columns")
	}
	// Re-derived, and LOSSY on purpose: a scope in two agencies collapses to one.
	// Deterministically the alphabetically first, so a down/up/down cycle is stable
	// rather than picking a different agency each time.
	var scopeAgency, runAgency string
	_ = pool.QueryRow(`SELECT COALESCE(agency_id,'') FROM scopes WHERE id='sc'`).Scan(&scopeAgency)
	_ = pool.QueryRow(`SELECT COALESCE(agency,'') FROM runs WHERE id='r1'`).Scan(&runAgency)
	if scopeAgency != "ag-a" {
		t.Errorf("re-derived scopes.agency_id = %q, want ag-a (alphabetically first by name)", scopeAgency)
	}
	if runAgency != "A" {
		t.Errorf("re-derived runs.agency = %q, want A (alphabetically first)", runAgency)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("up 690→700: %v", err)
	}
	if has("runs", "agency") != 0 || has("scopes", "agency_id") != 0 {
		t.Error("re-up did not drop the columns again")
	}
	// Full fidelity is restored because the join tables were never the lossy part.
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc'`).Scan(&n)
	if n != 2 {
		t.Errorf("scope membership after the round trip = %d, want 2", n)
	}
}

// TestMigrate710RoundTrip (LU-6): the entity-code registry backfills every
// existing definition as live, hands out non-reused codes, and adds the run
// column that names a log folder.
//
// The properties worth pinning are the ones the whole design rests on: that the
// partial unique index permits a tuple to recur over time (an entity deleted and
// recreated is deliberately a DIFFERENT entity, LU-Q6(b)) while forbidding two
// live rows for it; and that AUTOINCREMENT never reissues a freed code, which is
// what makes it safe to leave a dead entity's log folder in place.
func TestMigrate710RoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "codes710.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(700); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 700: %v", err)
	}
	const now = "2026-01-01T00:00:00Z"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Seed BEFORE the up-step: TestMigrateUpDown only exercises empty DBs, so a
	// backfill bug is invisible unless data is present at the moment it runs.
	// The same NAME under both sources, because PRIMARY KEY (source, name) makes
	// the git and amadeus namespaces deliberately disjoint — they must not
	// collapse onto one code.
	exec(`INSERT INTO jobs(source, name, run_type, created_at) VALUES('git','deploy','bash',?)`, now)
	exec(`INSERT INTO jobs(source, name, run_type, created_at) VALUES('amadeus','deploy','bash',?)`, now)
	exec(`INSERT INTO workflows(source, name, created_at) VALUES('git','deploy',?)`, now)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r-old','deploy','bash','success','t','manual',?)`, now)

	if err := m.Migrate(710); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 710: %v", err)
	}
	has := func(table, col string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, table, col).Scan(&n); err != nil {
			t.Fatalf("pragma: %v", err)
		}
		return n
	}
	if has("runs", "entity_code") != 1 {
		t.Fatal("migration 710 did not add runs.entity_code")
	}

	// All three definitions are backfilled, live, with distinct codes.
	codeOf := func(kind, source, name string) int64 {
		t.Helper()
		var c int64
		if err := pool.QueryRow(`SELECT code FROM entity_codes
			WHERE kind=? AND source=? AND name=? AND deleted_at IS NULL`,
			kind, source, name).Scan(&c); err != nil {
			t.Fatalf("no live code for %s/%s/%s: %v", kind, source, name, err)
		}
		return c
	}
	gitJob := codeOf("job", "git", "deploy")
	amaJob := codeOf("job", "amadeus", "deploy")
	gitWF := codeOf("workflow", "git", "deploy")
	seen := map[int64]bool{gitJob: true, amaJob: true, gitWF: true}
	if len(seen) != 3 {
		t.Errorf("three distinct entities sharing the name 'deploy' collapsed onto %d code(s)", len(seen))
	}

	// The pre-migration run keeps a NULL code: its log is at the flat path and
	// stays there (LU-Q8(a)). Back-filling it would point the reader at a folder
	// that does not exist.
	var code sql.NullString
	if err := pool.QueryRow(`SELECT entity_code FROM runs WHERE id='r-old'`).Scan(&code); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if code.Valid {
		t.Errorf("pre-migration run got entity_code=%q, want NULL (its log is at the flat path)", code.String)
	}

	// A second LIVE row for a tuple is refused by the partial unique index.
	if _, err := pool.Exec(`INSERT INTO entity_codes(kind, source, name, created_at)
	                        VALUES('job','git','deploy',?)`, now); err == nil {
		t.Error("a second live code for one tuple was allowed; the partial unique index is not doing its job")
	}

	// But once the live row is stamped deleted, the tuple may recur — with a
	// FRESH code, never the freed one (LU-Q6(b) + AUTOINCREMENT).
	exec(`UPDATE entity_codes SET deleted_at=? WHERE code=?`, now, gitJob)
	exec(`INSERT INTO entity_codes(kind, source, name, created_at) VALUES('job','git','deploy',?)`, now)
	reborn := codeOf("job", "git", "deploy")
	if reborn == gitJob {
		t.Errorf("recreated entity reused code %d; a new entity must never inherit a dead one's log folder", gitJob)
	}
	var eras int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM entity_codes WHERE kind='job' AND source='git' AND name='deploy'`).Scan(&eras)
	if eras != 2 {
		t.Errorf("history rows for the recurring tuple = %d, want 2 (both eras retained)", eras)
	}

	// Down drops the registry and the column; the run rows survive untouched.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 710→700: %v", err)
	}
	if has("runs", "entity_code") != 0 {
		t.Error("down migration left runs.entity_code behind")
	}
	var tables int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entity_codes'`).Scan(&tables)
	if tables != 0 {
		t.Error("down migration left the entity_codes table behind")
	}
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 1 {
		t.Errorf("runs after rollback = %d, want 1 (a rollback must not destroy run history)", runs)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("up 700→710: %v", err)
	}
	if has("runs", "entity_code") != 1 {
		t.Error("re-up did not restore runs.entity_code")
	}
}

// TestMigrate730WebhookBackfill pins the upgrade guarantee behind F2-3. The
// four webhook columns (migration 060) default to 0 and were written by the
// settings page but read by nothing, so every install has effectively been
// running "webhook enabled, all events" while storing zeros. Wiring the flags
// without back-filling them would have stopped every working webhook the moment
// an operator upgraded — with the failure visible only in GitLab's own delivery
// log. This asserts the back-fill turns a stored-zero row on.
func TestMigrate730WebhookBackfill(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "wh.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(720); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 720: %v", err)
	}

	// The pre-730 state of every real install: a settings row whose webhook
	// flags are the untouched defaults.
	if _, err := pool.Exec(`INSERT INTO gitlab_config(id) VALUES(1)`); err != nil {
		t.Fatalf("seed gitlab_config: %v", err)
	}
	var enabled, push, mr, tag int
	_ = pool.QueryRow(`SELECT webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag FROM gitlab_config WHERE id=1`).
		Scan(&enabled, &push, &mr, &tag)
	if enabled != 0 {
		t.Fatalf("pre-condition: webhook_enabled = %d, want 0 (the whole point of the back-fill)", enabled)
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate 720→730: %v", err)
	}
	_ = pool.QueryRow(`SELECT webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag FROM gitlab_config WHERE id=1`).
		Scan(&enabled, &push, &mr, &tag)
	if enabled != 1 || push != 1 || mr != 1 || tag != 1 {
		t.Errorf("after 730: enabled=%d push=%d mr=%d tag=%d, want all 1 — an upgrade must not silently disable a working webhook",
			enabled, push, mr, tag)
	}

	// Down is a no-op by design: on the way down the flags stop being read, and
	// re-zeroing them would destroy a deliberate operator choice.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate 730→720: %v", err)
	}
	_ = pool.QueryRow(`SELECT webhook_enabled FROM gitlab_config WHERE id=1`).Scan(&enabled)
	if enabled != 1 {
		t.Errorf("after down: webhook_enabled = %d, want 1 (the down must not re-zero a stored choice)", enabled)
	}
}

// TestMigrate830DataRoundTrip is the fence the RA plan's §10 asks for: migration
// 830 rebuilds `secrets`, and that table holds KEK-SEALED BLOBS. A rebuild that
// re-encoded a BLOB — coerced it through TEXT, dropped a trailing NUL, mangled a
// byte that is not valid UTF-8 — would silently destroy every stored secret in the
// installation, and NOTHING else in the test suite would notice: the rows would
// still be there, still the right count, and only fail at reveal time in
// production, long after the upgrade.
//
// So this seeds adversarial bytes (embedded NUL, 0xFF, invalid UTF-8, a high
// surrogate) at 820, cycles 830 down and up, and asserts byte equality. It also
// covers the ownership dimension itself: two same-named rows in one scope must
// survive the up (they are the whole point), and the down must collapse them
// deterministically rather than abort with a UNIQUE violation.
func TestMigrate830DataRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "rt830.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(820); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 820: %v", err)
	}
	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// Bytes chosen to break a TEXT round trip: a NUL in the middle, a trailing NUL,
	// 0xFF (never valid UTF-8), and a lone high-surrogate encoding.
	ciphertext := []byte{0x00, 0x01, 0xFF, 0xFE, 'a', 0x00, 0xED, 0xA0, 0x80, 0x7F, 0x00}
	nonce := []byte{0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF, 0x00}
	dek := []byte{0x80, 0x00, 0x7F, 0xFF}
	seed(`INSERT INTO secrets(id,key,scope,source,ciphertext,nonce,wrapped_dek,kek_version,created_at)
	      VALUES('s1','TOKEN','prod','stored',?,?,?,3,'2026-01-01T00:00:00Z')`, ciphertext, nonce, dek)
	seed(`INSERT INTO env_vars(id,key,scope,value,created_at)
	      VALUES('v1','REGION','prod','eu-west','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO ssh_credentials(id,label,source,created_at,last_modified_at)
	      VALUES('k1','deploy_key','stored','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	// An inbound reference that must still resolve after the rebuild: row ids are
	// preserved by the copy, so membership survives even though the FK is not
	// enforced inside the migration transaction.
	seed(`INSERT INTO agencies(id,name,created_at) VALUES('ag-a','TeamA','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO secret_agencies(secret_id,agency_id) VALUES('s1','ag-a')`)
	seed(`INSERT INTO env_var_agencies(env_var_id,agency_id) VALUES('v1','ag-a')`)
	// gitlab_config.pat_secret_id is ON DELETE SET NULL, so a naive DROP TABLE
	// unlinks the PAT and breaks repo sync — silently, and only at the next sync.
	seed(`INSERT INTO gitlab_config(id,base_url,pat_secret_id) VALUES(1,'https://gl','s1')`)

	if err := m.Migrate(830); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 830: %v", err)
	}

	assertBlobs := func(stage string) {
		t.Helper()
		var gotC, gotN, gotD []byte
		var kekVer int
		if err := pool.QueryRow(
			`SELECT ciphertext, nonce, wrapped_dek, kek_version FROM secrets WHERE id='s1'`).
			Scan(&gotC, &gotN, &gotD, &kekVer); err != nil {
			t.Fatalf("%s: read sealed secret: %v", stage, err)
		}
		if !bytes.Equal(gotC, ciphertext) {
			t.Errorf("%s: ciphertext MUTATED by the rebuild\n got %#v\nwant %#v", stage, gotC, ciphertext)
		}
		if !bytes.Equal(gotN, nonce) {
			t.Errorf("%s: nonce mutated\n got %#v\nwant %#v", stage, gotN, nonce)
		}
		if !bytes.Equal(gotD, dek) {
			t.Errorf("%s: wrapped_dek mutated\n got %#v\nwant %#v", stage, gotD, dek)
		}
		if kekVer != 3 {
			t.Errorf("%s: kek_version = %d, want 3 — a lost version breaks rotation", stage, kekVer)
		}
	}
	assertBlobs("after up")

	// Membership still resolves — the rebuild preserved the id it points at.
	for _, q := range []struct{ what, query string }{
		{"secret_agencies", `SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s1'`},
		{"env_var_agencies", `SELECT COUNT(*) FROM env_var_agencies WHERE env_var_id='v1'`},
	} {
		var n int
		if err := pool.QueryRow(q.query).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q.what, err)
		}
		if n != 1 {
			t.Errorf("%s row lost across the rebuild (got %d) — ON DELETE CASCADE fired", q.what, n)
		}
	}
	var pat sql.NullString
	if err := pool.QueryRow(`SELECT pat_secret_id FROM gitlab_config WHERE id=1`).Scan(&pat); err != nil {
		t.Fatalf("gitlab_config: %v", err)
	}
	if pat.String != "s1" {
		t.Errorf("gitlab_config.pat_secret_id = %q, want \"s1\" — ON DELETE SET NULL unlinked the PAT", pat.String)
	}

	// The point of the phase: two departments, one key, one scope.
	seed(`INSERT INTO secrets(id,key,scope,source,vault_ref,owner_agency,created_at)
	      VALUES('s2','TOKEN','prod','vault','kv/b#t','ag-b','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO env_vars(id,key,scope,value,owner_agency,created_at)
	      VALUES('v2','REGION','prod','us-east','ag-b','2026-01-01T00:00:00Z')`)
	seed(`INSERT INTO ssh_credentials(id,label,source,owner_agency,created_at,last_modified_at)
	      VALUES('k2','deploy_key','stored','ag-b','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)

	// ...and the unowned case stays strict: a second '' row in the same scope is
	// still a collision, which is what NOT NULL DEFAULT '' buys over a nullable column.
	if _, err := pool.Exec(`INSERT INTO secrets(id,key,scope,source,vault_ref,created_at)
	      VALUES('s3','TOKEN','prod','vault','kv/c#t','2026-01-01T00:00:00Z')`); err == nil {
		t.Error("a second UNOWNED secret with the same key+scope was accepted — " +
			"a nullable owner column would regress the constraint exactly this way")
	}

	// Down must collapse the duplicates deterministically, not abort.
	if err := m.Migrate(820); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 820: %v", err)
	}
	for _, tc := range []struct{ table, want string }{{"secrets", "s1"}, {"env_vars", "v1"}} {
		var n int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM ` + tc.table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n != 1 {
			t.Errorf("%s after down = %d rows, want 1 (MIN(id) survivor)", tc.table, n)
		}
		var id string
		_ = pool.QueryRow(`SELECT id FROM ` + tc.table).Scan(&id)
		if id != tc.want {
			t.Errorf("%s survivor = %q, want %q (deterministic MIN(id))", tc.table, id, tc.want)
		}
	}
	assertBlobs("after down")

	// And back up, because a rollback is usually followed by a retry.
	if err := m.Migrate(830); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate back up to 830: %v", err)
	}
	assertBlobs("after re-up")
}

// TestMigrate890ReactionTriggerKindRoundTrip — the RX-3 rebuild, with data.
//
// TestMigrateUpDown only proves the rebuild applies to an EMPTY database, which
// is precisely the condition under which both of this migration's failure modes
// are invisible. Two things get destroyed by the rebuild's DROP TABLEs unless
// they are stashed, and both look completely valid afterwards:
//
//   - run_agencies is cascade-wiped (ON DELETE CASCADE → runs). An empty
//     isolation index is a legal state; the symptom is every run silently
//     becoming unclaimable by every departmental runner, in production, later.
//   - runs.workflow_run_id is blanked (ON DELETE SET NULL → workflow_runs). A
//     NULL is a legal value; the symptom is History losing the link between a
//     workflow and the runs it produced, for every workflow that ever ran.
//
// Neither shows up as an error. This test is the only thing standing between
// the stash logic and a silent data loss on upgrade.
func TestMigrate890ReactionTriggerKindRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "reaction890.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(880); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 880: %v", err)
	}

	// A workflow run, a run linked to it, and the run's agency membership —
	// exactly the three shapes the rebuild endangers.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-1','nightly','success','tester','scheduled','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind,
	                       workflow_run_id, agencies_json, created_at)
	      VALUES('run-1','extract','bash','success','tester','workflow','wfr-1','["fin"]','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO run_agencies(run_id, agency) VALUES('run-1','fin')`)

	// Before 890 both CHECKs must reject 'reaction'.
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-rx','x','bash','success','t','reaction','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("pre-890: a 'reaction' run should have failed the trigger_kind CHECK")
	}
	if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-rx','x','success','t','reaction','2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("pre-890: a 'reaction' workflow run should have failed the trigger_kind CHECK")
	}

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate 880→890: %v", err)
	}

	assertSurvived := func(when string) {
		t.Helper()
		var wfID sql.NullString
		if err := pool.QueryRow(`SELECT workflow_run_id FROM runs WHERE id='run-1'`).Scan(&wfID); err != nil {
			t.Fatalf("%s: read run: %v", when, err)
		}
		if !wfID.Valid || wfID.String != "wfr-1" {
			t.Errorf("%s: runs.workflow_run_id = %v, want wfr-1 — DROP TABLE workflow_runs "+
				"SET NULLs this for every run unless it is stashed and restored", when, wfID)
		}
		var agencies int
		if err := pool.QueryRow(`SELECT COUNT(*) FROM run_agencies WHERE run_id='run-1'`).Scan(&agencies); err != nil {
			t.Fatalf("%s: count run_agencies: %v", when, err)
		}
		if agencies != 1 {
			t.Errorf("%s: run_agencies rows = %d, want 1 — DROP TABLE runs cascade-wipes this "+
				"table unless it is stashed and restored", when, agencies)
		}
		var wfr int
		_ = pool.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE id='wfr-1'`).Scan(&wfr)
		if wfr != 1 {
			t.Errorf("%s: workflow_runs rows = %d, want 1", when, wfr)
		}
	}
	assertSurvived("after up")

	// The point of the migration: both tables now accept 'reaction'.
	if _, err := pool.Exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-rx','x','bash','success','t','reaction','2026-01-01T00:00:00Z')`); err != nil {
		t.Errorf("post-890: a 'reaction' run should be accepted: %v", err)
	}
	if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-rx','x','success','t','reaction','2026-01-01T00:00:00Z')`); err != nil {
		t.Errorf("post-890: a 'reaction' workflow run should be accepted: %v", err)
	}

	// The vocabularies stay DIFFERENT: runs keeps 'workflow', workflow_runs
	// must not gain it. Pinned because "while we're rebuilding both, let's
	// unify them" is the obvious tidy-up and it would be a semantic change.
	if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-nope','x','success','t','workflow','2026-01-01T00:00:00Z')`); err == nil {
		t.Error("workflow_runs.trigger_kind must NOT have gained 'workflow' — the two vocabularies " +
			"differ deliberately and this migration adds one value to each, nothing more")
	}

	// Down must narrow again — and must refuse while a 'reaction' row exists,
	// rather than silently rewriting a run's provenance.
	if err := m.Steps(-1); err == nil {
		t.Error("890 down should FAIL while a 'reaction' row exists; silently relabelling it " +
			"would put a lie in History")
	}
	// Clear the dirty state the failed down left behind, drop the offending
	// rows, and retry: now it must succeed with the data intact.
	if err := m.Force(890); err != nil {
		t.Fatalf("force 890: %v", err)
	}
	exec(`DELETE FROM runs WHERE trigger_kind = 'reaction'`)
	exec(`DELETE FROM workflow_runs WHERE trigger_kind = 'reaction'`)
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate 890→880 after clearing reaction rows: %v", err)
	}
	assertSurvived("after down")
}

// TestMigrate910ServiceAccountsRoundTrip covers the ET-A table. It is purely
// additive, so the risk is not data loss on rebuild — it is that the
// one-shape-per-row CHECK copied from access_grants (810) does not actually
// bind, which would let a token exist that is neither agency-bound nor
// unrestricted and therefore resolves to an identity nobody authored.
func TestMigrate910ServiceAccountsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "svcacct910.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(910); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 910: %v", err)
	}

	if _, err := pool.Exec(`INSERT INTO agencies(id, name, created_at) VALUES('ag-1','Tax','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed agency: %v", err)
	}

	// The two legal shapes.
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-1','nagios','hash-1','operator','ag-1',0,'admin@example.com','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("agency-bound account rejected: %v", err)
	}
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-2','ci','hash-2','admin',NULL,1,'admin@example.com','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("unrestricted account rejected: %v", err)
	}

	// Neither shape, and both shapes: the CHECK must refuse each.
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-3','shapeless','hash-3','operator',NULL,0,'admin@example.com','2026-08-11T00:00:00Z')`); err == nil {
		t.Error("a row bound to neither an agency nor all scopes was accepted — the 810-style CHECK is not binding")
	}
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-4','both','hash-4','operator','ag-1',1,'admin@example.com','2026-08-11T00:00:00Z')`); err == nil {
		t.Error("a row bound to BOTH an agency and all scopes was accepted")
	}

	// Name and token hash are the two identity columns; both must be unique.
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-5','nagios','hash-5','operator','ag-1',0,'admin@example.com','2026-08-11T00:00:00Z')`); err == nil {
		t.Error("a duplicate account name was accepted")
	}
	if _, err := pool.Exec(`
		INSERT INTO service_accounts(id, name, token_hash, role, agency_id, all_scopes, created_by, created_at)
		VALUES('sa-6','other','hash-1','operator','ag-1',0,'admin@example.com','2026-08-11T00:00:00Z')`); err == nil {
		t.Error("two accounts sharing one token hash were accepted — that is two principals with one credential")
	}

	// Deleting the agency cascades the account away rather than stranding a
	// token whose grant no longer resolves.
	if _, err := pool.Exec(`DELETE FROM agencies WHERE id='ag-1'`); err != nil {
		t.Fatalf("delete agency: %v", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM service_accounts WHERE id='sa-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("agency-bound account survived its agency's deletion (%d rows) — its grant would resolve to nothing", n)
	}

	// Down must remove the table cleanly.
	if err := m.Migrate(900); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 900: %v", err)
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='service_accounts'`).Scan(&n); err != nil {
		t.Fatalf("count table: %v", err)
	}
	if n != 0 {
		t.Error("service_accounts survived the down migration")
	}
}

// TestMigrate920DefinitionRevisionsRoundTrip covers RH. The table is additive, so
// the risk is not data loss — it is that the soft-delete columns land on the
// wrong tables, or that the revision CHECKs do not bind and let a snapshot exist
// under a kind or action nothing knows how to read back.
func TestMigrate920DefinitionRevisionsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "rev920.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(910); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 910: %v", err)
	}
	// Seed a definition BEFORE the migration so the ALTERs are exercised against
	// real data rather than an empty table.
	if _, err := pool.Exec(`INSERT INTO jobs(name, source, run_type, concurrency_policy, synced_at)
	                        VALUES('nightly','amadeus','bash','Allow','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := m.Migrate(920); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 920: %v", err)
	}

	// The pre-existing row survived and defaults to "not deleted".
	var deletedAt sql.NullString
	if err := pool.QueryRow(`SELECT deleted_at FROM jobs WHERE name='nightly'`).Scan(&deletedAt); err != nil {
		t.Fatalf("read job after migrate: %v", err)
	}
	if deletedAt.Valid {
		t.Errorf("deleted_at = %q on a pre-existing row, want NULL", deletedAt.String)
	}
	// All three definition tables carry the pair.
	for _, table := range []string{"jobs", "workflows", "schedules"} {
		if _, err := pool.Exec(`SELECT deleted_at, deleted_by FROM ` + table + ` LIMIT 1`); err != nil {
			t.Errorf("%s is missing the soft-delete columns: %v", table, err)
		}
	}

	ins := func(kind, action string, no int) error {
		_, err := pool.Exec(`
			INSERT INTO definition_revisions(id, kind, source, name, revision_no, action, actor, created_at, snapshot_json, snapshot_digest)
			VALUES(?, ?, 'amadeus', 'nightly', ?, ?, 'a@example.com', '2026-08-11T00:00:00Z', '{}', 'deadbeef')`,
			kind+action+strconv.Itoa(no), kind, no, action)
		return err
	}
	if err := ins("job", "created", 1); err != nil {
		t.Fatalf("valid revision rejected: %v", err)
	}
	if err := ins("script", "created", 2); err == nil {
		t.Error("a revision with an unknown kind was accepted")
	}
	if err := ins("job", "vandalised", 3); err == nil {
		t.Error("a revision with an unknown action was accepted")
	}
	// revision_no is the ordering key; two rows sharing one would make history
	// ambiguous and "restore revision 1" nondeterministic.
	if err := ins("job", "updated", 1); err == nil {
		t.Error("a duplicate revision_no for one definition was accepted")
	}

	// Down removes the table; the columns deliberately stay (see the down file).
	if err := m.Migrate(910); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 910: %v", err)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='definition_revisions'`).Scan(&n); err != nil {
		t.Fatalf("count table: %v", err)
	}
	if n != 0 {
		t.Error("definition_revisions survived the down migration")
	}
}

// TestMigrate930SLAMonitoringRoundTrip covers SL's additive columns and, more
// importantly, that the down is a REAL inverse. 920 and 930 both drop columns
// rather than leaving them, so an operator who rolls back and forward again is
// not wedged on "duplicate column name" — this drives that cycle.
func TestMigrate930SLAMonitoringRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sla930.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(930); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 930: %v", err)
	}

	if _, err := pool.Exec(`
		INSERT INTO jobs(name, source, run_type, concurrency_policy, synced_at, warn_after_seconds, must_finish_by)
		VALUES('nightly','amadeus','bash','Allow','t', 1800, '06:00')`); err != nil {
		t.Fatalf("seed job with deadlines: %v", err)
	}
	var warn int
	var by string
	if err := pool.QueryRow(`SELECT warn_after_seconds, must_finish_by FROM jobs WHERE name='nightly'`).Scan(&warn, &by); err != nil {
		t.Fatalf("read deadlines: %v", err)
	}
	if warn != 1800 || by != "06:00" {
		t.Errorf("deadlines round-tripped as %d/%q, want 1800/06:00", warn, by)
	}
	if _, err := pool.Exec(`
		INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at, sla_warned_at)
		VALUES('r1','nightly','bash','running','scheduler','scheduled','2026-08-11T00:00:00Z','2026-08-11T01:00:00Z')`); err != nil {
		t.Fatalf("seed run with warn stamp: %v", err)
	}

	// Down THEN up again — the cycle a partial down would break.
	if err := m.Migrate(920); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 920: %v", err)
	}
	if _, err := pool.Exec(`SELECT warn_after_seconds FROM jobs LIMIT 1`); err == nil {
		t.Error("warn_after_seconds survived the down migration; re-applying 930 would fail on a duplicate column")
	}
	if err := m.Migrate(930); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-applying 930 after a rollback failed — the down was not a true inverse: %v", err)
	}
}

// TestMigrate940QueuePolicyRoundTrip covers the jobs REBUILD, which is the
// expensive and dangerous half of QP.
//
// The failure this exists to catch is not a lost column — that shows up
// immediately — but a lost TRIGGER. jobs carries five delete-cascade triggers
// and DROP TABLE destroys them silently along with the table; a rebuild that
// recreates four of five leaves a table whose deletes stop cascading, and
// nothing complains until a recreated job of the same name inherits an orphan.
func TestMigrate940QueuePolicyRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "queue940.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(930); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 930: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// A Replace job (whose value is about to be removed), an Allow job, and the
	// full set of dependent rows the triggers are supposed to cascade.
	exec(`INSERT INTO jobs(name, source, run_type, concurrency_policy, synced_at, deleted_at, warn_after_seconds)
	      VALUES('legacy','git','bash','Replace','t', NULL, 900)`)
	exec(`INSERT INTO jobs(name, source, run_type, concurrency_policy, synced_at)
	      VALUES('plain','git','bash','Allow','t')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position)
	      VALUES('git','job','plain','default','0 2 * * *',0)`)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at) VALUES('git','job','plain','op','t')`)
	exec(`INSERT INTO pending_runs(id, kind, name, source, run_at, scheduled_by, created_at)
	      VALUES('p1','job','plain','git','2026-08-11T00:00:00Z','op','2026-08-11T00:00:00Z')`)

	if err := m.Migrate(940); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 940: %v", err)
	}

	// Data survived, and the decorative value was coerced to what it actually did.
	var policy string
	var warn int
	if err := pool.QueryRow(`SELECT concurrency_policy, warn_after_seconds FROM jobs WHERE name='legacy'`).Scan(&policy, &warn); err != nil {
		t.Fatalf("read rebuilt job: %v", err)
	}
	if policy != "Allow" {
		t.Errorf("a Replace job became %q, want Allow — Replace never did anything, so Allow is what it was really doing", policy)
	}
	if warn != 900 {
		t.Errorf("warn_after_seconds = %d after the rebuild, want 900 — an SL column was dropped by the copy", warn)
	}

	// The new value is accepted, the removed one is not.
	if _, err := pool.Exec(`UPDATE jobs SET concurrency_policy='Queue' WHERE name='plain'`); err != nil {
		t.Errorf("Queue rejected by the new CHECK: %v", err)
	}
	if _, err := pool.Exec(`UPDATE jobs SET concurrency_policy='Replace' WHERE name='plain'`); err == nil {
		t.Error("Replace is still accepted; the CHECK was not narrowed")
	}

	// THE TRIGGERS. Delete the job and every dependent must go with it.
	exec(`UPDATE jobs SET concurrency_policy='Allow' WHERE name='plain'`)
	exec(`DELETE FROM jobs WHERE source='git' AND name='plain'`)
	for _, c := range []struct{ table, q string }{
		{"definition_schedules", `SELECT COUNT(*) FROM definition_schedules WHERE owner_name='plain'`},
		{"paused_jobs", `SELECT COUNT(*) FROM paused_jobs WHERE name='plain'`},
		{"pending_runs", `SELECT COUNT(*) FROM pending_runs WHERE name='plain'`},
	} {
		var n int
		if err := pool.QueryRow(c.q).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.table, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d row(s) after the job was deleted — the rebuild lost that trigger", c.table, n)
		}
	}

	// The queue columns and the priority column exist and default sanely.
	exec(`INSERT INTO pending_runs(id, kind, name, source, run_at, scheduled_by, created_at, concurrency_key, gate_kind)
	      VALUES('p2','job','legacy','git','2026-08-11T00:00:00Z','op','2026-08-11T00:00:00Z','git/legacy','concurrency')`)
	var gate string
	if err := pool.QueryRow(`SELECT gate_kind FROM pending_runs WHERE id='p2'`).Scan(&gate); err != nil || gate != "concurrency" {
		t.Errorf("gate_kind round-tripped as %q (%v)", gate, err)
	}
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r1','legacy','bash','queued','op','manual','2026-08-11T00:00:00Z')`)
	var prio int
	if err := pool.QueryRow(`SELECT priority FROM runs WHERE id='r1'`).Scan(&prio); err != nil || prio != 0 {
		t.Errorf("runs.priority = %d (%v), want a 0 default", prio, err)
	}

	// Down, then up again — the cycle a careless rebuild breaks.
	if err := m.Migrate(930); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 930: %v", err)
	}
	if err := m.Migrate(940); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-applying 940 after a rollback failed: %v", err)
	}
}

// TestMigrate950SubworkflowsRoundTrip guards the FK stash-and-restore.
//
// runs.workflow_run_id is a real foreign key into workflow_runs with ON DELETE
// SET NULL, and FK enforcement is live during migrations — so the rebuild's
// DROP TABLE fires that action and NULLs the parent link on every child run
// ever recorded. The failure is silent and total: History de-nests itself
// across the whole install and nothing errors. 830 discovered it, 890
// documented it, and this is the third table to need the recipe.
func TestMigrate950SubworkflowsRoundTrip(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sw950.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(940); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 940: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-1','nightly','success','tester','scheduled','2026-08-11T00:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, workflow_run_id, created_at)
	      VALUES('run-1','extract','bash','success','tester','workflow','wfr-1','2026-08-11T00:00:00Z')`)
	// An unparented run, to prove the restore does not invent links.
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('run-2','standalone','bash','success','tester','manual','2026-08-11T00:00:00Z')`)

	// Nesting must be REJECTED before the migration — that is what 890 chose.
	if _, err := pool.Exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at)
	                        VALUES('wfr-x','child','running','tester','workflow','2026-08-11T00:00:00Z')`); err == nil {
		t.Fatal("pre-950 schema accepted trigger_kind='workflow'; the test's premise is wrong")
	}

	if err := m.Migrate(950); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 950: %v", err)
	}

	// THE POINT OF THIS TEST.
	var parent sql.NullString
	if err := pool.QueryRow(`SELECT workflow_run_id FROM runs WHERE id='run-1'`).Scan(&parent); err != nil {
		t.Fatalf("read child run: %v", err)
	}
	if !parent.Valid || parent.String != "wfr-1" {
		t.Fatalf("runs.workflow_run_id = %v after the rebuild, want wfr-1 — the ON DELETE SET NULL "+
			"cascade destroyed it and the stash did not restore it", parent)
	}
	var orphan sql.NullString
	_ = pool.QueryRow(`SELECT workflow_run_id FROM runs WHERE id='run-2'`).Scan(&orphan)
	if orphan.Valid {
		t.Errorf("the restore invented a parent link for an unparented run: %q", orphan.String)
	}

	// Nesting is now legal, and the parent columns work.
	exec(`INSERT INTO workflow_runs(id, workflow_name, status, triggered_by, trigger_kind, created_at,
	                                parent_workflow_run_id, parent_node_id, workflow_depth)
	      VALUES('wfr-2','child','running','tester','workflow','2026-08-11T00:01:00Z','wfr-1','node-a',1)`)
	var depth int
	var pnode string
	if err := pool.QueryRow(
		`SELECT workflow_depth, parent_node_id FROM workflow_runs WHERE id='wfr-2'`).Scan(&depth, &pnode); err != nil {
		t.Fatalf("read nested run: %v", err)
	}
	if depth != 1 || pnode != "node-a" {
		t.Errorf("nested run = depth %d / node %q, want 1 / node-a", depth, pnode)
	}

	// Deleting the parent orphans the child rather than deleting it: a child's
	// own history outlives its provenance.
	exec(`DELETE FROM workflow_runs WHERE id='wfr-1'`)
	var stillThere int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM workflow_runs WHERE id='wfr-2'`).Scan(&stillThere)
	if stillThere != 1 {
		t.Error("deleting a parent deleted its child run; the FK should SET NULL, not CASCADE")
	}

	// Down, then up again.
	if err := m.Migrate(940); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 940: %v", err)
	}
	if err := m.Migrate(950); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-applying 950 failed: %v", err)
	}
}

// TestMigrate1010UIDBackfill exercises R2-1's backfill WITH DATA PRESENT
// (the rbac2 plan). The migration stamps run history with the
// definition uid that migration 1000 minted, and the interesting cases are the
// ones where it must decline to guess:
//
//   - runs / workflow_runs carry a source column (NULL ⇒ git, the 170
//     convention), so their join is exact even for the same name in both pools;
//   - activity carries NO source column, so a name present in BOTH pools must
//     backfill to NULL rather than attribute one pool's history to the other;
//   - a run whose definition is already gone stays NULL.
func TestMigrate1010UIDBackfill(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "uidbf.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1000); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1000: %v", err)
	}

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}

	// 'ambiguous' exists in BOTH source pools — legal today, since uniqueness is
	// per-source. 'only-git' exists once.
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('ambiguous','git','uid-amb-git','bash','t')`)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('ambiguous','amadeus','uid-amb-ama','bash','t')`)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('only-git','git','uid-onlygit','bash','t')`)
	exec(`INSERT INTO workflows(name, source, uid, steps, synced_at) VALUES('wf-one','git','uid-wf-git','[]','t')`)

	// Runs: one per pool for the ambiguous name, plus a legacy NULL-source row
	// (reads as git) and one whose job no longer exists.
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r-git','ambiguous','git','bash','success','t','manual','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r-ama','ambiguous','amadeus','bash','success','t','manual','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r-legacy','only-git','bash','success','t','manual','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO runs(id, job_name, job_source, run_type, status, triggered_by, trigger_kind, created_at)
	      VALUES('r-gone','deleted-job','git','bash','success','t','manual','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO workflow_runs(id, workflow_name, workflow_source, status, triggered_by, trigger_kind, created_at)
	      VALUES('wfr-1','wf-one','git','success','t','manual','2026-08-13T00:00:00Z')`)

	// Activity: the unique name must resolve, the ambiguous one must not.
	exec(`INSERT INTO activity(kind, actor, job_name, at, created_at)
	      VALUES('run-end','t','only-git','2026-08-13T00:00:00Z','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO activity(kind, actor, job_name, at, created_at)
	      VALUES('run-end','t','ambiguous','2026-08-13T00:00:00Z','2026-08-13T00:00:00Z')`)
	exec(`INSERT INTO activity(kind, actor, workflow_name, at, created_at)
	      VALUES('workflow-end','t','wf-one','2026-08-13T00:00:00Z','2026-08-13T00:00:00Z')`)

	if err := m.Migrate(1010); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1010: %v", err)
	}

	uidOf := func(q string, args ...any) sql.NullString {
		t.Helper()
		var u sql.NullString
		if err := pool.QueryRow(q, args...).Scan(&u); err != nil {
			t.Fatalf("read uid: %v\n%s", err, q)
		}
		return u
	}

	// Source-qualified joins: each pool's run gets its own pool's uid.
	if got := uidOf(`SELECT job_uid FROM runs WHERE id='r-git'`); got.String != "uid-amb-git" {
		t.Errorf("git run job_uid = %v, want uid-amb-git", got)
	}
	if got := uidOf(`SELECT job_uid FROM runs WHERE id='r-ama'`); got.String != "uid-amb-ama" {
		t.Errorf("amadeus run job_uid = %v, want uid-amb-ama", got)
	}
	// A NULL job_source is git (the 170 convention), not "unknown".
	if got := uidOf(`SELECT job_uid FROM runs WHERE id='r-legacy'`); got.String != "uid-onlygit" {
		t.Errorf("legacy NULL-source run job_uid = %v, want uid-onlygit", got)
	}
	if got := uidOf(`SELECT job_uid FROM runs WHERE id='r-gone'`); got.Valid {
		t.Errorf("run for a deleted job got job_uid %q, want NULL", got.String)
	}
	if got := uidOf(`SELECT workflow_uid FROM workflow_runs WHERE id='wfr-1'`); got.String != "uid-wf-git" {
		t.Errorf("workflow run workflow_uid = %v, want uid-wf-git", got)
	}

	// Activity's source-blind rule: resolve only what is unambiguous.
	if got := uidOf(`SELECT job_uid FROM activity WHERE job_name='only-git'`); got.String != "uid-onlygit" {
		t.Errorf("activity job_uid = %v, want uid-onlygit", got)
	}
	if got := uidOf(`SELECT job_uid FROM activity WHERE job_name='ambiguous'`); got.Valid {
		t.Errorf("activity for a name in BOTH pools got job_uid %q, want NULL "+
			"(activity has no source column; picking one would attribute the wrong pool)", got.String)
	}
	if got := uidOf(`SELECT workflow_uid FROM activity WHERE workflow_name='wf-one'`); got.String != "uid-wf-git" {
		t.Errorf("activity workflow_uid = %v, want uid-wf-git", got)
	}

	// Down drops the columns without losing the rows; re-up re-derives them.
	if err := m.Migrate(1000); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down to 1000: %v", err)
	}
	var runCount int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runCount)
	if runCount != 4 {
		t.Errorf("runs after down = %d, want 4 (the down-migration lost rows)", runCount)
	}
	if err := m.Migrate(1010); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("re-applying 1010 failed: %v", err)
	}
	if got := uidOf(`SELECT job_uid FROM runs WHERE id='r-ama'`); got.String != "uid-amb-ama" {
		t.Errorf("after re-up job_uid = %v, want uid-amb-ama", got)
	}
}

// TestMigrate1100ActivityRunnerNameBackfill exercises migration 1100's two
// backfill arms and, more importantly, the rows it must REFUSE to stamp
// (AA-1, the activity-actors plan).
//
// The column names the runner an activity row is about, because `actor` carries
// `runner:<uuid>` — filterable but unreadable. The backfill's rule is 1010's:
// stamp only where the join is exact, leave NULL otherwise. A migration that
// invented a plausible name would be worse than one that admits it cannot know,
// so the NULL cases are the load-bearing assertions here.
func TestMigrate1100ActivityRunnerNameBackfill(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "arn.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1090); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1090: %v", err)
	}

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	seed(`INSERT INTO runners(id, name, status, capabilities, registered_at, created_at)
	      VALUES('rn-live','runner-east','online','["bash"]','2026-08-01T00:00:00Z','2026-08-01T00:00:00Z')`)
	// (a) A runner-executed run: runs.runner_id resolves to a live runner.
	seed(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, runner_id, created_at)
	      VALUES('run-live','nightly','bash','success','sched','scheduled','runner','rn-live','2026-08-20T00:00:00Z')`)
	seed(`INSERT INTO activity(kind, actor, job_name, trace_id, at, created_at)
	      VALUES('run-end','runner:rn-live','nightly','run-live','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)
	// (b) A run whose runner has since been DEREGISTERED — the join finds
	//     nothing and the row must stay NULL, not acquire a guess.
	seed(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, runner_id, created_at)
	      VALUES('run-gone','nightly','bash','failure','sched','scheduled','runner',NULL,'2026-08-20T00:00:00Z')`)
	seed(`INSERT INTO activity(kind, actor, job_name, trace_id, at, created_at)
	      VALUES('run-end','runner:rn-gone','nightly','run-gone','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)
	// (c) A runner-lifecycle config row: the display name was already in
	//     `target`, so it recovers even though no runner row is needed.
	seed(`INSERT INTO activity(kind, actor, target, summary, at, created_at)
	      VALUES('config','ops@corp.example','runner:runner-west','drain initiated','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)
	// (d) An SSH-executor run — no runner was involved at all, so its run row
	//     carries runner_id NULL and the join finds nothing. (An earlier draft
	//     pointed this row at the RUNNER-executed run above, which is a
	//     combination that cannot occur; 1110 then stamped it and the test
	//     rightly failed. The fixture, not the migration, was wrong.)
	seed(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, runner_id, created_at)
	      VALUES('run-ssh','nightly','bash','success','sched','scheduled','ssh',NULL,'2026-08-20T00:00:00Z')`)
	seed(`INSERT INTO activity(kind, actor, job_name, trace_id, at, created_at)
	      VALUES('run-end','ssh-executor','nightly','run-ssh','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)
	// (e) A config row about the runners COLLECTION, not one runner (a
	//     registration-token event). `target` does not carry the prefix, so it
	//     must not be sliced.
	seed(`INSERT INTO activity(kind, actor, target, summary, at, created_at)
	      VALUES('config','ops@corp.example','runners','registration token #1 revoked','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)
	// (f) A STOPPED run: it executed on a live runner, but the kill handler
	//     wrote the run-end row with the OPERATOR's email as the actor. 1100's
	//     `actor LIKE 'runner:%'` predicate misses it; 1110 widens to the join
	//     and catches it. This is the gap AA-1 recorded and AA-2 closes.
	seed(`INSERT INTO runs(id, job_name, run_type, status, triggered_by, trigger_kind, executor, runner_id, created_at)
	      VALUES('run-killed','nightly','bash','killed','ops','manual','runner','rn-live','2026-08-20T00:00:00Z')`)
	seed(`INSERT INTO activity(kind, actor, job_name, trace_id, at, created_at)
	      VALUES('run-end','ops@corp.example','nightly','run-killed','2026-08-20T00:00:00Z','2026-08-20T00:00:00Z')`)

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1090→1100: %v", err)
	}

	nameOf := func(where string, args ...any) sql.NullString {
		t.Helper()
		var n sql.NullString
		if err := pool.QueryRow(`SELECT runner_name FROM activity WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatalf("read runner_name (%s): %v", where, err)
		}
		return n
	}

	if got := nameOf(`actor = 'runner:rn-live'`); !got.Valid || got.String != "runner-east" {
		t.Errorf("(a) live runner run: runner_name = %v, want 'runner-east'", got)
	}
	if got := nameOf(`actor = 'runner:rn-gone'`); got.Valid {
		t.Errorf("(b) deregistered runner: runner_name = %q, want NULL — the migration must not invent a name", got.String)
	}
	if got := nameOf(`summary = 'drain initiated'`); !got.Valid || got.String != "runner-west" {
		t.Errorf("(c) lifecycle row: runner_name = %v, want 'runner-west' (stripped from target)", got)
	}
	if got := nameOf(`actor = 'ssh-executor'`); got.Valid {
		t.Errorf("(d) ssh-executor run: runner_name = %q, want NULL — no runner was involved", got.String)
	}
	if got := nameOf(`target = 'runners'`); got.Valid {
		t.Errorf("(e) collection-targeted config: runner_name = %q, want NULL — 'runners' is not 'runner:<name>'", got.String)
	}
	if got := nameOf(`trace_id = 'run-killed'`); got.Valid {
		t.Errorf("(f) stopped run at 1100: runner_name = %q, want NULL — the operator's email does not match 'runner:%%'", got.String)
	}

	// 1110 widens the predicate from "a runner agent wrote this row" to "this
	// run executed on a runner", which is what runs.runner_id actually answers.
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1100→1110: %v", err)
	}
	if got := nameOf(`trace_id = 'run-killed'`); !got.Valid || got.String != "runner-east" {
		t.Errorf("(f) stopped run at 1110: runner_name = %v, want 'runner-east'", got)
	}
	// Widening must not reach rows that never had a runner.
	if got := nameOf(`actor = 'ssh-executor'`); got.Valid {
		t.Errorf("(d) ssh-executor run at 1110: runner_name = %q, want NULL — no runner_id to join", got.String)
	}
	if got := nameOf(`actor = 'runner:rn-gone'`); got.Valid {
		t.Errorf("(b) deregistered runner at 1110: runner_name = %q, want NULL", got.String)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1110→1100: %v", err)
	}

	// Down drops the column without losing the rows it was stamped on.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1100→1090: %v", err)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM activity`).Scan(&n)
	if n != 6 {
		t.Fatalf("after down: activity rows = %d, want 6 (rows lost dropping runner_name)", n)
	}
	// And re-deriving on the way back up is identical — the property the down
	// migration's header claims.
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up again 1090→1100: %v", err)
	}
	if got := nameOf(`actor = 'runner:rn-live'`); !got.Valid || got.String != "runner-east" {
		t.Errorf("after down/up: runner_name = %v, want 'runner-east' re-derived", got)
	}
}

// TestMigrate1150AppriseObjectForm pins DM-1's backfill: the apprise_targets
// column must hold exactly one shape afterwards, because from v1.5.43 both the
// settings card and the notification dispatcher read it through a single parser
// that understands only the object form. A row left in the pre-rich bare-string
// form would read as ZERO targets in both — invisible in the UI and silently
// undelivered — which is the bug this migration exists to prevent.
func TestMigrate1150AppriseObjectForm(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "apprise.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()

	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1140); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate to 1140: %v", err)
	}

	set := func(v string) {
		t.Helper()
		if _, err := pool.Exec(`INSERT INTO notification_config(id, apprise_targets, last_modified_by, last_modified_at)
		                        VALUES(1, ?, 'test', '2026-09-18T00:00:00Z')
		                        ON CONFLICT(id) DO UPDATE SET apprise_targets = excluded.apprise_targets`, v); err != nil {
			t.Fatalf("seed apprise_targets: %v", err)
		}
	}
	get := func() string {
		t.Helper()
		var s sql.NullString
		if err := pool.QueryRow(`SELECT apprise_targets FROM notification_config WHERE id = 1`).Scan(&s); err != nil {
			t.Fatalf("read apprise_targets: %v", err)
		}
		return s.String
	}

	// A MIXED row is the interesting fixture: a deployment that added a rich
	// target through the UI after having bare strings on file keeps both, and
	// the migration must convert only the strings.
	set(`["slack://legacy",{"label":"On-call","service":"email","url":"mailto://a@b.c","enabled":true},{"label":"Off","service":"slack","url":"slack://off","enabled":false}]`)

	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up 1140→1150: %v", err)
	}

	var elems []struct {
		Label   string `json:"label"`
		Service string `json:"service"`
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(get()), &elems); err != nil {
		t.Fatalf("after 1150 the column is not an array of objects: %v\ngot: %s", err, get())
	}
	if len(elems) != 3 {
		t.Fatalf("elements = %d, want 3 (the migration must not drop or add targets): %s", len(elems), get())
	}
	// The converted one: enabled, because a bare URL was always delivered to.
	if elems[0].URL != "slack://legacy" || !elems[0].Enabled {
		t.Errorf("converted legacy target = %+v, want url=slack://legacy enabled=true", elems[0])
	}
	if elems[0].Label != "" || elems[0].Service != "" {
		t.Errorf("converted legacy target invented metadata: %+v — label/service must stay empty", elems[0])
	}
	// The pass-through ones keep every field, INCLUDING enabled=false. A target
	// the operator switched off must not be switched back on by a backfill.
	if elems[1].Label != "On-call" || elems[1].Service != "email" || elems[1].URL != "mailto://a@b.c" || !elems[1].Enabled {
		t.Errorf("object target 1 = %+v, want it untouched", elems[1])
	}
	if elems[2].Enabled {
		t.Errorf("object target 2 = %+v, want enabled=false preserved", elems[2])
	}

	// Down is a no-op by design (the object form is readable by every binary
	// that ever read this column), and stepping back up must not double-convert.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down 1150→1140: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up again 1140→1150: %v", err)
	}
	after := get()
	var again []map[string]any
	if err := json.Unmarshal([]byte(after), &again); err != nil || len(again) != 3 {
		t.Fatalf("re-running 1150 over the object form corrupted it: %v\ngot: %s", err, after)
	}

	// An already-converted row is left byte-identical: the EXISTS guard means a
	// second run is not even a rewrite. (This is what makes the migration safe
	// to re-run on a restored backup.)
	set(`[{"label":"","service":"","url":"slack://x","enabled":true}]`)
	before := get()
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if got := get(); got != before {
		t.Errorf("object-form row was rewritten: got %s, want %s unchanged", got, before)
	}

	// A column that is not valid JSON is left ALONE rather than replaced by
	// NULL — the guard exists so a corrupt row stays diagnosable.
	set(`not json`)
	if err := m.Steps(-1); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if got := get(); got != "not json" {
		t.Errorf("invalid JSON column = %q, want it left untouched", got)
	}
}
