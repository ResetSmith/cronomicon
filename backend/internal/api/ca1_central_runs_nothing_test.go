package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
)

// 2.5.3 CA1: the built-in agency (Central; 'global') is the installation's
// administration and runs nothing. Nothing that runs is created as its, or
// moved into it, through the app (CA-4), and what it owns from before is
// refused when it would run (CA-5).

// The composer's scopeless job (CA-5, first step, until CA7): working in an
// agency, a global administrator's job with no scope was that agency's. Since
// CA7 every job names a scope, so it is refused, a global administrator's too,
// and nothing is stored.
func TestCA7AScopelessJobIsRefusedEvenForAGlobalAdministrator(t *testing.T) {
	h, pool := gateServer(t)
	seedRHScript(t, pool) // Work, and deploy.sh in its repository, where its in-app jobs find it
	r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/jobs", gRoot,
		`{"name":"ca1-work-noscope","scope":"","scriptRef":"deploy.sh"}`, agencyctx.Header, workAgency)
	if r.Code != http.StatusUnprocessableEntity || errCode(r.Body.Bytes()) != "validation_failed" ||
		!strings.Contains(r.Body.String(), "every job names one of its agency's scopes") {
		t.Fatalf("a scopeless job composed in Work by a global administrator = %d %s, want 422 naming the rule", r.Code, r.Body)
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM jobs WHERE name = 'ca1-work-noscope'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d jobs stored (%v), want none", n, err)
	}
	// Its scope, named, is taken: the job is Work's.
	r = gateReqWithHeader(t, h, http.MethodPost, "/api/v1/jobs", gRoot,
		`{"name":"ca1-work-noscope","scope":"`+workScope+`","scriptRef":"deploy.sh"}`, agencyctx.Header, workAgency)
	if r.Code != http.StatusCreated {
		t.Fatalf("the same job on %s = %d %s, want 201", workScope, r.Code, r.Body)
	}
	var owner string
	if err := pool.QueryRow(`SELECT owner_agency FROM jobs WHERE name = 'ca1-work-noscope'`).Scan(&owner); err != nil || owner != workAgency {
		t.Fatalf("its owner = %q (%v), want %s", owner, err, workAgency)
	}
}

// Nothing that runs is created as Central's: named, defaulted, or by working
// in Central. Alert rules and push targets still are (CA-2).
func TestCA1NothingThatRunsIsCreatedInCentral(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	refused := func(what string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "central_runs_nothing") {
			t.Errorf("%s = %d %s, want 422 central_runs_nothing", what, code, body)
		}
	}
	none := func(method, path, body string) (int, string) {
		r := gateReq(t, h, method, path, gRoot, body)
		return r.Code, r.Body.String()
	}
	inCentral := func(method, path, body string) (int, string) {
		r := gateReqWithHeader(t, h, method, path, gRoot, body, agencyctx.Header, "global")
		return r.Code, r.Body.String()
	}

	for _, c := range []struct{ what, path, body string }{
		{"a secret", "/api/v1/env-secrets", `{"key":"CA1_S","source":"stored","value":"v"}`},
		{"a variable", "/api/v1/env-vars", `{"key":"CA1_V","value":"v"}`},
		{"a scope", "/api/v1/scopes", `{"scope":"ca1-scope","hosts":["h1"]}`},
		{"a job with no scope", "/api/v1/jobs", `{"name":"ca1-noscope","scope":"","scriptRef":"deploy.sh"}`},
		{"a job on a scope no agency holds", "/api/v1/jobs", `{"name":"ca1-nowhere","scope":"ca1-no-such","scriptRef":"deploy.sh"}`},
		{"a schedule", "/api/v1/schedule-defs", `{"name":"ca1-sched","cron":"0 2 * * *"}`},
		{"a jobless workflow", "/api/v1/workflows", `{"name":"ca1-wf","steps":[]}`},
	} {
		code, body := none(http.MethodPost, c.path, c.body)
		refused(c.what+", naming no agency", code, body)
	}
	code, body := inCentral(http.MethodPost, "/api/v1/env-vars", `{"key":"CA1_VC","value":"v"}`)
	refused("a variable created while working in Central", code, body)
	code, body = none(http.MethodPost, "/api/v1/env-vars", `{"key":"CA1_VN","value":"v","agencyIds":["global"]}`)
	refused("a variable naming Central", code, body)
	// Repeats of one id are one id (the review of CA1): naming Central twice
	// skipped the check and was compacted to Central after it.
	code, body = none(http.MethodPost, "/api/v1/env-vars", `{"key":"CA1_VD","value":"v","agencyIds":["global","global"]}`)
	refused("a variable naming Central twice", code, body)

	var n int
	_ = pool.QueryRow(`SELECT (SELECT COUNT(*) FROM env_vars WHERE key LIKE 'CA1_%')
	                        + (SELECT COUNT(*) FROM jobs WHERE name LIKE 'ca1-%')
	                        + (SELECT COUNT(*) FROM scopes WHERE name = 'ca1-scope')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d refused rows were written", n)
	}

	// What Central keeps: an alert rule naming no agency is still its.
	if code, body := none(http.MethodPost, "/api/v1/alerts",
		`{"targetMode":"all","trigger":"failure","channels":["email"],"recipients":"ops@example","enabled":true}`); code >= 300 {
		t.Errorf("Central's alert rule = %d %s, want it created", code, body)
	}
}

// Nothing moves INTO Central; a scope is not cloned into it.
func TestCA1NothingMovesIntoCentral(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/env-vars", gRoot,
		`{"key":"CA1_MOVE","value":"v"}`, agencyctx.Header, "ag:FIN")
	if r.Code != http.StatusCreated {
		t.Fatalf("seed FIN's variable = %d %s", r.Code, r.Body)
	}
	var id string
	if err := pool.QueryRow(`SELECT id FROM env_vars WHERE key = 'CA1_MOVE'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	r = gateReq(t, h, http.MethodPut, "/api/v1/env-var-agencies", gRoot,
		`{"repair":true,"reason":"ca1","assignments":[{"id":"`+id+`","agencyIds":["global"]}]}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "central_runs_nothing") {
		t.Errorf("moving FIN's variable into Central = %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
	var owner string
	_ = pool.QueryRow(`SELECT owner_agency FROM env_vars WHERE id = ?`, id).Scan(&owner)
	if owner != "ag:FIN" {
		t.Errorf("after the refused move it is %q's, want ag:FIN", owner)
	}

	r = gateReq(t, h, http.MethodPut, "/api/v1/scope-agencies", gRoot,
		`{"repair":true,"reason":"ca1","assignments":[{"id":"sc:fin","agencyIds":["global","global"]}]}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "central_runs_nothing") {
		t.Errorf("moving FIN's scope into Central, named twice = %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
	_ = pool.QueryRow(`SELECT owner_agency FROM scopes WHERE id = 'sc:fin'`).Scan(&owner)
	if owner != "ag:FIN" {
		t.Errorf("after the refused scope move it is %q's, want ag:FIN", owner)
	}

	r = gateReq(t, h, http.MethodPost, "/api/v1/scopes/sc:fin/clone", gRoot, `{"name":"ca1-clone"}`)
	if r.Code != http.StatusUnprocessableEntity || !strings.Contains(r.Body.String(), "central_runs_nothing") {
		t.Errorf("cloning FIN's scope into Central = %d %s, want 422 central_runs_nothing", r.Code, r.Body)
	}
}

// What Central owns from before (an upgrade's leftover) does not run: a
// person's Run of its job and of its workflow are refused with the reason.
func TestCA1ALeftoverOfCentralsIsRefusedWhenRun(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool) // j-glob: a job of Central's, born with no owner
	var rowid string
	if err := pool.QueryRow(`SELECT rowid FROM jobs WHERE uid = 'j-glob'`).Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	r := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+rowid+"/run", gRoot, `{}`)
	if r.Code != http.StatusConflict || !strings.Contains(r.Body.String(), "run_refused") ||
		!strings.Contains(r.Body.String(), "runs nothing") {
		t.Errorf("running Central's job = %d %s, want 409 run_refused naming the reason", r.Code, r.Body)
	}
	var n int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM runs WHERE job_uid = 'j-glob' AND status IN ('queued','running')`).Scan(&n)
	if n != 0 {
		t.Errorf("%d runs of Central's job were queued", n)
	}
}

// RB-30, as it stands after CA7: no grant authors a job with no scope. The
// restricted composer's 403 (requireComposeScope) became the rule's 422, asked
// before any grant, in the composer's own agency as everywhere.
func TestCA1ARestrictedComposerStillCannotAuthorAScopelessJob(t *testing.T) {
	h, pool := gateServer(t)
	seedSwitcher(t, pool)
	r := gateReqWithHeader(t, h, http.MethodPost, "/api/v1/jobs", gFinAdmin,
		`{"name":"ca1-fin-noscope","scope":"","scriptRef":"deploy.sh"}`, agencyctx.Header, "ag:FIN")
	if r.Code != http.StatusUnprocessableEntity || errCode(r.Body.Bytes()) != "validation_failed" {
		t.Errorf("FIN's administrator composing a scopeless job in FIN = %d %s, want 422 validation_failed", r.Code, r.Body)
	}
}
