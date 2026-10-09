package api_test

// Phase R0 of 2.4.0 read these four; these tests reproduce them, before the
// schedules half of Phase R1 changes anything. Each passes on the code as it
// is and is inverted by that phase. No production code changes.
//
// They share one cause. A definition (a job or a workflow) that takes its
// timing from a reusable schedule holds a COPY of it in definition_schedules,
// with the schedule's NAME in source_ref. A Git schedule and an in-app
// schedule may share a name. Every statement that goes from a schedule to the
// definitions bound to it, or from a name to a schedule, goes by the bare
// name.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

type grSchedAPI struct {
	t      *testing.T
	ts     *httptest.Server
	pool   *sql.DB
	client *http.Client
	csrf   string
}

func newGRSchedAPI(t *testing.T) *grSchedAPI {
	t.Helper()
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	a := &grSchedAPI{t: t, ts: ts, pool: pool, client: client, csrf: csrf}
	a.exec(`INSERT INTO scripts(name, run_type, command, content_hash, synced_at) VALUES('backup-db','bash','pg_dump','sha256:a','t')`)
	return a
}

func (a *grSchedAPI) exec(q string, args ...any) {
	a.t.Helper()
	if _, err := a.pool.ExecContext(context.Background(), q, args...); err != nil {
		a.t.Fatalf("seed: %v\n%s", err, q)
	}
}

func (a *grSchedAPI) str(q string, args ...any) string {
	a.t.Helper()
	var s sql.NullString
	if err := a.pool.QueryRowContext(context.Background(), q, args...).Scan(&s); err != nil {
		a.t.Fatalf("query: %v\n%s", err, q)
	}
	return s.String
}

func (a *grSchedAPI) count(q string, args ...any) int {
	a.t.Helper()
	var n int
	if err := a.pool.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		a.t.Fatalf("query: %v\n%s", err, q)
	}
	return n
}

func (a *grSchedAPI) do(method, path string, body any) (int, []byte) {
	a.t.Helper()
	var rdr io.Reader = bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.ts.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("X-CSRF-Token", a.csrf)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// must runs a request and fails the test unless it answers one of want.
func (a *grSchedAPI) must(method, path string, body any, want ...int) []byte {
	a.t.Helper()
	code, raw := a.do(method, path, body)
	for _, w := range want {
		if code == w {
			return raw
		}
	}
	a.t.Fatalf("%s %s = %d, want %v: %s", method, path, code, want, raw)
	return nil
}

// gitSchedule writes a Git schedule and a Git job bound to it, as a sync
// leaves them: the job's entry is a copy of the schedule, with its name in
// source_ref and (the name being unique at that moment) its uid.
func (a *grSchedAPI) gitScheduleWithJob(schedule, cron, job string) {
	a.t.Helper()
	a.exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, synced_at) VALUES(?, ?, 'git', ?, 'h', 't')`, "s-git-"+schedule, schedule, cron)
	a.exec(`INSERT INTO jobs(uid, name, source, run_type, command, synced_at) VALUES(?, ?, 'git', 'bash', 'echo hi', 't')`, "j-"+job, job)
	a.exec(`INSERT INTO definition_schedules(owner_source, owner_kind, owner_name, name, cron, position, source_ref, owner_uid, schedule_uid)
	        VALUES('git', 'job', ?, ?, ?, 0, ?, ?, ?)`, job, schedule, cron, schedule, "j-"+job, "s-git-"+schedule)
}

// Present defect 12 (H12 of the plan) and 15. An in-app schedule is created
// with the name of a Git schedule that a Git job is bound to. From then on the
// in-app schedule counts that job as its user, refuses to be deleted because
// of it, rewrites that job's timing when it is edited, and removes that job's
// timing when it is force-deleted. Phase R1 (GR-8) inverts all four: the
// in-app schedule has nothing to do with a definition bound to Git's.
func TestGR0_AnInAppScheduleActsOnTheDefinitionsOfGitsScheduleOfThatName(t *testing.T) {
	a := newGRSchedAPI(t)
	a.gitScheduleWithJob("nightly", "0 0 1 * * *", "git-job")
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "nightly", "cron": "0 0 3 * * *"}, http.StatusCreated)
	entry := func() (cron string, n int) {
		n = a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source='git' AND owner_name='git-job'`)
		if n > 0 {
			cron = a.str(`SELECT cron FROM definition_schedules WHERE owner_source='git' AND owner_name='git-job'`)
		}
		return
	}

	// 15: the in-app schedule, which nothing is bound to, reports the Git job.
	raw := a.must(http.MethodGet, "/api/v1/schedule-defs/nightly?source=cronomicon", nil, http.StatusOK)
	var def struct {
		UsedByCount int `json:"usedByCount"`
	}
	_ = json.Unmarshal(raw, &def)
	if def.UsedByCount != 1 {
		t.Errorf("the in-app schedule's usedByCount = %d: today it is expected to count the Git schedule's job (1)", def.UsedByCount)
	}
	// And refuses a plain delete on account of it.
	if code, raw := a.do(http.MethodDelete, "/api/v1/schedule-defs/nightly", nil); code != http.StatusConflict {
		t.Errorf("deleting the unused in-app schedule = %d (%s): today it is expected to be refused (409) because of the Git schedule's job", code, raw)
	}

	// 12: editing the in-app schedule rewrites the Git job's timing.
	a.must(http.MethodPut, "/api/v1/schedule-defs/nightly", map[string]any{"cron": "0 0 5 * * *"}, http.StatusOK)
	if cron, _ := entry(); cron != "0 0 5 * * *" {
		t.Errorf("after editing the IN-APP schedule the Git job's entry is %q: today it is expected to have been rewritten to the in-app schedule's '0 0 5 * * *'", cron)
	}
	if got := a.str(`SELECT cron FROM schedules WHERE source='git' AND name='nightly'`); got != "0 0 1 * * *" {
		t.Errorf("the Git schedule itself was changed: %q", got)
	}

	// 12: force-deleting the in-app schedule removes the Git job's timing.
	a.must(http.MethodDelete, "/api/v1/schedule-defs/nightly?force=true", nil, http.StatusNoContent)
	if _, n := entry(); n != 0 {
		t.Errorf("after force-deleting the IN-APP schedule the Git job still has %d entries: today its entry is expected to be gone", n)
	}
}

// Present defect 14. A job that ticks `weekly` is bound to whichever schedule
// of that name sorts first by source, and a schedule in the recycle bin is not
// left out: with the in-app `weekly` binned and Git's `weekly` live, the job
// is expanded from the binned one. Phase R1 inverts it: a binned schedule is
// never bound.
func TestGR0_TheComposerBindsAJobToABinnedSchedule(t *testing.T) {
	a := newGRSchedAPI(t)
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "weekly", "cron": "0 0 2 * * 1"}, http.StatusCreated)
	a.must(http.MethodDelete, "/api/v1/schedule-defs/weekly", nil, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM schedules WHERE source='cronomicon' AND name='weekly' AND deleted_at IS NOT NULL`); n != 1 {
		t.Fatalf("the in-app schedule is not in the recycle bin (count %d)", n)
	}
	a.exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, synced_at) VALUES('s-git-weekly', 'weekly', 'git', '0 0 7 * * 1', 'h', 't')`)

	a.must(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "weekly-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"weekly"}}, http.StatusCreated)
	switch got := a.str(`SELECT cron FROM definition_schedules WHERE owner_name='weekly-job' AND name='weekly'`); got {
	case "0 0 2 * * 1":
		// Today: the binned in-app schedule's timing.
	case "0 0 7 * * 1":
		t.Errorf("the job was bound to the live Git schedule: this is fixed, and the test is to be inverted")
	default:
		t.Errorf("the job's entry is %q: neither schedule's timing", got)
	}
}

// Present defect 7. A schedule that is force-deleted remembers the definitions
// it was detached from, and restoring it from the recycle bin binds them
// again. The rows it writes back carry neither the owner's uid nor the
// schedule's, so nothing that keys on a uid sees them: saving the owning job
// again does not replace its entry, it adds a second one of the same name.
// Phase R1 inverts it: a restored binding is a binding like any other.
func TestGR0_ARestoredScheduleLeavesBindingsThatASaveDuplicates(t *testing.T) {
	a := newGRSchedAPI(t)
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "rs", "cron": "0 0 2 * * *"}, http.StatusCreated)
	job := map[string]any{"name": "rs-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"rs"}}
	a.must(http.MethodPost, "/api/v1/jobs", job, http.StatusCreated)
	entries := func() int {
		return a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon' AND owner_name='rs-job' AND name='rs'`)
	}
	if n := entries(); n != 1 {
		t.Fatalf("the bound job has %d entries, want 1", n)
	}

	a.must(http.MethodDelete, "/api/v1/schedule-defs/rs?force=true", nil, http.StatusNoContent)
	if n := entries(); n != 0 {
		t.Fatalf("the force-delete left %d entries, want the job detached", n)
	}
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/rs/restore", nil, http.StatusOK, http.StatusNoContent)
	if n := entries(); n != 1 {
		t.Fatalf("the restore put back %d entries, want 1", n)
	}
	// The restored row has neither uid.
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='rs-job' AND name='rs' AND owner_uid IS NULL AND schedule_uid IS NULL`); n != 1 {
		t.Errorf("the restored entry carries a uid (rows with neither: %d): today it is expected to carry none", n)
	}

	// The job is saved again, unchanged.
	id := a.count(`SELECT rowid FROM jobs WHERE source='cronomicon' AND name='rs-job'`)
	a.must(http.MethodPut, "/api/v1/jobs/"+strconv.Itoa(id), job, http.StatusOK)
	switch n := entries(); n {
	case 2:
		// Today: the save did not see the restored row, and added its own.
	case 1:
		t.Errorf("saving the job left one entry: this is fixed, and the test is to be inverted")
	default:
		t.Errorf("saving the job left %d entries", n)
	}
}
