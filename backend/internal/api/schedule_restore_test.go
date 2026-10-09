package api_test

import (
	"net/http"
	"testing"
)

// Two agencies may each own an in-app job of one name, and both may be bound
// to one schedule. Force-deleting the schedule detaches both; restoring it
// binds BOTH again, each to its own job. The tombstone names each owner by uid
// (since 2.4.0), because the name alone does not say whose binding it was.
func TestGR1_ARestoreRebindsEachOfTwoSameNamedJobs(t *testing.T) {
	a := newGRSchedAPI(t)
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "shared", "cron": "0 0 2 * * *"}, http.StatusCreated)
	sched := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='shared'`)
	// Two in-app jobs called `nightly`, as two agencies' composers leave them:
	// each with its own uid and its own entry tied to the schedule.
	for _, uid := range []string{"j-fin", "j-tax"} {
		a.exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES(?, 'nightly', 'cronomicon', 'bash', 'echo hi', 't')`, uid)
		a.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
		        VALUES('cronomicon', 'job', 'nightly', 'shared', '0 0 2 * * *', 0, 'shared', ?, ?)`, uid, sched)
	}
	bound := func(uid string) int {
		return a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_uid=? AND schedule_uid=? AND name='shared'`, uid, sched)
	}

	a.must(http.MethodDelete, "/api/v1/schedule-defs/shared?force=true", nil, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE name='shared'`); n != 0 {
		t.Fatalf("the force-delete left %d entries", n)
	}
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/shared/restore", nil, http.StatusOK, http.StatusNoContent)
	for _, uid := range []string{"j-fin", "j-tax"} {
		if n := bound(uid); n != 1 {
			t.Errorf("after the restore the job %s has %d entries tied to the schedule, want its own one back", uid, n)
		}
	}
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE name='shared'`); n != 2 {
		t.Errorf("entries after the restore = %d, want one per job", n)
	}
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE name='shared' AND owner_uid IS NULL`); n != 0 {
		t.Errorf("%d restored entries have no owner uid", n)
	}
}

// A tombstone written before 2.4.0 names its owners by NAME only. A restore
// from one still binds: to the definition's uid when the name is one
// definition, and under the name, as it always did, when two definitions hold
// it. It never skips a binding for being ambiguous, because a skipped binding
// is a job that silently stops firing.
func TestGR1_ARestoreFromAnOlderTombstoneStillBinds(t *testing.T) {
	a := newGRSchedAPI(t)
	// stripOwnerUIDs rewrites the schedule's delete tombstone into the shape a
	// 2.3 server wrote: the same bindings, without ownerUid.
	stripOwnerUIDs := func(name string) {
		a.exec(`UPDATE definition_revisions
		           SET snapshot_json = json_set(snapshot_json, '$.detachedBindings',
		                 (SELECT json_group_array(json(json_remove(value, '$.ownerUid')))
		                    FROM json_each(definition_revisions.snapshot_json, '$.detachedBindings')))
		         WHERE kind='schedule' AND name=? AND json_extract(snapshot_json, '$.detachedBindings') IS NOT NULL`, name)
		if n := a.count(`SELECT COUNT(*) FROM definition_revisions WHERE kind='schedule' AND name=? AND snapshot_json LIKE '%ownerUid%'`, name); n != 0 {
			t.Fatalf("the tombstone of %s still carries an ownerUid", name)
		}
		if n := a.count(`SELECT COUNT(*) FROM definition_revisions WHERE kind='schedule' AND name=? AND snapshot_json LIKE '%detachedBindings%' AND snapshot_json LIKE '%ownerName%'`, name); n != 1 {
			t.Fatalf("the tombstone of %s lost its bindings in the rewrite (rows with bindings: %d)", name, n)
		}
	}
	entryOf := func(sched, ownerUID string) int {
		return a.count(`SELECT COUNT(*) FROM definition_schedules WHERE name=? AND owner_uid=?`, sched, ownerUID)
	}

	// One definition of the name: bound by its uid.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "one", "cron": "0 0 2 * * *"}, http.StatusCreated)
	a.must(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "solo-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"one"}}, http.StatusCreated)
	solo := a.str(`SELECT uid FROM jobs WHERE source='cronomicon' AND name='solo-job'`)
	a.must(http.MethodDelete, "/api/v1/schedule-defs/one?force=true", nil, http.StatusNoContent)
	stripOwnerUIDs("one")
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/one/restore", nil, http.StatusOK, http.StatusNoContent)
	if n := entryOf("one", solo); n != 1 {
		t.Errorf("restored from an older tombstone, the job has %d entries under its uid, want 1", n)
	}

	// Two definitions of the name: bound under the name, not dropped.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "two", "cron": "0 0 3 * * *"}, http.StatusCreated)
	sched := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='two'`)
	for _, uid := range []string{"j-fin", "j-tax"} {
		a.exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES(?, 'twin', 'cronomicon', 'bash', 'echo hi', 't')`, uid)
		a.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
		        VALUES('cronomicon', 'job', 'twin', 'two', '0 0 3 * * *', 0, 'two', ?, ?)`, uid, sched)
	}
	a.must(http.MethodDelete, "/api/v1/schedule-defs/two?force=true", nil, http.StatusNoContent)
	stripOwnerUIDs("two")
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/two/restore", nil, http.StatusOK, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE name='two' AND owner_name='twin'`); n != 1 {
		t.Errorf("restored from an older tombstone with two same-named jobs: %d entries under the name, want the binding put back (once), not dropped", n)
	}
	if got := a.str(`SELECT COALESCE(schedule_uid,'') FROM definition_schedules WHERE name='two' AND owner_name='twin'`); got != sched {
		t.Errorf("the entry put back under the name is tied to schedule %q, want %q", got, sched)
	}
}
