package api_test

// Phase R0 of 2.4.0 read three defects of reusable schedules, and these tests
// reproduced them (as TestGR0_…) before the schedules half of Phase R1 changed
// anything. That half fixed them, and each test now says what is true instead:
// TestGR1_…, with what it used to pin in its comment.
//
// They shared one cause. A definition (a job or a workflow) that takes its
// timing from a reusable schedule holds a COPY of it in definition_schedules,
// with the schedule's NAME in source_ref. A Git schedule and an in-app
// schedule may share a name. Every statement that went from a schedule to the
// definitions bound to it, or from a name to a schedule, went by the bare
// name. Since migration 1300 an entry carries the uid of the schedule it was
// expanded from, and those statements key on it.

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

// An in-app schedule has nothing to do with the definitions bound to Git's
// schedule of the same name.
//
// Until Phase R1 (present defects 12 and 15; H12 of the plan) it did: it
// counted Git's schedule's job as its user, refused to be deleted because of
// it, rewrote that job's timing when it was edited, and removed that job's
// timing when it was force-deleted.
func TestGR1_AnInAppScheduleLeavesGitsScheduleOfThatNameAlone(t *testing.T) {
	a := newGRSchedAPI(t)
	a.gitScheduleWithJob("nightly", "0 0 1 * * *", "git-job")
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "nightly", "cron": "0 0 3 * * *"}, http.StatusCreated)
	// An in-app job bound to the IN-APP schedule: the composer takes the in-app
	// schedule of a name before Git's.
	a.must(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "app-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"nightly"}}, http.StatusCreated)
	entryOf := func(source, job string) (cron string, n int) {
		n = a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source=? AND owner_name=?`, source, job)
		if n > 0 {
			cron = a.str(`SELECT cron FROM definition_schedules WHERE owner_source=? AND owner_name=?`, source, job)
		}
		return
	}
	usedBy := func(source string) (int, []string) {
		raw := a.must(http.MethodGet, "/api/v1/schedule-defs/nightly?source="+source, nil, http.StatusOK)
		var def struct {
			UsedByCount int `json:"usedByCount"`
			UsedBy      []struct {
				Name string `json:"name"`
			} `json:"usedBy"`
		}
		_ = json.Unmarshal(raw, &def)
		names := []string{}
		for _, u := range def.UsedBy {
			names = append(names, u.Name)
		}
		return def.UsedByCount, names
	}

	// Each schedule counts its own user and not the other's.
	if n, names := usedBy("cronomicon"); n != 1 || len(names) != 1 || names[0] != "app-job" {
		t.Errorf("the in-app schedule is used by %v (count %d), want [app-job]", names, n)
	}
	if n, names := usedBy("git"); n != 1 || len(names) != 1 || names[0] != "git-job" {
		t.Errorf("Git's schedule is used by %v (count %d), want [git-job]", names, n)
	}
	// The list agrees with the detail.
	raw := a.must(http.MethodGet, "/api/v1/schedule-defs?pageSize=50", nil, http.StatusOK)
	var list struct {
		Items []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			UsedByCount int    `json:"usedByCount"`
		} `json:"items"`
	}
	_ = json.Unmarshal(raw, &list)
	for _, it := range list.Items {
		if it.Name == "nightly" && it.UsedByCount != 1 {
			t.Errorf("list: nightly (%s) usedByCount = %d, want 1 (by name it was 2)", it.Source, it.UsedByCount)
		}
	}

	// Editing the in-app schedule reaches its own user and leaves Git's alone.
	a.must(http.MethodPut, "/api/v1/schedule-defs/nightly", map[string]any{"cron": "0 0 5 * * *"}, http.StatusOK)
	if cron, _ := entryOf("cronomicon", "app-job"); cron != "0 0 5 * * *" {
		t.Errorf("the in-app job's entry is %q after its schedule was edited, want '0 0 5 * * *'", cron)
	}
	if cron, n := entryOf("git", "git-job"); n != 1 || cron != "0 0 1 * * *" {
		t.Errorf("after editing the IN-APP schedule the Git job has %d entries at %q, want its own '0 0 1 * * *' untouched", n, cron)
	}
	if got := a.str(`SELECT cron FROM schedules WHERE source='git' AND name='nightly'`); got != "0 0 1 * * *" {
		t.Errorf("the Git schedule itself was changed: %q", got)
	}

	// The delete is refused on account of the in-app job, and names it alone.
	code, body := a.do(http.MethodDelete, "/api/v1/schedule-defs/nightly", nil)
	if code != http.StatusConflict {
		t.Fatalf("deleting the in-app schedule while app-job uses it = %d, want 409", code)
	}
	if !bytes.Contains(body, []byte("app-job")) || bytes.Contains(body, []byte("git-job")) {
		t.Errorf("the refusal should name app-job and not git-job: %s", body)
	}
	// Forced: the in-app job is detached, Git's keeps its timing.
	a.must(http.MethodDelete, "/api/v1/schedule-defs/nightly?force=true", nil, http.StatusNoContent)
	if _, n := entryOf("cronomicon", "app-job"); n != 0 {
		t.Errorf("the force-delete left the in-app job bound (%d entries)", n)
	}
	if cron, n := entryOf("git", "git-job"); n != 1 || cron != "0 0 1 * * *" {
		t.Errorf("after force-deleting the IN-APP schedule the Git job has %d entries at %q, want its own untouched", n, cron)
	}
	// Purging it from the recycle bin does not reach Git's either.
	a.must(http.MethodDelete, "/api/v1/recycle-bin/schedule/nightly", nil, http.StatusOK, http.StatusNoContent)
	if _, n := entryOf("git", "git-job"); n != 1 {
		t.Errorf("purging the in-app schedule removed the Git job's entry (%d left)", n)
	}
}

// An in-app schedule with no user of its own is deleted without ceremony, even
// when Git's schedule of that name is in use. (It used to be refused, 409, on
// account of Git's schedule's job.)
func TestGR1_AnUnusedInAppScheduleIsNotHeldByGitsUsers(t *testing.T) {
	a := newGRSchedAPI(t)
	a.gitScheduleWithJob("nightly", "0 0 1 * * *", "git-job")
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "nightly", "cron": "0 0 3 * * *"}, http.StatusCreated)
	a.must(http.MethodDelete, "/api/v1/schedule-defs/nightly", nil, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='git-job'`); n != 1 {
		t.Errorf("deleting the unused in-app schedule left the Git job with %d entries, want 1", n)
	}
}

// A job that ticks `weekly` is bound to a LIVE schedule of that name. With the
// in-app `weekly` in the recycle bin and Git's `weekly` live, it is Git's.
// (Until Phase R1, present defect 14, it was the binned one: the lookup had no
// filter for the recycle bin and took whichever source sorted first.)
func TestGR1_TheComposerNeverBindsABinnedSchedule(t *testing.T) {
	a := newGRSchedAPI(t)
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "weekly", "cron": "0 0 2 * * 1"}, http.StatusCreated)
	a.must(http.MethodDelete, "/api/v1/schedule-defs/weekly", nil, http.StatusNoContent)
	if n := a.count(`SELECT COUNT(*) FROM schedules WHERE source='cronomicon' AND name='weekly' AND deleted_at IS NOT NULL`); n != 1 {
		t.Fatalf("the in-app schedule is not in the recycle bin (count %d)", n)
	}
	a.exec(`INSERT INTO schedules(uid, name, source, cron, content_hash, synced_at) VALUES('s-git-weekly', 'weekly', 'git', '0 0 7 * * 1', 'h', 't')`)

	a.must(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "weekly-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"weekly"}}, http.StatusCreated)
	if got := a.str(`SELECT cron FROM definition_schedules WHERE owner_name='weekly-job' AND name='weekly'`); got != "0 0 7 * * 1" {
		t.Errorf("the job's entry is %q, want the live Git schedule's '0 0 7 * * 1'", got)
	}
	if got := a.str(`SELECT schedule_uid FROM definition_schedules WHERE owner_name='weekly-job' AND name='weekly'`); got != "s-git-weekly" {
		t.Errorf("the job's entry is tied to %q, want the live Git schedule", got)
	}

	// With only a binned schedule of the name, there is nothing to bind.
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "gone", "cron": "0 0 2 * * 1"}, http.StatusCreated)
	a.must(http.MethodDelete, "/api/v1/schedule-defs/gone", nil, http.StatusNoContent)
	if code, body := a.do(http.MethodPost, "/api/v1/jobs",
		map[string]any{"name": "gone-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"gone"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("binding a job to a schedule that is only in the recycle bin = %d (%s), want 422", code, body)
	}
}

// A schedule restored from the recycle bin binds its definitions again, and a
// restored binding is a binding like any other: it carries its owner's uid and
// the schedule's, so saving the owning job again replaces it. (Until Phase R1,
// present defect 7, it carried neither, and the save added a second entry of
// the same name.)
func TestGR1_ARestoredSchedulesBindingsAreBindings(t *testing.T) {
	a := newGRSchedAPI(t)
	a.must(http.MethodPost, "/api/v1/schedule-defs", map[string]any{"name": "rs", "cron": "0 0 2 * * *"}, http.StatusCreated)
	job := map[string]any{"name": "rs-job", "scriptRef": "backup-db", "scope": "", "scheduleRefs": []string{"rs"}}
	a.must(http.MethodPost, "/api/v1/jobs", job, http.StatusCreated)
	entries := func() int {
		return a.count(`SELECT COUNT(*) FROM definition_schedules WHERE owner_source='cronomicon' AND owner_name='rs-job' AND name='rs'`)
	}
	schedUID := a.str(`SELECT uid FROM schedules WHERE source='cronomicon' AND name='rs'`)
	jobUID := a.str(`SELECT uid FROM jobs WHERE source='cronomicon' AND name='rs-job'`)

	a.must(http.MethodDelete, "/api/v1/schedule-defs/rs?force=true", nil, http.StatusNoContent)
	if n := entries(); n != 0 {
		t.Fatalf("the force-delete left %d entries, want the job detached", n)
	}
	a.must(http.MethodPost, "/api/v1/recycle-bin/schedule/rs/restore", nil, http.StatusOK, http.StatusNoContent)
	if n := entries(); n != 1 {
		t.Fatalf("the restore put back %d entries, want 1", n)
	}
	if got := a.str(`SELECT owner_uid FROM definition_schedules WHERE owner_name='rs-job' AND name='rs'`); got != jobUID {
		t.Errorf("the restored entry's owner_uid = %q, want the job's %q", got, jobUID)
	}
	if got := a.str(`SELECT schedule_uid FROM definition_schedules WHERE owner_name='rs-job' AND name='rs'`); got != schedUID {
		t.Errorf("the restored entry's schedule_uid = %q, want the schedule's %q", got, schedUID)
	}
	// So the restored schedule sees its user again, and an edit reaches it.
	a.must(http.MethodPut, "/api/v1/schedule-defs/rs", map[string]any{"cron": "0 0 9 * * *"}, http.StatusOK)
	if got := a.str(`SELECT cron FROM definition_schedules WHERE owner_name='rs-job' AND name='rs'`); got != "0 0 9 * * *" {
		t.Errorf("an edit of the restored schedule did not reach its restored binding: %q", got)
	}

	// The job is saved again, unchanged: one entry, not two.
	id := a.count(`SELECT rowid FROM jobs WHERE source='cronomicon' AND name='rs-job'`)
	a.must(http.MethodPut, "/api/v1/jobs/"+strconv.Itoa(id), job, http.StatusOK)
	if n := entries(); n != 1 {
		t.Errorf("saving the job after a restore left %d entries of one name, want 1", n)
	}
}
