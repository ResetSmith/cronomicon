package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Global is a real agency since migration 1220 (LR-8, LR-21). These pin the
// rules that make it one the installation can rely on: it is always there, it
// is nobody's to grant, a row is in it or in a department and never both, and
// a row that is in no agency at all is a fault, not a meaning.

// LR-21, LR-27 — the built-in agency cannot be deleted, by anyone. Since 2.5.2
// (GR) it may be renamed, by a global administrator, and no name is reserved:
// names are unique, and that is the only rule.
func TestGlobalAgency_IsPermanentAndRenamable(t *testing.T) {
	h, pool := gateServer(t)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/global", gRoot, ``); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "builtin_agency" {
		t.Errorf("delete Global = %d %s, want 422 builtin_agency (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/global", gFinAdmin, `{"name":"Central"}`); rec.Code != http.StatusForbidden {
		t.Errorf("FIN's admin renaming the built-in agency = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/global", gRoot, `{"name":"Central","description":"the platform team"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("renaming the built-in agency = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agencies WHERE id='global' AND name='Central' AND builtin=1`); n != 1 {
		t.Fatal("the built-in row was not renamed, or lost its mark")
	}
	// "Global" is an ordinary name now, free for another agency…
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN", gRoot, `{"name":"Global"}`); rec.Code != http.StatusOK {
		t.Errorf("renaming FIN to Global once it is free = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	// …and a name in use is refused, the built-in agency's included.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:TAX", gRoot, `{"name":"Central"}`); rec.Code/100 == 2 {
		t.Errorf("renaming TAX to the built-in agency's name = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	// The database keeps the mark even for a write that does not come through the route.
	if _, err := pool.Exec(`UPDATE agencies SET builtin = 0 WHERE id = 'global'`); err == nil {
		t.Error("a raw write un-marked the built-in agency")
	}
}

// LR-9, LR-25 — Global cannot be named in a grant or a service account. The
// way to be a global administrator is an all-scopes grant; a grant "on Global"
// would read like one and reach only Global's scopes.
func TestGlobalAgency_CannotBeGranted(t *testing.T) {
	h, pool := gateServer(t)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/access-grants", gRoot,
		`{"adGroup":"platform","role":"admin","agencyId":"global","allScopes":false}`)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("granting on Global = %d, want a 4xx refusal (%s)", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPost, "/api/v1/service-accounts", gRoot,
		`{"name":"platform-bot","role":"operator","agencyId":"global","allScopes":false}`)
	if rec.Code < 400 || rec.Code >= 500 {
		t.Errorf("a service account on Global = %d, want a 4xx refusal (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM access_grants WHERE agency_id='global')
	                             + (SELECT COUNT(*) FROM service_accounts WHERE agency_id='global')`); n != 0 {
		t.Fatalf("%d grants or service accounts name Global", n)
	}
}

// LR-22, LR-25, LR-26 — what a global administrator creates without naming an
// agency was Global's until 2.5.3, and is refused since (CA-4: Central runs
// nothing); naming a department moves a Global row out of Global, and nothing
// moves back in (CA-4); and no write leaves a row in both, or in neither.
func TestGlobalAgency_ARowIsInGlobalOrADepartmentNeverBothNeverNeither(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gRoot, `{"key":"SHARED_TOKEN","source":"stored","value":"v"}`)
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
		t.Fatalf("root creating a secret naming no agency = %d (%s), want 422 central_runs_nothing", rec.Code, rec.Body)
	}
	// Global's secret is an upgrade's leftover, written as one (born with no
	// owner, it is Global's).
	exec(`INSERT INTO secrets (id, key, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('s-shared', 'SHARED_TOKEN', 'stored', 't', 't', 't', 't')`)
	sec := struct{ ID string }{"s-shared"}
	agenciesOf := func(table, col, id string) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(`SELECT COALESCE(group_concat(agency_id, ','), '') FROM (SELECT agency_id FROM `+table+` WHERE `+col+` = ? ORDER BY agency_id)`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := agenciesOf("secret_agencies", "secret_id", sec.ID); got != "global" {
		t.Fatalf("a global administrator's new secret is in %q, want global", got)
	}
	var owner string
	_ = pool.QueryRow(`SELECT owner_agency FROM secrets WHERE id = ?`, sec.ID).Scan(&owner)
	if owner != "global" {
		t.Fatalf("its owner is %q, want global", owner)
	}

	exec(`INSERT INTO runners (id,name,status,registered_at,created_at) VALUES ('r1','r1','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)

	// One body per membership route, for a row that is Global's today.
	routes := []struct {
		kind, path, id, table, col string
	}{
		{"secret", "/api/v1/secret-agencies", sec.ID, "secret_agencies", "secret_id"},
		{"scope", "/api/v1/scope-agencies", "sc:shared", "scope_agencies", "scope_id"},
		{"runner", "/api/v1/runner-agencies", "r1", "runner_agencies", "runner_id"},
	}
	// A runner obeys the same two rules and one more (MA-11, Phase G3): it is
	// not moved between agencies at all, so the move below is asserted for the
	// secret and the scope, and refused for the runner.
	body := func(kind, id, agencies string) string {
		if kind == "runner" {
			return asRepair(`[{"runnerId":"` + id + `","agencyIds":` + agencies + `}]`)
		}
		return asRepair(`[{"id":"` + id + `","agencyIds":` + agencies + `}]`)
	}
	for _, rt := range routes {
		if got := agenciesOf(rt.table, rt.col, rt.id); got != "global" {
			t.Fatalf("%s %s starts in %q, want global", rt.kind, rt.id, got)
		}
		// Neither: an empty set is refused, and the row keeps what it had.
		rec := gateReq(t, h, http.MethodPut, rt.path, gRoot, body(rt.kind, rt.id, `[]`))
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "agency_required" {
			t.Errorf("%s: an empty agency set = %d %s, want 422 agency_required (%s)", rt.kind, rec.Code, errCode(rec.Body.Bytes()), rec.Body)
		}
		// Both: Global beside a department is refused.
		rec = gateReq(t, h, http.MethodPut, rt.path, gRoot, body(rt.kind, rt.id, `["global","ag:FIN"]`))
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "global_mixed" {
			t.Errorf("%s: Global with a department = %d %s, want 422 global_mixed (%s)", rt.kind, rec.Code, errCode(rec.Body.Bytes()), rec.Body)
		}
		if got := agenciesOf(rt.table, rt.col, rt.id); got != "global" {
			t.Fatalf("%s: a refused write changed the row to %q", rt.kind, got)
		}
		// Naming a department moves it out of Global.
		rec = gateReq(t, h, http.MethodPut, rt.path, gRoot, body(rt.kind, rt.id, `["ag:FIN"]`))
		if rt.kind == "runner" {
			if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "serve_list_fixed" {
				t.Errorf("runner: moving Global's agent to FIN = %d %s, want 422 serve_list_fixed (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
			}
			if got := agenciesOf(rt.table, rt.col, rt.id); got != "global" {
				t.Errorf("runner: a refused move changed the row to %q", got)
			}
			continue
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: moving to FIN = %d (%s)", rt.kind, rec.Code, rec.Body)
		}
		if got := agenciesOf(rt.table, rt.col, rt.id); got != "ag:FIN" {
			t.Errorf("%s: after moving to FIN it is in %q, want ag:FIN alone", rt.kind, got)
		}
		// And not back, even by a global administrator: until 2.5.3 it moved
		// back; since CA-4 nothing moves into Central.
		rec = gateReq(t, h, http.MethodPut, rt.path, gRoot, body(rt.kind, rt.id, `["global"]`))
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
			t.Fatalf("%s: moving back to Global = %d (%s), want 422 central_runs_nothing", rt.kind, rec.Code, rec.Body)
		}
		if got := agenciesOf(rt.table, rt.col, rt.id); got != "ag:FIN" {
			t.Errorf("%s: after the refused move back it is in %q, want ag:FIN", rt.kind, got)
		}
	}

	// The agency-shaped editor obeys the same two rules.
	//
	// Removing FIN's only hold on a secret would leave it in no agency. The
	// owner rule speaks first when FIN owns the row, so the row under test is
	// one FIN holds and does not own: Global's, held in FIN (a pre-2.3.0 shape).
	// Until 2.5.3 the test made the secret above so by taking its owner away;
	// since then the database refuses to make FIN's row Central's (1670), so
	// it is written as an upgrade leaves it.
	exec(`INSERT INTO secrets (id, key, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('s-held', 'HELD_TOKEN', 'stored', 't', 't', 't', 't')`)
	exec(`DELETE FROM secret_agencies WHERE secret_id = 's-held'`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('s-held', 'ag:FIN')`)
	sec.ID = "s-held"
	rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot, asRepair(`{"members":[{"kind":"scope","id":"sc:fin"}]}`))
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "agency_required" {
		t.Errorf("emptying a secret's agencies through the agency editor = %d %s, want 422 agency_required (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if got := agenciesOf("secret_agencies", "secret_id", sec.ID); got != "ag:FIN" {
		t.Errorf("the refused save left the secret in %q", got)
	}
	// Adding a department's row to Global through Global's own editor is the
	// mixed write by another door.
	// (Global's list as it stands — r1, and the local runner, which is Global's
	// from the first start; the shared scope is FIN's since the moves above,
	// which do not move back since 2.5.3 — plus the row under test.)
	var localID string
	if err := pool.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&localID); err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/global/members", gRoot,
		asRepair(`{"members":[{"kind":"runner","id":"r1"},{"kind":"runner","id":"`+localID+`"},{"kind":"secret","id":"`+sec.ID+`"}]}`))
	// Until 2.5.3 the answer was 422 global_mixed; since CA-4 the move into
	// Central is refused before that is asked.
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
		t.Errorf("adding FIN's secret to Global = %d %s, want 422 central_runs_nothing (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if got := agenciesOf("secret_agencies", "secret_id", sec.ID); got != "ag:FIN" {
		t.Errorf("the refused save left the secret in %q", got)
	}
}

// LR-26 — making something Global was a global administrator's decision (since
// 2.5.3 nobody's: CA-4). A department's administrator can neither move their
// row into Global nor take a Global row for themselves.
func TestGlobalAgency_OnlyAGlobalAdministratorMovesARowInOrOut(t *testing.T) {
	h, pool := gateServer(t)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a secret = %d (%s)", rec.Code, rec.Body)
	}
	var fin struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &fin)
	rec = gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gFinAdmin, `[{"id":"`+fin.ID+`","agencyIds":["global"]}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin moving their secret into Global = %d, want 403 (%s)", rec.Code, rec.Body)
	}

	// A Global secret: since 2.5.3 (CA-4) nobody creates one, so it is an
	// upgrade's leftover, written as one (born with no owner, it is Global's).
	exec := mustExec(t, pool)
	exec(`INSERT INTO secrets (id, key, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('s-shared', 'SHARED_TOKEN', 'stored', 't', 't', 't', 't')`)
	shared := struct{ ID string }{"s-shared"}
	rec = gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gFinAdmin, `[{"id":"`+shared.ID+`","agencyIds":["ag:FIN"]}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("fin admin taking a Global secret = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	// An all-scopes VIEWER who administers one agency is not a global
	// administrator either (the GC-1 distinction, on the new agency).
	rec = gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gMixed, `[{"id":"`+shared.ID+`","agencyIds":["ag:FIN"]}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("an all-scopes viewer + fin admin taking a Global secret = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secret_agencies WHERE (secret_id = ? AND agency_id = 'global') OR (secret_id = ? AND agency_id = 'ag:FIN')`, shared.ID, fin.ID); n != 2 {
		t.Fatalf("a refused move changed a row's agency (%d of 2 rows intact)", n)
	}
}

// LR-22 — "no membership rows" used to MEAN global. It means nothing now: the
// database gives every row Global's until it has another, so a row with none is
// damage, and the gate says so instead of reading it as shared (which would
// hand a department's orphaned secret to whoever asked as a global row) or as
// absent (which would hide it).
func TestGlobalAgency_ARowInNoAgencyIsAFaultNotGlobal(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin, `{"key":"FIN_TOKEN","source":"stored","scope":"fin-hosts","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("fin admin creating a secret = %d (%s)", rec.Code, rec.Body)
	}
	var sec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
	exec(`DELETE FROM secret_agencies WHERE secret_id = ?`, sec.ID)

	for _, who := range []string{gRoot, gFinAdmin} {
		rec := gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/"+sec.ID, who, ``)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s deleting a secret that is in no agency = %d, want 500 (%s)", who, rec.Code, rec.Body)
		}
	}
	// Someone who cannot see the row's scope is told what they are always told:
	// the fault is not an oracle for a secret's existence.
	rec = gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/"+sec.ID, gTaxAdmin, ``)
	if rec.Code != http.StatusNotFound {
		t.Errorf("tax admin deleting it = %d, want the 404 a healthy fin secret gives them (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id = ?`, sec.ID); n != 1 {
		t.Fatal("the secret was deleted through a gate that could not say whose it was")
	}
	// A row that does not exist is still an ordinary not-found, not a fault.
	rec = gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/no-such-secret", gRoot, ``)
	if rec.Code == http.StatusInternalServerError {
		t.Errorf("deleting a secret that does not exist = 500 (%s)", rec.Body)
	}
}

// RB-24, LR-24 — the Jobs list tells each caller what the three buttons will do
// for THEM, by each route's own rule. A job with no scope is Global's: a
// department may run it against its own scope and stop the run it started, and
// may not pause it for everyone or stop Global's unbound run.
func TestGlobalAgency_JobRowFlagsAnswerEachRoutesOwnQuestion(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO jobs (name,source,run_type,scope,enabled,created_at) VALUES
	      ('platform-idle','cronomicon','bash',NULL,1,'2026-01-01T00:00:00Z'),
	      ('platform-unbound-run','cronomicon','bash',NULL,1,'2026-01-01T00:00:00Z'),
	      ('platform-fin-run','cronomicon','bash',NULL,1,'2026-01-01T00:00:00Z'),
	      ('fin-job','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z'),
	      ('fin-job-tax-run','cronomicon','bash','fin-hosts',1,'2026-01-01T00:00:00Z')`)
	run := func(job string, scope any) {
		// The run is its scope's agency's, as a producer snapshots it (I2).
		exec(`INSERT INTO runs (id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, executor, created_at, agencies_json)
		      VALUES (?, ?, 'cronomicon', 'bash', ?, 'running', 'u', 'manual', 'runner', '2026-01-02T00:00:00Z',
		              COALESCE((SELECT json_array(a.name) FROM scopes sc JOIN agencies a ON a.id = sc.owner_agency WHERE sc.name = ?), '["Global"]'))`,
			"run-"+job, job, scope, scope)
	}
	run("platform-unbound-run", nil)     // Global's own run of a Global job
	run("platform-fin-run", "fin-hosts") // FIN ran the Global job against its scope
	run("fin-job-tax-run", "tax-hosts")  // a FIN job run with a per-run scope override

	type flags struct{ run, kill, pause bool }
	list := func(who string) map[string]flags {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/jobs?pageSize=50", who, ``)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s listing jobs = %d (%s)", who, rec.Code, rec.Body)
		}
		var page struct {
			Items []struct {
				Name     string `json:"name"`
				CanRun   bool   `json:"canRun"`
				CanKill  bool   `json:"canKill"`
				CanPause bool   `json:"canPause"`
			} `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		out := map[string]flags{}
		for _, it := range page.Items {
			out[it.Name] = flags{it.CanRun, it.CanKill, it.CanPause}
		}
		return out
	}
	for _, c := range []struct {
		who  string
		want map[string]flags
	}{
		// Since 2.5.3 (CA-1) Global's jobs are leftovers the run route refuses
		// for everyone: no Run, while stopping a run already going and pausing
		// are answered as before.
		{gRoot, map[string]flags{
			"platform-idle": {false, true, true}, "platform-unbound-run": {false, true, true},
			"platform-fin-run": {false, true, true}, "fin-job": {true, true, true}, "fin-job-tax-run": {true, true, true},
		}},
		// Global's jobs are not listed to FIN's people since 2.5.0 C2 (question
		// 77): only to those who reach every agency (gMixed below).
		{gFinOperator, map[string]flags{
			"fin-job":         {true, true, true},
			"fin-job-tax-run": {true, false, true}, // the active run is in TAX's scope
		}},
		{gFinViewer, map[string]flags{
			"fin-job": {}, "fin-job-tax-run": {},
		}},
		// Reaches every scope, holds no verb: nothing — and the all-scopes viewer
		// who administers FIN is FIN's administrator, not Global's.
		{gAllViewer, map[string]flags{
			"platform-idle": {}, "platform-unbound-run": {}, "platform-fin-run": {}, "fin-job": {}, "fin-job-tax-run": {},
		}},
		// Reads Global's jobs (it reaches every agency) but may not run them: a
		// job with no scope runs against its own agency's scopes, and Global's
		// are a global administrator's (2.5.0 C2, question 78, LR-24 retired).
		// FIN's own run of one is still FIN's to stop.
		{gMixed, map[string]flags{
			"platform-idle": {false, false, false}, "platform-unbound-run": {false, false, false},
			"platform-fin-run": {false, true, false}, "fin-job": {true, true, true}, "fin-job-tax-run": {true, false, true},
		}},
	} {
		got := list(c.who)
		for name, want := range c.want {
			if g, ok := got[name]; !ok {
				t.Errorf("%s does not see %s", c.who, name)
			} else if g != want {
				t.Errorf("%s on %s: run/kill/pause = %v, want %v", c.who, name, g, want)
			}
		}
		if c.who == gFinOperator || c.who == gFinViewer {
			for _, name := range []string{"platform-idle", "platform-unbound-run", "platform-fin-run"} {
				if _, ok := got[name]; ok {
					t.Errorf("%s sees Global's %s (question 77)", c.who, name)
				}
			}
		}
	}

	// The flags are a promise about the routes. Keep them honest on the two rows
	// where they differ from each other.
	id := func(name string) string { return rowID(t, pool, `SELECT rowid FROM jobs WHERE name=?`, name) }
	// Global's jobs are absent to FIN's people (question 77), so their writes
	// answer 404, not a 403 that says the job is there (2.5.0 C11, question 109).
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-fin-run")+"/pause", gFinOperator, `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("fin operator pausing Global's job with no scope = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-unbound-run")+"/kill", gFinOperator, `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("fin operator stopping Global's unbound run = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("fin-job-tax-run")+"/kill", gFinOperator, `{}`); rec.Code/100 == 2 {
		t.Errorf("fin operator stopping a run in TAX's scope = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-fin-run")+"/kill", gFinOperator, `{}`); rec.Code/100 != 2 {
		t.Errorf("fin operator stopping their own run of a job with no scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}

	// LR-24 retired (2.5.0 C2, question 78): Global's job with no scope is not
	// run against an agency's scope, by the agency (to which it is absent) nor by
	// a global administrator (IN-3: Global's jobs run on Global's hosts). Until
	// 2.5.3 a global administrator still ran it unbound; since CA-1 Global's job
	// runs nothing, and that run is refused too.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-idle")+"/run", gFinOperator, `{"scope":"fin-hosts"}`); rec.Code != http.StatusNotFound {
		t.Errorf("fin operator running Global's job against FIN's scope = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-idle")+"/run", gRoot, `{"scope":"fin-hosts"}`); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "scope_not_in_agency") {
		t.Errorf("a global administrator running Global's job against FIN's scope = %d, want 422 scope_not_in_agency (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-idle")+"/run", gRoot, `{}`); rec.Code != http.StatusConflict ||
		errCode(rec.Body.Bytes()) != "run_refused" || !strings.Contains(rec.Body.String(), "runs nothing") {
		t.Errorf("a global administrator running Global's job unbound = %d, want 409 run_refused (Central runs nothing) (%s)", rec.Code, rec.Body)
	}
}

// A row that is in no agency is refused by every gate, which would make it
// permanent: nothing could edit, move or delete it. The membership setters are
// the way out, for a global administrator only — and for nobody else, since a
// department claiming an orphan would be taking a row it cannot be shown to own.
func TestGlobalAgency_AGlobalAdministratorCanReHomeARowInNoAgency(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)

	// A secret Global owns, on FIN's scope, in no agency at all. Until 2.5.3 the
	// test made FIN's own secret so by taking its owner away; since then the
	// database refuses to make FIN's row Central's (1670), so the orphan is
	// written as an upgrade leaves one (born with no owner, it is Global's).
	exec(`INSERT INTO secrets (id, key, source, scope, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('s-orphan', 'FIN_TOKEN', 'stored', 'fin-hosts', 't', 't', 't', 't')`)
	sec := struct{ ID string }{"s-orphan"}
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at) VALUES ('r1','r1','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`DELETE FROM secret_agencies WHERE secret_id = ?`, sec.ID)
	exec(`DELETE FROM runner_agencies WHERE runner_id = 'r1'`)

	secretBody := `[{"id":"` + sec.ID + `","agencyIds":["ag:FIN"]}]`
	// A runner with no serve row still has an OWNER (Global here), and the one
	// list it can be given is that owner (MA-11): the repair puts it back where
	// it belongs and cannot be used to place it somewhere new.
	runnerBody := `[{"runnerId":"r1","agencyIds":["global"]}]`
	// FIN's administrator holds the permission on the TARGET agency, and that is
	// not enough: the row is not shown to be theirs. (A secret in no agency is
	// damage and answers 500 to anyone but a global administrator; a runner
	// always has an owner to ask, so it is a plain 403.)
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gFinAdmin, secretBody); rec.Code != http.StatusInternalServerError {
		t.Errorf("fin admin claiming an orphaned secret = %d, want 500 (%s)", rec.Code, rec.Body)
	}
	for _, body := range []string{runnerBody, `[{"runnerId":"r1","agencyIds":["ag:FIN"]}]`} {
		if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gFinAdmin, body); rec.Code != http.StatusForbidden {
			t.Errorf("fin admin claiming an orphaned runner with %s = %d, want 403 (%s)", body, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM secret_agencies WHERE secret_id = ?) + (SELECT COUNT(*) FROM runner_agencies WHERE runner_id = 'r1')`, sec.ID); n != 0 {
		t.Fatalf("a refused claim wrote %d membership rows", n)
	}
	// Nor can a global administrator use the repair to move it.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gRoot, asRepair(`[{"runnerId":"r1","agencyIds":["ag:FIN"]}]`)); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "serve_list_fixed" {
		t.Errorf("root giving an orphaned Global runner to FIN = %d %s, want 422 serve_list_fixed (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	for path, body := range map[string]string{"/api/v1/secret-agencies": secretBody, "/api/v1/runner-agencies": runnerBody} {
		if rec := gateReq(t, h, http.MethodPut, path, gRoot, asRepair(body)); rec.Code != http.StatusOK {
			t.Errorf("root re-homing an orphan through %s = %d, want 200 (%s)", path, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM secret_agencies WHERE secret_id = ? AND agency_id = 'ag:FIN')
	                             + (SELECT COUNT(*) FROM runner_agencies WHERE runner_id = 'r1' AND agency_id = 'global')`, sec.ID); n != 2 {
		t.Fatalf("%d of 2 orphans were re-homed", n)
	}
	// And the row works again: its agency's administrator may delete it.
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/"+sec.ID, gFinAdmin, ``); rec.Code/100 != 2 {
		t.Errorf("fin admin deleting the re-homed secret = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
}

// An agency that still holds anything cannot be deleted: the membership tables
// cascade from it, and a row whose only agency that was would be left in none.
func TestGlobalAgency_AnAgencyInUseIsNotDeleted(t *testing.T) {
	h, pool := gateServer(t)
	rec := gateReq(t, h, http.MethodDelete, "/api/v1/agencies/ag:FIN", gRoot, ``)
	if rec.Code != http.StatusConflict {
		t.Fatalf("deleting an agency that owns a scope = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	// The database refuses it too, for a delete that does not come through the
	// guard (the guard is a count followed by a delete, not one statement).
	if _, err := pool.Exec(`DELETE FROM agencies WHERE id = 'ag:FIN'`); err == nil {
		t.Fatal("a raw delete of an agency that owns a scope succeeded")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_agencies WHERE scope_id = 'sc:fin' AND agency_id = 'ag:FIN'`); n != 1 {
		t.Fatal("the scope lost its agency")
	}
}
