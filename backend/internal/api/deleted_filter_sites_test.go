package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// FX-A5 — `deletedFilterSites`, the enumeration revisions_mount.go's header has
// been pointing at since the recycle bin shipped and which was never written.
//
// The header's claim is that every route which ACTS on a definition refuses a
// binned one. Nothing enforced that claim, and twice now the gap has been found
// by review rather than by a test: v0.57.32 caught the workflow trigger, pause
// and machine-name routes; FX-A caught GET /workflows/{id} and the reaction
// writes. Both times the shape was the same — a guard added to one twin reads
// as complete, because jobs and workflows are otherwise symmetric.
//
// This is deliberately BEHAVIOURAL rather than a list of SQL sites. A grep-based
// enumeration of `deleted_at IS NULL` would pass while a route consulted the
// filter and then ignored it, and would need editing every time a query moved.
// What matters to an operator is that the route says no.
//
// Adding a route that acts on a definition means adding a row here. The
// exceptions are enumerated below, with the reason each one is exempt — an
// exception with no reason is a bug that has been written down.

type defRoute struct {
	name   string
	method string
	path   func(jobID, wfID int64) string
	body   any
}

// Routes that must refuse a binned definition.
func binnedDefinitionRoutes() []defRoute {
	return []defRoute{
		{"read a job", http.MethodGet,
			func(j, _ int64) string { return "/api/v1/jobs/" + itoa(j) }, nil},
		{"edit a job", http.MethodPut,
			func(j, _ int64) string { return "/api/v1/jobs/" + itoa(j) },
			map[string]any{"name": "billing", "scriptRef": "deploy.sh", "scope": "", "runType": "bash"}},
		{"run a job", http.MethodPost,
			func(j, _ int64) string { return "/api/v1/jobs/" + itoa(j) + "/run" }, map[string]any{}},
		{"pause a job", http.MethodPost,
			func(j, _ int64) string { return "/api/v1/jobs/" + itoa(j) + "/pause" }, map[string]any{}},
		{"resume a job", http.MethodPost,
			func(j, _ int64) string { return "/api/v1/jobs/" + itoa(j) + "/resume" }, map[string]any{}},
		{"author reactions on a job", http.MethodPut,
			func(int64, int64) string { return "/api/v1/reactions/job/billing" },
			map[string]any{"reactions": []any{}}},

		{"read a workflow", http.MethodGet,
			func(_, w int64) string { return "/api/v1/workflows/" + itoa(w) }, nil},
		// disabled:false rather than true — the live half of this table runs every
		// case against the SAME definition, so a case that disables it would make
		// the next one fail for a reason that has nothing to do with the bin.
		{"edit a workflow", http.MethodPatch,
			func(_, w int64) string { return "/api/v1/workflows/" + itoa(w) },
			map[string]any{"disabled": false}},
		{"trigger a workflow", http.MethodPost,
			func(_, w int64) string { return "/api/v1/workflows/" + itoa(w) + "/trigger" }, map[string]any{}},
		{"author reactions on a workflow", http.MethodPut,
			func(int64, int64) string { return "/api/v1/reactions/workflow/release" },
			map[string]any{"reactions": []any{}}},
	}
}

// Documented exemptions — each acts on something OTHER than the definition, so
// a binned definition is not what it is being asked about:
//
//	POST /jobs/{id}/kill              acts on a RUN, not the definition; a run
//	                                  started before the bin must stay killable.
//	GET/POST /recycle-bin/...         the bin's own routes must see binned rows.
//	PUT /job-tags, /workflow-tags     tags are catalog metadata (FX-Q5: allowed
//	                                  on both twins, symmetric and deliberate).
//	PUT /job-annotation,              the same exemption, for the same reason
//	    /workflow-annotation          (AN-Q6): the bin KEEPS an annotation and a
//	                                  restore brings it back, so a note that can
//	                                  be read but not corrected would be the odd
//	                                  state — and "why did this get binned" is
//	                                  exactly the note someone writes then.
//	                                  Pinned by TestAnnotationWritableOnBinnedDefinition.
//	GET /jobs, /workflows, ...        list routes filter rather than refuse.
//	SLA's in-flight run watch         watches a RUN whose definition may be
//	                                  binned mid-flight (sla.go).
func TestDeletedFilterSites_EveryActingRouteRefusesABinnedDefinition(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	jobID := createRHJob(t, ts, client, csrf, "billing")
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "billing"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)

	// Bin both, and leave them binned for every case below.
	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/workflows/"+itoa(wf.ID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin workflow = %d: %s", c, b)
	}
	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin job = %d: %s", c, b)
	}

	for _, rt := range binnedDefinitionRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			code, body := rhDo(t, client, rt.method, ts.URL+rt.path(jobID, wf.ID), csrf, rt.body)
			if code >= 200 && code < 300 {
				t.Errorf("%s %s on a BINNED definition = %d (want a refusal): %s\n"+
					"a binned definition must be inert on every route that acts on one; if this "+
					"route genuinely should work, add it to the exemption list above WITH its reason",
					rt.method, rt.path(jobID, wf.ID), code, body)
			}
		})
	}
}

// The mirror: every one of those routes works on a LIVE definition. Without this
// half, the guard test above is satisfiable by a route that refuses everything.
func TestDeletedFilterSites_TheSameRoutesWorkWhenLive(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	jobID := createRHJob(t, ts, client, csrf, "billing")
	code, body := rhDo(t, client, http.MethodPost, ts.URL+"/api/v1/workflows", csrf, map[string]any{
		"name": "release", "steps": []map[string]any{{"type": "job", "name": "billing"}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create workflow = %d: %s", code, body)
	}
	var wf struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &wf)

	for _, rt := range binnedDefinitionRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			code, body := rhDo(t, client, rt.method, ts.URL+rt.path(jobID, wf.ID), csrf, rt.body)
			if code == http.StatusNotFound || code == http.StatusConflict {
				t.Errorf("%s %s on a LIVE definition = %d: %s\n"+
					"the binned-definition guard has caught a live one",
					rt.method, rt.path(jobID, wf.ID), code, body)
			}
		})
	}
}

// FX-A4 — a definition in the recycle bin is not a live reference.
//
// Reverse indexes ("who uses this schedule?") were computed straight off
// definition_schedules, whose rows deliberately survive a soft delete. So a
// binned job kept its schedule hostage: the delete returned 409 naming a
// definition the operator could not see, and the ?force=true it demanded went
// on to detach nothing that was firing.
func TestBinnedOwnerIsNotALiveScheduleReference(t *testing.T) {
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
	var job struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(body, &job)

	// While the job is LIVE the reference is real and the delete is blocked.
	if c, _ := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly", csrf, nil); c != http.StatusConflict {
		t.Fatalf("delete with a live referrer = %d, want 409", c)
	}

	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(job.ID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin job = %d: %s", c, b)
	}

	// usedBy must stop counting it. The ?source=amadeus is load-bearing:
	// getScheduleDef defaults to source=git, so without it this GET 404s and the
	// Contains check below passes for a reason that has nothing to do with the bin.
	code, body = rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/schedule-defs/nightly?source=amadeus", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("get schedule = %d (want 200): %s", code, body)
	}
	// usedByCount is always present (usedBy itself is omitted when empty), so it is
	// the field that can distinguish "filtered" from "never rendered".
	if !strings.Contains(string(body), `"usedByCount":0`) {
		t.Errorf("usedByCount is not 0 — a binned owner still counts as using the schedule: %s", body)
	}
	if strings.Contains(string(body), `"importer"`) {
		t.Errorf("a binned owner is still listed as using the schedule: %s", body)
	}
	// ...and the delete must no longer need forcing.
	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/schedule-defs/nightly", csrf, nil); c != http.StatusNoContent {
		t.Errorf("delete with only a BINNED referrer = %d, want 204: %s\n"+
			"the operator is being told to force-detach a definition that cannot fire", c, b)
	}
}

// FX-A4 — the reaction guard blocks AUTHORING onto a binned definition and
// nothing else.
//
// The first attempt filtered binned rows inside definitionSource, which looked
// equivalent and was not: that helper also backs the LIST and the DELETE, so
// reactions written before the bin became invisible and unremovable while still
// going live on restore — sealing in the exact hazard the block exists to
// prevent. Read and remove must keep working.
func TestBinnedDefinitionReactionsAreReadableAndRemovable(t *testing.T) {
	ts, pool := newTestServer(t)
	client, csrf := devLoginWithCSRF(t, ts)
	seedRHScript(t, pool)

	jobID := createRHJob(t, ts, client, csrf, "billing")
	createRHJob(t, ts, client, csrf, "upstream")

	if code, b := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/reactions/job/billing", csrf, map[string]any{
		"reactions": []map[string]any{
			{"name": "on-upstream", "onKind": "job", "onName": "upstream", "onSource": "amadeus", "onOutcome": "success"},
		},
	}); code != http.StatusOK && code != http.StatusNoContent {
		t.Fatalf("author reaction = %d: %s", code, b)
	}

	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/jobs/"+itoa(jobID), csrf, nil); c != http.StatusNoContent {
		t.Fatalf("bin job = %d: %s", c, b)
	}

	// Still listable — otherwise the operator cannot see what will wake up on restore.
	code, body := rhDo(t, client, http.MethodGet, ts.URL+"/api/v1/reactions/job/billing", csrf, nil)
	if code != http.StatusOK {
		t.Fatalf("list reactions on a binned owner = %d: %s", code, body)
	}
	if !strings.Contains(string(body), "on-upstream") {
		t.Errorf("a binned definition's reactions vanished from the list: %s\n"+
			"they still fire on restore, so hiding them hides the thing the operator must decide about", body)
	}

	// Authoring is refused...
	if c, _ := rhDo(t, client, http.MethodPut, ts.URL+"/api/v1/reactions/job/billing", csrf,
		map[string]any{"reactions": []any{}}); c < 400 {
		t.Errorf("authoring reactions onto a binned definition = %d, want a refusal", c)
	}
	// ...but removal is not, or the operator cannot defuse it before restoring.
	if c, b := rhDo(t, client, http.MethodDelete, ts.URL+"/api/v1/reactions/job/billing/on-upstream", csrf, nil); c != http.StatusNoContent {
		t.Errorf("deleting a binned definition's reaction = %d, want 204: %s", c, b)
	}
}
