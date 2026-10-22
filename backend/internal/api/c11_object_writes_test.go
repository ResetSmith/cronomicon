package api_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 2.5.0 C11, defects 71 and 72 (the pre-release review's 1 and 2). C2 put
// CF-1 on every GET; the writes that name one object and answer with it, or
// act on it, still checked the scope only, and an empty scope passes for
// everyone. A tag write on another agency's job with no scope answered 200
// with the row, script body included, while its GET answered 404.
//
// Every write route of the classes visible, open and verb that names an object
// is listed here and probed across agencies: FIN's people on TAX's and Global's
// objects are answered 404 (question 109: before any verb or scope check, so no
// 403 names another agency's scope), nothing changes, and no body leaks. FIN's
// own object is the control. A new such route joins objectWriteProbes or
// TestC11_EveryObjectWriteIsProbed fails.

// objectWriteProbes: the route (as writeRouteGates names it), and the request
// on an object of the given agency key ("fin", "tax", "glob").
type objectWrite struct {
	method string
	path   func(obj map[string]string, key string) string
	body   string
}

var objectWriteProbes = map[string]objectWrite{
	"POST /api/v1/jobs/{jobId}/run":                {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/jobs/" + o["job:"+k] + "/run" }, `{}`},
	"POST /api/v1/jobs/{jobId}/pause":              {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/jobs/" + o["job:"+k] + "/pause" }, `{}`},
	"POST /api/v1/jobs/{jobId}/resume":             {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/jobs/" + o["job:"+k] + "/resume" }, `{}`},
	"POST /api/v1/jobs/{jobId}/kill":               {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/jobs/" + o["job:"+k] + "/kill" }, `{}`},
	"PUT /api/v1/job-tags/{jobId}":                 {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/job-tags/" + o["job:"+k] }, `{"tags":[]}`},
	"PUT /api/v1/job-annotation/{jobId}":           {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/job-annotation/" + o["job:"+k] }, `{"annotation":"x"}`},
	"PATCH /api/v1/workflows/{workflowId}":         {http.MethodPatch, func(o map[string]string, k string) string { return "/api/v1/workflows/" + o["wf:"+k] }, `{"disabled":true}`},
	"POST /api/v1/workflows/{workflowId}/trigger":  {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/workflows/" + o["wf:"+k] + "/trigger" }, `{}`},
	"PUT /api/v1/workflow-tags/{workflowId}":       {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/workflow-tags/" + o["wf:"+k] }, `{"tags":[]}`},
	"PUT /api/v1/workflow-annotation/{workflowId}": {http.MethodPut, func(o map[string]string, k string) string { return "/api/v1/workflow-annotation/" + o["wf:"+k] }, `{"annotation":"x"}`},
	"POST /api/v1/workflows/runs/{traceId}/cancel": {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/workflows/runs/wr-live-" + k + "/cancel" }, `{}`},
	"DELETE /api/v1/pending-runs/{id}":             {http.MethodDelete, func(o map[string]string, k string) string { return "/api/v1/pending-runs/pr-" + k }, ``},
	"POST /api/v1/runs/{traceId}/notes":            {http.MethodPost, func(o map[string]string, k string) string { return "/api/v1/runs/rl-" + k + "/notes" }, `{"kind":"acknowledged","body":"x"}`},
	"POST /api/v1/workflow-runs/{traceId}/notes": {http.MethodPost, func(o map[string]string, k string) string {
		return "/api/v1/workflow-runs/wr-live-" + k + "/notes"
	}, `{"kind":"note","body":"x"}`},
	"PUT /api/v1/schedule-tags/{name}": {http.MethodPut, func(o map[string]string, k string) string {
		return "/api/v1/schedule-tags/s0-" + k + "-sched?source=cronomicon"
	}, `{"tags":[]}`},
	// The route addresses Global's repository only (an agency's own scripts are
	// the review's section F), so the probe is on Global's script.
	"PUT /api/v1/script-tags/{name...}": {http.MethodPut, func(o map[string]string, k string) string {
		return "/api/v1/script-tags/" + url.PathEscape("s0-"+k+"-script.sh")
	}, `{"tags":[]}`},
}

func TestC11_EveryObjectWriteIsProbed(t *testing.T) {
	for route, g := range writeRouteGates {
		if g.class != clsVisible && g.class != clsOpen && g.class != clsVerb {
			continue
		}
		if !strings.Contains(route, "{") {
			continue // names no object
		}
		if _, ok := objectWriteProbes[route]; !ok {
			t.Errorf("%s (class %s) names an object and is not in objectWriteProbes: add it, so another agency's object is answered 404", route, g.class)
		}
	}
	for route := range objectWriteProbes {
		if _, ok := writeRouteGates[route]; !ok {
			t.Errorf("objectWriteProbes names %s, which writeRouteGates does not", route)
		}
	}
}

func TestC11_NoObjectWriteAnswersAnotherAgency(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	exec := mustExec(t, pool)
	now := time.Now().UTC()
	at, later := now.Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339)

	// Bodies that must never come back to another agency.
	exec(`UPDATE jobs SET command = 'echo FIN-BODY'  WHERE uid = 'j-fin0'`)
	exec(`UPDATE jobs SET command = 'echo TAX-BODY'  WHERE uid = 'j-tax'`)
	exec(`UPDATE jobs SET command = 'echo GLOB-BODY' WHERE uid = 'j-glob'`)
	// TAX's own job with no scope: the empty scope passed for everyone.
	exec(`INSERT INTO jobs (uid, name, source, run_type, scope, owner_agency, command, created_at)
	      VALUES ('j-tax0', 's0-tax-noscope', 'cronomicon', 'bash', NULL, 'ag:TAX', 'echo TAX0-BODY', ?)`, at)
	// A live run of each, for Kill; a live workflow run of each, for cancel;
	// a parked run of each, for its cancel.
	exec(`INSERT INTO runs (id, job_name, job_uid, job_source, run_type, status, triggered_by, trigger_kind, created_at, scope, agencies_json) VALUES
	        ('rl-fin',  's0-fin-noscope', 'j-fin0',  'cronomicon', 'bash', 'queued', 'x', 'manual', ?, 'fin-hosts', '["FIN"]'),
	        ('rl-tax',  's0-tax-noscope', 'j-tax0',  'cronomicon', 'bash', 'queued', 'x', 'manual', ?, 'tax-hosts', '["TAX"]'),
	        ('rl-glob', 's0-glob-job',    'j-glob',  'cronomicon', 'bash', 'queued', 'x', 'manual', ?, NULL,        '["Global"]')`, at, at, at)
	exec(`INSERT INTO workflow_runs (id, workflow_name, workflow_source, workflow_uid, status, triggered_by, trigger_kind, created_at, agency_id) VALUES
	        ('wr-live-fin',  's0-fin-flow',  'cronomicon', 'w-fin',  'running', 'x', 'manual', ?, 'ag:FIN'),
	        ('wr-live-tax',  's0-tax-flow',  'cronomicon', 'w-tax',  'running', 'x', 'manual', ?, 'ag:TAX'),
	        ('wr-live-glob', 's0-glob-flow', 'cronomicon', 'w-glob', 'running', 'x', 'manual', ?, 'global')`, at, at, at)
	exec(`INSERT INTO pending_runs (id, kind, name, source, scope, run_at, scheduled_by, created_at, owner_uid) VALUES
	        ('pr-fin',  'job', 's0-fin-noscope', 'cronomicon', NULL, ?, 'x', ?, 'j-fin0'),
	        ('pr-tax',  'job', 's0-tax-noscope', 'cronomicon', NULL, ?, 'x', ?, 'j-tax0'),
	        ('pr-glob', 'job', 's0-glob-job',    'cronomicon', NULL, ?, 'x', ?, 'j-glob')`, later, at, later, at, later, at)
	exec(`UPDATE scripts SET command = 'echo GLOB-SCRIPT-BODY' WHERE uid = 'sx-glob'`)

	id := func(q, uid string) string { return rowID(t, pool, q, uid) }
	obj := map[string]string{
		"job:fin":  id(`SELECT rowid FROM jobs WHERE uid = ?`, "j-fin0"),
		"job:tax":  id(`SELECT rowid FROM jobs WHERE uid = ?`, "j-tax0"),
		"job:glob": id(`SELECT rowid FROM jobs WHERE uid = ?`, "j-glob"),
		"wf:fin":   id(`SELECT rowid FROM workflows WHERE uid = ?`, "w-fin"),
		"wf:tax":   id(`SELECT rowid FROM workflows WHERE uid = ?`, "w-tax"),
		"wf:glob":  id(`SELECT rowid FROM workflows WHERE uid = ?`, "w-glob"),
	}
	leaks := []string{"TAX-BODY", "TAX0-BODY", "GLOB-BODY", "GLOB-SCRIPT-BODY"}
	state := func() string {
		var b strings.Builder
		for _, q := range []string{
			`SELECT COALESCE(group_concat(id || status, ','), '') FROM runs WHERE id LIKE 'rl-%'`,
			`SELECT COALESCE(group_concat(id || status || cancelled, ','), '') FROM workflow_runs WHERE id LIKE 'wr-live-%'`,
			`SELECT COALESCE(group_concat(id, ','), '') FROM pending_runs`,
			`SELECT COALESCE(group_concat(owner_uid, ','), '') FROM paused_jobs`,
			`SELECT COALESCE(group_concat(uid || COALESCE(tags,''), ','), '') FROM jobs WHERE uid IN ('j-tax0','j-glob')`,
			`SELECT COALESCE(group_concat(uid || COALESCE(tags,''), ','), '') FROM workflows WHERE uid IN ('w-tax','w-glob')`,
			`SELECT COALESCE(group_concat(uid || COALESCE(tags,''), ','), '') FROM schedules WHERE uid IN ('sch-tax','sch-glob')`,
			`SELECT COALESCE(group_concat(uid || COALESCE(tags,''), ','), '') FROM scripts`,
			`SELECT COUNT(*) FROM workflow_runs`,
			`SELECT COUNT(*) FROM runs`,
			`SELECT COUNT(*) FROM run_notes`,
		} {
			var s string
			if err := pool.QueryRow(q).Scan(&s); err != nil {
				t.Fatalf("state %s: %v", q, err)
			}
			b.WriteString(s + "|")
		}
		return b.String()
	}

	for route, p := range objectWriteProbes {
		for _, who := range []string{gFinAdmin, gFinOperator, gFinViewer} {
			for _, k := range []string{"tax", "glob"} {
				if route == "PUT /api/v1/script-tags/{name...}" && k == "tax" {
					continue // the route addresses Global's scripts only
				}
				before := state()
				rec := gateReq(t, h, p.method, p.path(obj, k), who, p.body)
				if rec.Code != http.StatusNotFound {
					t.Errorf("%s by %s on %s's object = %d, want 404 (%s)", route, who, k, rec.Code, rec.Body)
				}
				for _, l := range leaks {
					if strings.Contains(rec.Body.String(), l) {
						t.Errorf("%s by %s on %s's object leaked %s", route, who, k, l)
					}
				}
				if after := state(); after != before {
					t.Errorf("%s by %s on %s's object changed something:\n before %s\n after  %s", route, who, k, before, after)
				}
			}
		}
	}

	// The control: FIN's administrator on FIN's own object is not told it is
	// absent (the routes answer as they did). Global's script is a global
	// administrator's.
	for route, p := range objectWriteProbes {
		who, k := gFinAdmin, "fin"
		if route == "PUT /api/v1/script-tags/{name...}" {
			who, k = gRoot, "glob"
		}
		if rec := gateReq(t, h, p.method, p.path(obj, k), who, p.body); rec.Code == http.StatusNotFound {
			t.Errorf("%s by %s on its own object = 404, want it found (%s)", route, who, rec.Body)
		}
	}
}
