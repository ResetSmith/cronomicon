package api_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RH — revision history + recycle bin (the prod-features plan §4).
//
// The two properties worth defending are the ones a careless change breaks
// silently:
//
//  1. A binned definition STOPS BEING LIVE. The soft delete is an UPDATE, so the
//     AFTER DELETE triggers never fire and its schedule bindings survive intact
//     — which means nothing about the row itself prevents the scheduler from
//     firing it. Only the deleted_at filters do. If one is dropped, everything
//     still looks fine until a deleted job runs at 2am.
//  2. Restoring loses nothing. That is the whole reason the bindings are left in
//     place rather than deleted on the way in.

func rhDo(t *testing.T, client *http.Client, method, url, csrf string, body any) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

// createRHJob makes an cronomicon job with one schedule entry and returns its rowid.
func createRHJob(t *testing.T, ts *httptest.Server, client *http.Client, csrf, name string) int64 {
	t.Helper()
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": name, "scriptRef": "deploy.sh", "runType": "bash",
		"scope":     "",
		"schedules": []map[string]any{{"name": "default", "cron": "0 2 * * *"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create job = %d: %s", code, body)
	}
	var out struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	return out.ID
}

func seedRHScript(t *testing.T, pool *sql.DB) {
	t.Helper()
	if _, err := pool.Exec(
		`INSERT OR IGNORE INTO scripts(name, run_type, content_hash, synced_at)
		 VALUES('deploy.sh','bash','sha256:x','2026-08-11T00:00:00Z')`); err != nil {
		t.Fatalf("seed script: %v", err)
	}
}

// Every write leaves a snapshot, and an unchanged save does not manufacture one.
func TestRevisionsRecordEachWrite(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	id := createRHJob(t, ts, client, csrf, "billing")

	edit := func(desc string) {
		code, body := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(id), csrf, map[string]any{
			"name": "billing", "scriptRef": "deploy.sh", "runType": "bash", "description": desc,
			"scope":     "",
			"schedules": []map[string]any{{"name": "default", "cron": "0 2 * * *"}},
		})
		if code != http.StatusOK {
			t.Fatalf("edit = %d: %s", code, body)
		}
	}
	edit("first change")
	edit("first change") // byte-identical — must NOT add a revision

	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/definitions/job/billing/revisions", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list revisions = %d: %s", code, body)
	}
	var out struct {
		Items []struct {
			RevisionNo int64  `json:"revisionNo"`
			Action     string `json:"action"`
			Actor      string `json:"actor"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Items) != 2 {
		t.Fatalf("revisions = %d, want 2 (created + one real edit; the no-op save must not count): %s", len(out.Items), body)
	}
	// Newest first.
	if out.Items[0].RevisionNo != 2 || out.Items[0].Action != "updated" {
		t.Errorf("latest revision = %d/%s, want 2/updated", out.Items[0].RevisionNo, out.Items[0].Action)
	}
	if out.Items[1].Action != "created" {
		t.Errorf("first revision action = %q, want created", out.Items[1].Action)
	}
	if out.Items[0].Actor == "" {
		t.Error("revision has no actor")
	}
}

// Restoring an older revision re-submits it through the write path.
func TestRestoreRevisionRewritesTheDefinition(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	id := createRHJob(t, ts, client, csrf, "billing")

	code, body := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/jobs/"+itoa(id), csrf, map[string]any{
		"name": "billing", "scriptRef": "deploy.sh", "runType": "bash", "description": "the regrettable edit",
		"scope":     "",
		"schedules": []map[string]any{{"name": "default", "cron": "0 2 * * *"}},
	})
	if code != http.StatusOK {
		t.Fatalf("edit = %d: %s", code, body)
	}

	// Operator-owned tags are set AFTER the snapshot was taken; a restore must
	// not revert them (PF-Q8).
	if _, err := pool.Exec(`UPDATE jobs SET tags='["keep-me"]' WHERE source='cronomicon' AND name='billing'`); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	code, body = rhDo(t, client, http.MethodPost,
		ts.URL+"/api/v1/definitions/job/billing/revisions/1/restore", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, body)
	}

	var desc sql.NullString
	var tags string
	if err := pool.QueryRow(
		`SELECT description, COALESCE(tags,'[]') FROM jobs WHERE source='cronomicon' AND name='billing'`).Scan(&desc, &tags); err != nil {
		t.Fatalf("read job: %v", err)
	}
	if desc.String == "the regrettable edit" {
		t.Error("restore did not roll the description back")
	}
	if tags != `["keep-me"]` {
		t.Errorf("tags = %s, want [\"keep-me\"] — a restore must not revert operator-owned tags", tags)
	}
	// The restore is itself a revision: 1 created, 2 updated, 3 the restore.
	_, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/definitions/job/billing/revisions", csrf, nil)
	var out struct {
		Items []struct{ RevisionNo int64 } `json:"items"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Items) != 3 {
		t.Errorf("revisions after restore = %d, want 3 — a restore is a change like any other", len(out.Items))
	}
}

// The load-bearing one: a binned definition must be invisible to the catalog AND
// unreachable by every path that would act on it.
func TestSoftDeletedJobIsInertButRecoverable(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	id := createRHJob(t, ts, client, csrf, "billing")

	if code, body := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(id), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", code, body)
	}

	// Its schedule bindings SURVIVE — that is what makes the restore lossless,
	// and exactly why the filters below have to do the work instead.
	var bindings int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='billing'`).Scan(&bindings)
	if bindings == 0 {
		t.Fatal("the soft delete destroyed the schedule bindings — an undelete could not restore them")
	}

	// Gone from the catalog.
	_, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs", csrf, nil)
	if bytes.Contains(body, []byte(`"billing"`)) {
		t.Error("a binned job is still listed in the catalog")
	}
	// Unreadable, unrunnable, uneditable.
	for _, tc := range []struct {
		name, method, path string
		want               int
		body               any
	}{
		{"read", http.MethodGet, "/api/v1/jobs/" + itoa(id), http.StatusNotFound, nil},
		{"run", http.MethodPost, "/api/v1/jobs/" + itoa(id) + "/run", http.StatusNotFound, map[string]any{}},
		{"pause", http.MethodPost, "/api/v1/jobs/" + itoa(id) + "/pause", http.StatusNotFound, map[string]any{}},
		{"edit", http.MethodPut, "/api/v1/jobs/" + itoa(id), http.StatusConflict,
			map[string]any{"name": "billing", "scriptRef": "deploy.sh", "scope": "", "runType": "bash"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, b := rhDo(t, client, tc.method, ts.URL+tc.path, csrf, tc.body); code != tc.want {
				t.Errorf("%s = %d, want %d: %s", tc.name, code, tc.want, b)
			}
		})
	}

	// The name is still taken, and the 409 says why rather than leaving the
	// operator to guess.
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": "billing", "scriptRef": "deploy.sh", "runType": "bash",
		"scope": "",
	})
	if code != http.StatusConflict {
		t.Errorf("recreate = %d, want 409", code)
	}
	if !bytes.Contains(body, []byte("recycle bin")) {
		t.Errorf("the 409 does not mention the recycle bin: %s", body)
	}

	// It is in the bin...
	code, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/recycle-bin", csrf, nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte(`"billing"`)) {
		t.Fatalf("recycle bin = %d: %s", code, body)
	}

	// ...and comes back whole.
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/job/billing/restore", csrf, nil); code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, b)
	}
	_, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs", csrf, nil)
	if !bytes.Contains(body, []byte(`"billing"`)) {
		t.Error("the restored job is not back in the catalog")
	}
	var after int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='billing'`).Scan(&after)
	if after != bindings {
		t.Errorf("schedule bindings after restore = %d, want %d — the restore was lossy", after, bindings)
	}
	if code, _ := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa(id), csrf, nil); code != http.StatusOK {
		t.Errorf("restored job reads as %d, want 200", code)
	}
}

// Purging is the hard delete the soft delete deferred.
func TestPurgeFreesTheNameAndRemovesTheRow(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	id := createRHJob(t, ts, client, csrf, "billing")

	if code, _ := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(id), csrf, nil); code != http.StatusNoContent {
		t.Fatal("delete failed")
	}
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/recycle-bin/job/billing", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("purge = %d: %s", code, b)
	}

	var rows, bindings int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE source='cronomicon' AND name='billing'`).Scan(&rows)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM definition_schedules WHERE owner_name='billing'`).Scan(&bindings)
	if rows != 0 {
		t.Errorf("purged job row survived: %d", rows)
	}
	if bindings != 0 {
		t.Errorf("purge left %d schedule bindings — the real DELETE should have fired the cascade trigger", bindings)
	}
	// The name is free again.
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": "billing", "scriptRef": "deploy.sh", "runType": "bash",
		"scope": "",
	}); code != http.StatusCreated {
		t.Errorf("recreate after purge = %d, want 201: %s", code, b)
	}
}

// Workflows and schedules travel the same road.
func TestSoftDeleteCoversWorkflowsAndSchedules(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "step-one")

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "step-one"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
		"name": "nightly", "cron": "0 3 * * *",
	}); code != http.StatusCreated {
		t.Fatalf("create schedule = %d: %s", code, b)
	}

	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/workflows/"+itoa(wf.ID), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete workflow = %d: %s", code, b)
	}
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete schedule = %d: %s", code, b)
	}

	_, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/recycle-bin", csrf, nil)
	for _, want := range []string{`"release"`, `"nightly"`, `"workflow"`, `"schedule"`} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("recycle bin missing %s: %s", want, body)
		}
	}
	// Both are out of their catalogs.
	if _, b := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/workflows", csrf, nil); bytes.Contains(b, []byte(`"release"`)) {
		t.Error("a binned workflow is still listed")
	}
	if _, b := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/schedule-defs", csrf, nil); bytes.Contains(b, []byte(`"nightly"`)) {
		t.Error("a binned schedule is still listed")
	}
	// And both come back.
	for _, kind := range []string{"workflow/release", "schedule/nightly"} {
		if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/"+kind+"/restore", csrf, nil); code != http.StatusOK {
			t.Errorf("restore %s = %d: %s", kind, code, b)
		}
	}
}

// The workflow twin of TestSoftDeletedJobIsInertButRecoverable, and the reason
// it needed writing: fetchWorkflowByID deliberately returns binned rows so the
// recycle bin can read them, so the tombstone by itself stops NOTHING. Every
// route that acts on a workflow has to refuse one on its own, and trigger and
// pause did not.
//
// The machine surface is the third way in and the worst of the three: the name
// stays claimed while binned (PRIMARY KEY (source, name)), so a name-addressed
// trigger resolved to a live rowid and fired a definition the operator had
// deleted — with no session anywhere in the picture to notice.
func TestBinnedWorkflowIsNotTriggerable(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	jobID := createRHJob(t, ts, client, csrf, "step-one")

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "step-one"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)
	wfPath := ts.URL + "/api/v1/workflows/" + itoa(wf.ID)

	// ET-B: the name-addressed trigger authenticates as a service account, so the
	// principal has to exist before the definitions go into the bin.
	_, token := mintAccount(t, ts, client, csrf, map[string]any{
		"name": "nagios", "role": "admin", "allScopes": true,
	})
	// PF-Q15's opt-in is set, so the job twin's 404 below can only come from the
	// deleted_at filter — a 403 would mean we proved the wrong thing.
	if _, err := pool.Exec(`UPDATE jobs SET requestable=1 WHERE source='cronomicon' AND name='step-one'`); err != nil {
		t.Fatalf("opt the job in: %v", err)
	}

	// The control: while it is live, pausing it is a 200. Whatever these routes
	// answer after the delete cannot then be blamed on scopes, CSRF or the role.
	if code, b := rhDo(t, client, http.MethodPatch, wfPath, csrf, map[string]any{"disabled": false}); code != http.StatusOK {
		t.Fatalf("pause a live workflow = %d, want 200: %s", code, b)
	}

	if code, b := rhDo(t, client, http.MethodDelete, wfPath, csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete workflow = %d: %s", code, b)
	}
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete job = %d: %s", code, b)
	}

	t.Run("trigger", func(t *testing.T) {
		if code, b := rhDo(t, client, http.MethodPost, wfPath+"/trigger", csrf, map[string]any{}); code != http.StatusNotFound {
			t.Errorf("trigger a binned workflow = %d, want 404: %s", code, b)
		}
	})
	// Before the pause case, deliberately: a paused workflow answers 409 on the
	// trigger path, so pausing first would mask a broken resolver behind the right
	// kind of refusal for the wrong reason.
	t.Run("token trigger by name", func(t *testing.T) {
		resp := triggerAs(t, ts, token, "/api/v1/trigger/workflows/release")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("token-triggering a binned workflow = %d, want 404", resp.StatusCode)
		}
	})
	// The job twin is the control rather than a second regression: runJob has
	// filtered deleted_at since RH, so a binned job was already 404 on this route
	// by the time it got there. That downstream guard is precisely why the
	// workflow gap survived — nothing in the shared resolver had to hold.
	t.Run("token trigger a binned job by name", func(t *testing.T) {
		resp := triggerAs(t, ts, token, "/api/v1/trigger/jobs/step-one")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("token-triggering a binned job = %d, want 404", resp.StatusCode)
		}
	})
	t.Run("pause", func(t *testing.T) {
		if code, b := rhDo(t, client, http.MethodPatch, wfPath, csrf, map[string]any{"disabled": true}); code != http.StatusNotFound {
			t.Errorf("pause a binned workflow = %d, want 404: %s", code, b)
		}
	})

	// The point of all four: nothing ran.
	var runs int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs)
	if runs != 0 {
		t.Errorf("a binned definition produced %d runs", runs)
	}
	var pending int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM pending_runs`).Scan(&pending)
	if pending != 0 {
		t.Errorf("a binned definition produced %d pending runs", pending)
	}
}

// A schedule create is now transactional. Before RH it ran a bare INSERT on the
// pool with no transaction at all, which was survivable only while it was a lone
// statement — it now commits alongside the revision snapshot.
func TestScheduleCreateWritesRevisionAtomically(t *testing.T) {
	ts, _ := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
		"name": "nightly", "cron": "0 3 * * *", "description": "first",
	}); code != http.StatusCreated {
		t.Fatalf("create = %d: %s", code, b)
	}
	if code, b := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/schedule-defs/nightly", csrf, map[string]any{
		"name": "nightly", "cron": "0 4 * * *", "description": "second",
	}); code != http.StatusOK {
		t.Fatalf("update = %d: %s", code, b)
	}

	_, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/definitions/schedule/nightly/revisions", csrf, nil)
	var out struct {
		Items []struct {
			Action   string          `json:"action"`
			Snapshot json.RawMessage `json:"snapshot"`
		} `json:"items"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Items) != 2 {
		t.Fatalf("schedule revisions = %d, want 2: %s", len(out.Items), body)
	}
	if !bytes.Contains(out.Items[1].Snapshot, []byte("0 3 * * *")) {
		t.Errorf("the created revision does not carry the original cron: %s", out.Items[1].Snapshot)
	}
}

// Schedules are the exception the "a soft delete never takes the definition
// apart" rule has to admit, and the exception is where the restore was lossy.
//
// A binned schedule must stop firing its referrers, and its ref-expanded runtime
// entries are keyed by source_ref rather than cascaded by a trigger — so
// deleteScheduleDef really DELETEs them. Those rows are the ONLY record that a
// job references the schedule (the referrer stores its refs nowhere else), so a
// restore that cleared deleted_at alone gave back a catalog row no definition
// fires on. The delete now captures them into its tombstone revision and the
// restore replays them, in the same transaction as the un-bin.
func TestRestoringABinnedScheduleRebuildsItsBindings(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
		"name": "nightly", "cron": "0 0 2 * * *", "env": map[string]string{"STAGE": "prod"},
	}); code != http.StatusCreated {
		t.Fatalf("create schedule = %d: %s", code, b)
	}
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": "backup-job", "scriptRef": "deploy.sh", "runType": "bash",
		"scope":        "",
		"scheduleRefs": []string{"nightly"},
	}); code != http.StatusCreated {
		t.Fatalf("create referencing job = %d: %s", code, b)
	}

	// The whole row, so "intact" means every column rather than the ones that
	// happened to be checked.
	type binding struct {
		ownerSource, ownerKind, name, cron, sourceRef string
		env                                           sql.NullString
		position                                      int
	}
	read := func(owner string) (binding, bool) {
		t.Helper()
		var b binding
		err := pool.QueryRow(`
			SELECT owner_source, owner_kind, name, cron, source_ref, env, position
			  FROM definition_schedules WHERE owner_name=? AND source_ref='nightly'`, owner).
			Scan(&b.ownerSource, &b.ownerKind, &b.name, &b.cron, &b.sourceRef, &b.env, &b.position)
		if err == sql.ErrNoRows {
			return binding{}, false
		}
		if err != nil {
			t.Fatalf("read binding: %v", err)
		}
		return b, true
	}
	mirror := func() sql.NullString {
		t.Helper()
		var m sql.NullString
		if err := pool.QueryRow(
			`SELECT schedule FROM jobs WHERE source='cronomicon' AND name='backup-job'`).Scan(&m); err != nil {
			t.Fatalf("read legacy mirror: %v", err)
		}
		return m
	}

	before, ok := read("backup-job")
	if !ok {
		t.Fatal("the job's scheduleRef did not expand into a binding at all")
	}
	if before.sourceRef != "nightly" || before.cron != "0 0 2 * * *" {
		t.Fatalf("binding = %+v, want source_ref 'nightly' / cron '0 0 2 * * *'", before)
	}
	if m := mirror(); m.String != "0 0 2 * * *" {
		t.Fatalf("legacy mirror before the delete = %v, want the cron", m)
	}

	// ?force is the operator saying "detach the referrers" — the referrers stop
	// firing, which is exactly the state the restore has to be able to undo.
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly?force=true", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("forced delete = %d: %s", code, b)
	}
	if _, ok := read("backup-job"); ok {
		t.Fatal("the forced delete left the binding behind — the referrer would keep firing on a binned schedule")
	}
	if m := mirror(); m.Valid {
		t.Errorf("legacy mirror after the delete = %q, want NULL", m.String)
	}

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/schedule/nightly/restore", csrf, nil); code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, b)
	}

	after, ok := read("backup-job")
	if !ok {
		t.Fatal("the restore did not rebuild the binding — the schedule is back in the catalog with nothing referencing it")
	}
	if after != before {
		t.Errorf("restored binding = %+v, want %+v — the replay is meant to be byte-for-byte", after, before)
	}
	// The legacy display column mirrors the lowest-position entry, so it has to be
	// recomputed on the way back in exactly as the delete recomputed it on the way
	// out; leaving it NULL is the visible half of a restore that half happened.
	if m := mirror(); m.String != "0 0 2 * * *" {
		t.Errorf("legacy mirror after the restore = %v, want '0 0 2 * * *'", m)
	}
	// And the schedule itself is live again, not merely present.
	if _, b := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/schedule-defs", csrf, nil); !bytes.Contains(b, []byte(`"nightly"`)) {
		t.Error("the restored schedule is not back in the catalog")
	}
}

// FX2-C2 — deleting a schedule whose only referrer is BINNED proceeds without
// force (FX-Q9 as split by FX2-Q1: the delete is soft and the tombstone replays
// the binding on restore), but it must not proceed SILENTLY: the Change Log and
// Activity lines name the binned referrer, because they are the only record of
// who to ask when the restored job comes back schedule-less a week later.
func TestDeletingAScheduleNamesItsBinnedReferrers(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
		"name": "monthly", "cron": "0 0 4 1 * *",
	}); code != http.StatusCreated {
		t.Fatalf("create schedule = %d: %s", code, b)
	}
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
		"name": "archiver", "scriptRef": "deploy.sh", "runType": "bash",
		"scope":        "",
		"scheduleRefs": []string{"monthly"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create referencing job = %d: %s", code, body)
	}
	var job struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &job)
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(job.ID), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("bin owner = %d: %s", code, b)
	}

	// No force needed: the only referrer is binned, so nothing live blocks.
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/monthly", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete with binned-only referrer = %d, want 204: %s", code, b)
	}

	// …but the audit surfaces say what happened, and to whom.
	var details string
	if err := pool.QueryRow(`
		SELECT COALESCE(details,'') FROM change_log
		 WHERE category='Schedules' AND action='Deleted' AND target='monthly'
		 ORDER BY at DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatalf("read change_log: %v", err)
	}
	if !strings.Contains(details, "archiver") || !strings.Contains(details, "recycle-binned") {
		t.Errorf("change_log details = %q — a delete that detached a binned referrer's binding "+
			"must name it, or the detachment reads as data loss with no explanation", details)
	}
	if !strings.Contains(details, "restore the schedule") {
		t.Errorf("change_log details = %q — the remedy (restore replays the binding) should "+
			"travel with the warning", details)
	}
}

// FX-A2 — a binned owner KEEPS its binding across the schedule's restore, and a
// PURGED one does not. The distinction is the whole point: the binding row is
// the only record that an owner uses a schedule, so skipping the replay for a
// soft-deleted owner destroyed that fact permanently — the restore consumed the
// tombstone and nothing on the owner's own restore path reclaims a binding.
//
// This test formerly asserted the opposite for the binned case, on the reasoning
// that a replayed binding "invents a schedule entry for a definition nobody can
// see". That concern was real but is answered at the READ layer instead (FX-A4:
// drainScheduleRows drops a binned owner's entries), which costs no data. It
// also never actually exercised a hard-deleted owner despite its name, so the
// case it claimed to protect went uncovered — it is covered below now.
func TestRestoringAScheduleReplaysBinnedOwnersButNotPurgedOnes(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
		"name": "weekly", "cron": "0 0 3 * * 1",
	}); code != http.StatusCreated {
		t.Fatalf("create schedule = %d: %s", code, b)
	}
	var doomedID, purgedID int64
	for _, name := range []string{"survivor", "purged", "doomed"} {
		code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
			"name": name, "scriptRef": "deploy.sh", "runType": "bash",
			"scope":        "",
			"scheduleRefs": []string{"weekly"},
		})
		if code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", name, code, body)
		}
		var out struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &out)
		switch name {
		case "purged":
			purgedID = out.ID
		case "doomed":
			doomedID = out.ID
		}
	}

	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/weekly?force=true", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("forced delete = %d: %s", code, b)
	}
	// Bin an owner while the schedule is in the bin alongside it. The owner is
	// only soft-deleted, so the row is still there — the probe has to ask about
	// existence, and a binned owner still exists.
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(doomedID), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("delete owner = %d: %s", code, b)
	}
	// And PURGE a second owner outright — bin, then empty. Its row is gone, so
	// replaying its binding really would invent an entry for a definition that
	// is not there.
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(purgedID), csrf, nil); code != http.StatusNoContent {
		t.Fatalf("bin purged-owner = %d: %s", code, b)
	}
	if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/recycle-bin/job/purged", csrf, nil); code != http.StatusNoContent {
		t.Fatalf("purge owner = %d: %s", code, b)
	}

	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/schedule/weekly/restore", csrf, nil); code != http.StatusOK {
		t.Fatalf("restore = %d: %s", code, b)
	}

	owners := map[string]bool{}
	rows, err := pool.Query(`SELECT owner_name FROM definition_schedules WHERE source_ref='weekly'`)
	if err != nil {
		t.Fatalf("read bindings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		owners[n] = true
	}
	if !owners["survivor"] {
		t.Error("the live owner's binding was not replayed")
	}
	if !owners["doomed"] {
		t.Error("a binned owner LOST its binding — restoring it now yields a job that " +
			"references no schedule, and nothing anywhere still records that it did")
	}
	if owners["purged"] {
		t.Error("a purged owner was given its binding back — the restore invented a schedule entry for a definition that is gone")
	}

	// The binned owner's replayed entry must not be VISIBLE while it is binned:
	// that is the read-layer answer (FX-A4) which lets the replay above be safe.
	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/schedules", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list schedules = %d: %s", code, body)
	}
	if strings.Contains(string(body), `"doomed"`) {
		t.Error("the schedules inventory listed a binned owner's entry — it promises a run the scheduler will never fire")
	}
}

// FX-A2 — the same loss, reached the way an operator actually reaches it: the
// order they happen to restore in. Restoring the SCHEDULE first used to consume
// its tombstone while the job was still binned, so the job came back with no
// schedule and no record it ever had one. Restoring the job first worked. The
// operator is never told which order they picked, so both must survive.
func TestScheduleBindingSurvivesEitherRestoreOrder(t *testing.T) {
	for _, tc := range []struct{ name, first string }{
		{"schedule restored first", "schedule"},
		{"job restored first", "job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, pool := newTestServer(t)
			client, csrf := devLoginWithCSRF(t, ts)
			seedRHScript(t, pool)

			if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/schedule-defs", csrf, map[string]any{
				"name": "nightly", "cron": "0 0 2 * * *",
			}); code != http.StatusCreated {
				t.Fatalf("create schedule = %d: %s", code, b)
			}
			code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/jobs", csrf, map[string]any{
				"name": "importer", "scriptRef": "deploy.sh", "runType": "bash",
				"scope":        "",
				"scheduleRefs": []string{"nightly"},
			})
			if code != http.StatusCreated {
				t.Fatalf("create job = %d: %s", code, body)
			}
			var out struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(body, &out)

			if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly?force=true", csrf, nil); code != http.StatusNoContent {
				t.Fatalf("delete schedule = %d: %s", code, b)
			}
			if code, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(out.ID), csrf, nil); code != http.StatusNoContent {
				t.Fatalf("delete job = %d: %s", code, b)
			}

			restoreSchedule := func() {
				if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/schedule/nightly/restore", csrf, nil); code != http.StatusOK {
					t.Fatalf("restore schedule = %d: %s", code, b)
				}
			}
			restoreJob := func() {
				if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/recycle-bin/job/importer/restore", csrf, nil); code != http.StatusOK {
					t.Fatalf("restore job = %d: %s", code, b)
				}
			}
			if tc.first == "schedule" {
				restoreSchedule()
				restoreJob()
			} else {
				restoreJob()
				restoreSchedule()
			}

			var n int
			if err := pool.QueryRow(
				`SELECT COUNT(*) FROM definition_schedules WHERE source_ref='nightly' AND owner_name='importer'`).Scan(&n); err != nil {
				t.Fatalf("read bindings: %v", err)
			}
			if n != 1 {
				t.Fatalf("binding count = %d, want 1 — the job is back but fires on nothing, and the only record that it used 'nightly' is gone", n)
			}
		})
	}
}

// ─── SW: sub-workflow authoring guards ───────────────────────────────────────

// The compose path must refuse a reference that closes a cycle, and say WHERE.
// The runtime ceiling is the backstop, but catching it at authoring time is the
// difference between an error message and a workflow that burns three levels of
// runs before giving up.
func TestSubWorkflowCycleIsRefusedAtAuthoringTime(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)
	createRHJob(t, ts, client, csrf, "leaf")

	// a → (nothing yet)
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "alpha", "steps": []map[string]any{{"type": "job", "name": "leaf"}},
	}); code != http.StatusCreated {
		t.Fatalf("create alpha = %d: %s", code, b)
	}
	// b → a
	if code, b := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "beta", "steps": []map[string]any{{"type": "workflow", "name": "s", "workflow": "alpha"}},
	}); code != http.StatusCreated {
		t.Fatalf("create beta = %d: %s", code, b)
	}

	// Now close the loop: a → b. alpha → beta → alpha.
	code, body := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/workflows/"+itoa(workflowID(t, pool, "alpha")), csrf,
		map[string]any{"name": "alpha", "steps": []map[string]any{{"type": "workflow", "name": "s", "workflow": "beta"}}})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("closing a cycle = %d, want 422: %s", code, body)
	}
	if !bytes.Contains(body, []byte("cycle")) {
		t.Errorf("the refusal does not mention a cycle: %s", body)
	}
	// RX's lesson: naming the path is what makes it actionable.
	if !bytes.Contains(body, []byte("alpha")) || !bytes.Contains(body, []byte("beta")) {
		t.Errorf("the refusal does not name the path: %s", body)
	}
}

// Self-reference is the degenerate cycle and gets its own clearer message.
func TestSubWorkflowSelfReferenceIsRefused(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "ouroboros", "steps": []map[string]any{{"type": "workflow", "name": "s", "workflow": "ouroboros"}},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("self-reference = %d, want 422: %s", code, body)
	}
	if !bytes.Contains(body, []byte("cannot run itself")) {
		t.Errorf("unclear message for a self-reference: %s", body)
	}
}

// A reference to a workflow that does not exist is refused too.
func TestSubWorkflowUnknownReferenceIsRefused(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "parent", "steps": []map[string]any{{"type": "workflow", "name": "s", "workflow": "ghost"}},
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown reference = %d, want 422: %s", code, body)
	}
	if !bytes.Contains(body, []byte("unknown workflow")) {
		t.Errorf("unclear message: %s", body)
	}
}

func workflowID(t *testing.T, pool *sql.DB, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(`SELECT rowid FROM workflows WHERE source='cronomicon' AND name=?`, name).Scan(&id); err != nil {
		t.Fatalf("lookup workflow %s: %v", name, err)
	}
	return id
}
