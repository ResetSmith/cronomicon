package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// An entry that names a schedule and is tied to none (what migration 1300
// leaves under a name two schedules hold, and reports in the Notices inbox) is
// nobody's: an edit of either schedule does not reach it, and neither a delete
// nor a purge of either removes it. Until 2.4.0 the purge had an arm that took
// every entry with no schedule uid and the purged schedule's NAME, which would
// have removed the timing of a job that may be bound to the other schedule.
func TestGR1_APurgeLeavesAnEntryTiedToNoSchedule(t *testing.T) {
	a := newGRSchedAPI(t)
	a.gitScheduleWithJob("nightly", "0 0 1 * * *", "git-job")
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "nightly", "cron": "0 0 3 * * *"}, http.StatusCreated)
	a.exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES('j-amb', 'amb-job', 'cronomicon', 'bash', 'echo hi', 't')`)
	a.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	        VALUES('cronomicon', 'job', 'amb-job', 'nightly', '0 0 1 * * *', 0, 'nightly', 'j-amb', NULL)`)
	entry := func() (string, int) {
		n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_uid='j-amb'`)
		if n == 0 {
			return "", 0
		}
		return a.str(`SELECT cron FROM definition_schedules WHERE owner_uid='j-amb'`), n
	}

	a.must(http.MethodPut, "/api/v1/schedule-defs/nightly", map[string]any{"cron": "0 0 5 * * *"}, http.StatusOK)
	if cron, n := entry(); n != 1 || cron != "0 0 1 * * *" {
		t.Errorf("an edit of the in-app schedule reached an entry tied to no schedule: %d entries at %q", n, cron)
	}
	// It is not the in-app schedule's user, so the delete needs no force.
	a.must(http.MethodDelete, "/api/v1/schedule-defs/nightly", nil, http.StatusNoContent)
	if _, n := entry(); n != 1 {
		t.Errorf("deleting the in-app schedule removed an entry tied to no schedule (%d left)", n)
	}
	a.must(http.MethodDelete, "/api/v1/recycle-bin/schedule/nightly", nil, http.StatusOK, http.StatusNoContent)
	if cron, n := entry(); n != 1 || cron != "0 0 1 * * *" {
		t.Errorf("purging the in-app schedule removed or changed an entry tied to no schedule: %d entries at %q", n, cron)
	}
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='git-job'`); n != 1 {
		t.Errorf("the Git job has %d entries after the purge, want its own one", n)
	}
}

// A workflow is bound to a reusable schedule exactly as a job is: its entry
// carries the workflow's uid and the schedule's, the schedule counts it among
// its users, an edit reaches it, a forced delete detaches it and a restore
// binds it again. And a Git workflow bound to Git's schedule of the same name
// is left alone throughout.
func TestGR1_AWorkflowIsBoundToItsScheduleByUID(t *testing.T) {
	a := newGRSchedAPI(t)
	a.exec(`INSERT INTO jobs(name, source, run_type, command, content_hash, source_path, synced_at)
	        VALUES('build','git','bash','make','sha256:a','jobs/build.yaml','t')`)
	// Git's `cadence`, with a Git workflow bound to it.
	a.exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, synced_at) VALUES('s-git-cadence', 'cadence', 'git', '0 0 1 * * *', 'h', 't')`)
	a.exec(`INSERT INTO workflows(uid, name, source, steps, synced_at) VALUES('w-git', 'gitflow', 'git', '[]', 't')`)
	a.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	        VALUES('git', 'workflow', 'gitflow', 'cadence', '0 0 1 * * *', 0, 'cadence', 'w-git', 's-git-cadence')`)
	gitEntry := func(when string) {
		t.Helper()
		if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_uid='w-git' AND cron='0 0 1 * * *' AND schedule_uid='s-git-cadence'`); n != 1 {
			t.Errorf("%s, the Git workflow has %d entries as it had them, want 1", when, n)
		}
	}

	// The in-app `cadence`, and an in-app workflow that ticks it.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "cadence", "cron": "0 0 3 * * *"}, http.StatusCreated)
	sched := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='cadence'`)
	a.must(http.MethodPost, "/api/v1/workflows", map[string]any{
		"name": "flow", "steps": []map[string]any{{"type": "job", "name": "build"}}, "scheduleRefs": []string{"cadence"},
	}, http.StatusCreated)
	wf := a.str(`SELECT uid FROM workflows WHERE source='cronomicon' AND name='flow'`)
	entry := func() (cron, ownerUID, schedUID string, n int) {
		n = a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_kind='workflow' AND owner_source='cronomicon' AND owner_name='flow'`)
		if n == 1 {
			q := ` FROM definition_schedules WHERE owner_kind='workflow' AND owner_source='cronomicon' AND owner_name='flow'`
			cron = a.str(`SELECT cron` + q)
			ownerUID = a.str(`SELECT COALESCE(owner_uid,'')` + q)
			schedUID = a.str(`SELECT COALESCE(schedule_uid,'')` + q)
		}
		return
	}
	if cron, owner, s, n := entry(); n != 1 || cron != "0 0 3 * * *" || owner != wf || s != sched {
		t.Fatalf("the composed workflow's entry: %d rows, cron %q, owner %q, schedule %q; want one, at the in-app schedule's timing, with the workflow's uid %q and the schedule's %q",
			n, cron, owner, s, wf, sched)
	}

	raw := a.must(http.MethodGet, "/api/v1/schedule-defs/cadence?source=cronomicon", nil, http.StatusOK)
	var def struct {
		UsedByCount int `json:"usedByCount"`
		UsedBy      []struct {
			Name string `json:"name"`
		} `json:"usedBy"`
	}
	_ = json.Unmarshal(raw, &def)
	if def.UsedByCount != 1 || len(def.UsedBy) != 1 || def.UsedBy[0].Name != "flow" {
		t.Errorf("the in-app schedule is used by %v (count %d), want the workflow flow alone", def.UsedBy, def.UsedByCount)
	}

	a.must(http.MethodPut, "/api/v1/schedule-defs/cadence", map[string]any{"cron": "0 0 5 * * *"}, http.StatusOK)
	if cron, _, _, _ := entry(); cron != "0 0 5 * * *" {
		t.Errorf("the workflow's entry is %q after its schedule was edited, want '0 0 5 * * *'", cron)
	}
	gitEntry("after the in-app schedule was edited")

	a.must(http.MethodDelete, "/api/v1/schedule-defs/cadence?force=true", nil, http.StatusNoContent)
	if _, _, _, n := entry(); n != 0 {
		t.Errorf("the force-delete left the workflow bound (%d entries)", n)
	}
	gitEntry("after the in-app schedule was force-deleted")

	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/cadence/restore", nil, http.StatusOK, http.StatusNoContent)
	if cron, owner, s, n := entry(); n != 1 || cron != "0 0 5 * * *" || owner != wf || s != sched {
		t.Errorf("after the restore the workflow's entry: %d rows, cron %q, owner %q, schedule %q; want its one binding back with both uids",
			n, cron, owner, s)
	}
	gitEntry("after the in-app schedule was restored")
}

// A restore replays the tombstone of the schedule being restored, not the
// newest-looking tombstone under its name. A name outlives a schedule: one that
// was purged leaves its revisions behind, and revision numbers count per
// schedule, so an old schedule with a long history outranks a new one of the
// same name. Read by name, restoring the new schedule bound the OLD one's users
// to it.
func TestGR1_ARestoreReplaysItsOwnTombstone(t *testing.T) {
	a := newGRSchedAPI(t)
	// The first `rota`: a user, some history, then force-deleted and purged.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "rota", "cron": "0 0 2 * * *"}, http.StatusCreated)
	a.must(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "rota-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"rota"}}, http.StatusCreated)
	first := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='rota'`)
	for _, cron := range []string{"0 0 3 * * *", "0 0 4 * * *", "0 0 5 * * *"} {
		a.must(http.MethodPut, "/api/v1/schedule-defs/rota", map[string]any{"cron": cron}, http.StatusOK)
	}
	a.must(http.MethodDelete, "/api/v1/schedule-defs/rota?force=true", nil, http.StatusNoContent)
	a.must(http.MethodDelete, "/api/v1/recycle-bin/schedule/rota", nil, http.StatusOK, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM definition_revisions WHERE kind='schedule' AND uid=? AND action='deleted' AND snapshot_json LIKE '%rota-job%'`, first); n != 1 {
		t.Fatalf("the purged schedule's tombstone naming rota-job: %d rows, want it kept (this test stands on it)", n)
	}

	// A second `rota`, nobody's yet: binned and restored.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "rota", "cron": "0 0 9 * * *"}, http.StatusCreated)
	second := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='rota'`)
	if second == first {
		t.Fatalf("the second schedule of the name has the first one's uid")
	}
	a.must(http.MethodDelete, "/api/v1/schedule-defs/rota", nil, http.StatusNoContent)
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/rota/restore", nil, http.StatusOK, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='rota-job'`); n != 0 {
		t.Errorf("restoring the second schedule bound the first one's job to it (%d entries): a job nobody bound to this schedule now fires on it", n)
	}
}
