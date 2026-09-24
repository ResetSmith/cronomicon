package db

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// R2-2 (the rbac2 plan) — the nine delete-cascade triggers now key on
// the definition uid.
//
// These triggers are the highest-consequence part of AF-4b. The 940 migration's
// header says why: a cascade that stops working does not raise an error, it
// leaves orphan rows that the next definition of the same name silently
// inherits — a paused state, a schedule entry or a reaction belonging to
// something deleted months ago. Every trigger is exercised here in both of its
// arms, because the whole design of the re-key is that neither arm may be the
// only one that works.

func openCascadePool(t *testing.T) *sql.DB {
	t.Helper()
	pool, err := Open(filepath.Join(t.TempDir(), "cascade.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := Migrate(pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// seedCascadeFixture creates a job and a workflow, each with one row in every
// satellite table that cascades off it. withUID controls whether the satellite
// rows carry owner_uid — the two arms of every trigger.
func seedCascadeFixture(t *testing.T, pool *sql.DB, withUID bool) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('doomed','git','uid-job','bash','t')`)
	exec(`INSERT INTO workflows(name, source, uid, steps, synced_at) VALUES('doomed-wf','git','uid-wf','[]','t')`)

	jobUID, wfUID := any(nil), any(nil)
	if withUID {
		jobUID, wfUID = "uid-job", "uid-wf"
	}

	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, owner_uid)
	      VALUES('git','job','doomed','default','* * * * *',?)`, jobUID)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, owner_uid)
	      VALUES('git','workflow','doomed-wf','default','* * * * *',?)`, wfUID)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES('git','job','doomed','t','t',?)`, jobUID)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES('git','workflow','doomed-wf','t','t',?)`, wfUID)
	exec(`INSERT INTO pending_runs(id, kind, name, source, run_at, scheduled_by, created_at, status, owner_uid)
	      VALUES('pr-job','job','doomed','git','t','t','t','pending',?)`, jobUID)
	exec(`INSERT INTO pending_runs(id, kind, name, source, run_at, scheduled_by, created_at, status, owner_uid)
	      VALUES('pr-wf','workflow','doomed-wf','git','t','t','t','pending',?)`, wfUID)
	exec(`INSERT INTO reference_bindings(owner_kind, owner_source, owner_name, ref_kind, ref_name, created_by, created_at, owner_uid)
	      VALUES('job','git','doomed','secret','DB_PASSWORD','t','t',?)`, jobUID)
	exec(`INSERT INTO reactions(owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome, position, owner_uid)
	      VALUES('git','job','doomed','r1','git','job','other','success',0,?)`, jobUID)
	exec(`INSERT INTO reactions(owner_source, owner_kind, owner_name, name, on_source, on_kind, on_name, on_outcome, position, owner_uid)
	      VALUES('git','workflow','doomed-wf','r2','git','job','other','success',0,?)`, wfUID)
}

func cascadeCount(t *testing.T, pool *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("count: %v\n%s", err, query)
	}
	return n
}

// satelliteCounts totals every row that should die with the two definitions.
func satelliteCounts(t *testing.T, pool *sql.DB) int {
	t.Helper()
	return cascadeCount(t, pool, `SELECT
		 (SELECT COUNT(*) FROM definition_schedules)
		+(SELECT COUNT(*) FROM paused_jobs)
		+(SELECT COUNT(*) FROM pending_runs)
		+(SELECT COUNT(*) FROM reference_bindings)
		+(SELECT COUNT(*) FROM reactions)`)
}

// TestCascadeTriggersDeleteByUID is the primary arm: satellites carrying the
// owner's uid are cascaded when the definition is deleted.
func TestCascadeTriggersDeleteByUID(t *testing.T) {
	pool := openCascadePool(t)
	seedCascadeFixture(t, pool, true)

	// 2 schedule entries + 2 pauses + 2 parked runs + 1 binding + 2 reactions.
	if got := satelliteCounts(t, pool); got != 9 {
		t.Fatalf("fixture seeded %d satellite rows, want 9", got)
	}
	if _, err := pool.Exec(`DELETE FROM jobs WHERE name='doomed'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if _, err := pool.Exec(`DELETE FROM workflows WHERE name='doomed-wf'`); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	if got := satelliteCounts(t, pool); got != 0 {
		t.Errorf("%d satellite rows survived the cascade, want 0 — a trigger stopped cascading", got)
	}
}

// TestCascadeTriggersDeleteNullUIDByName pins the 1020–1040 window's fallback
// arm, so it runs at schema 1040: a satellite row missing its owner_uid was
// still cascaded by name WHILE NAMES WERE UNIQUE. 1050 removes that arm — see
// TestCascade1050NameArmGone for the successor contract.
func TestCascadeTriggersDeleteNullUIDByName(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "arm.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1040); err != nil {
		t.Fatalf("migrate to 1040: %v", err)
	}
	seedCascadeFixture(t, pool, false)

	if _, err := pool.Exec(`DELETE FROM jobs WHERE name='doomed'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if _, err := pool.Exec(`DELETE FROM workflows WHERE name='doomed-wf'`); err != nil {
		t.Fatalf("delete workflow: %v", err)
	}
	if got := satelliteCounts(t, pool); got != 0 {
		t.Errorf("%d NULL-uid satellite rows survived, want 0 — the pre-1050 name fallback arm is not working", got)
	}
}

// TestCascade1050NameArmGone pins the 1050 contract change, in both directions:
// a NULL-uid satellite is NO LONGER cascaded (it could belong to a same-named
// sibling now, so deleting it would be cross-identity collateral), and — the
// half that matters — a sibling's uid-stamped rows survive a same-named
// delete. The first half documents the intentional loss; every real writer has
// stamped the uid since R2-2, so nothing production-written is in that state.
func TestCascade1050NameArmGone(t *testing.T) {
	pool := openCascadePool(t)
	seedCascadeFixture(t, pool, false) // NULL-uid satellites, post-1050 schema

	if _, err := pool.Exec(`DELETE FROM jobs WHERE name='doomed'`); err != nil {
		t.Fatalf("delete job: %v", err)
	}
	if got := cascadeCount(t, pool,
		`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='doomed' AND owner_uid IS NULL`); got != 1 {
		t.Errorf("NULL-uid schedule entry rows = %d, want 1 — 1050's triggers must not delete by name", got)
	}
}

// TestCascadeTriggersSpareOtherDefinitions is the invariant per-agency naming
// will depend on: deleting one definition must not touch a same-named sibling's
// rows. Today the sibling lives in the other SOURCE pool (already legal); after
// R2-5 it will be another agency, and this is the assertion that carries over.
func TestCascadeTriggersSpareOtherDefinitions(t *testing.T) {
	pool := openCascadePool(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	// Same NAME, two source pools, each with its own schedule entry and pause.
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('twin','git','uid-git','bash','t')`)
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('twin','amadeus','uid-ama','bash','t')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, owner_uid)
	      VALUES('git','job','twin','default','* * * * *','uid-git')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, owner_uid)
	      VALUES('amadeus','job','twin','default','* * * * *','uid-ama')`)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES('git','job','twin','t','t','uid-git')`)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at, owner_uid)
	      VALUES('amadeus','job','twin','t','t','uid-ama')`)

	if _, err := pool.Exec(`DELETE FROM jobs WHERE name='twin' AND source='git'`); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if n := cascadeCount(t, pool, `SELECT COUNT(*) FROM definition_schedules WHERE owner_uid='uid-ama'`); n != 1 {
		t.Errorf("the surviving twin's schedule entry count = %d, want 1 — the cascade reached across identities", n)
	}
	if n := cascadeCount(t, pool, `SELECT COUNT(*) FROM paused_jobs WHERE owner_uid='uid-ama'`); n != 1 {
		t.Errorf("the surviving twin's pause count = %d, want 1 — the cascade reached across identities", n)
	}
	if n := cascadeCount(t, pool, `SELECT COUNT(*) FROM definition_schedules WHERE owner_uid='uid-git'`); n != 0 {
		t.Errorf("the deleted twin left %d schedule entries, want 0", n)
	}
}

// TestMigrate1020Backfill proves the satellite backfill with data present, and
// that the down-migration restores name-keyed triggers that still cascade —
// the ordering hazard called out in the down file's header.
func TestMigrate1020Backfill(t *testing.T) {
	pool, err := Open(filepath.Join(t.TempDir(), "sat.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer pool.Close()
	m, err := migrator(pool)
	if err != nil {
		t.Fatalf("migrator: %v", err)
	}
	if err := m.Migrate(1010); err != nil {
		t.Fatalf("migrate to 1010: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	exec(`INSERT INTO jobs(name, source, uid, run_type, synced_at) VALUES('bf','git','uid-bf','bash','t')`)
	exec(`INSERT INTO schedules(name, source, uid, cron, content_hash) VALUES('nightly','amadeus','uid-sched','0 2 * * *','h')`)
	exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, source_ref)
	      VALUES('git','job','bf','nightly','0 2 * * *','nightly')`)
	exec(`INSERT INTO paused_jobs(source, owner_kind, name, paused_by, paused_at) VALUES('git','job','bf','t','t')`)
	exec(`INSERT INTO entity_codes(kind, source, name, created_at) VALUES('job','git','bf','t')`)

	if err := m.Migrate(1020); err != nil {
		t.Fatalf("migrate to 1020: %v", err)
	}

	uid := func(q string) sql.NullString {
		t.Helper()
		var u sql.NullString
		if err := pool.QueryRow(q).Scan(&u); err != nil {
			t.Fatalf("read: %v\n%s", err, q)
		}
		return u
	}
	if got := uid(`SELECT owner_uid FROM definition_schedules`); got.String != "uid-bf" {
		t.Errorf("definition_schedules.owner_uid = %v, want uid-bf", got)
	}
	// source_ref names a schedule with no source; the unique-name rule resolves it.
	if got := uid(`SELECT schedule_uid FROM definition_schedules`); got.String != "uid-sched" {
		t.Errorf("definition_schedules.schedule_uid = %v, want uid-sched", got)
	}
	if got := uid(`SELECT owner_uid FROM paused_jobs`); got.String != "uid-bf" {
		t.Errorf("paused_jobs.owner_uid = %v, want uid-bf", got)
	}
	if got := uid(`SELECT uid FROM entity_codes`); got.String != "uid-bf" {
		t.Errorf("entity_codes.uid = %v, want uid-bf", got)
	}

	// Down: the restored name-keyed triggers must still cascade, which is what
	// proves they were recreated BEFORE the columns they referenced were dropped.
	if err := m.Migrate(1010); err != nil {
		t.Fatalf("migrate down to 1010: %v", err)
	}
	if _, err := pool.Exec(`DELETE FROM jobs WHERE name='bf'`); err != nil {
		t.Fatalf("delete after down-migration: %v", err)
	}
	if n := cascadeCount(t, pool, `SELECT COUNT(*) FROM definition_schedules`); n != 0 {
		t.Errorf("after rollback the cascade left %d schedule entries, want 0 — "+
			"the down-migration's triggers reference dropped columns", n)
	}
}
