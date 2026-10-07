package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Global is a real agency since migration 1220 (LR-8, LR-21). These pin the
// rules that make it one the installation can rely on: it is always there, it
// is nobody's to grant, a row is in it or in a department and never both, and
// a row that is in no agency at all is a fault, not a meaning.

// LR-21, LR-27 — the built-in agency cannot be renamed, deleted or imitated,
// by anyone.
func TestGlobalAgency_IsPermanentAndItsNameIsReserved(t *testing.T) {
	h, pool := gateServer(t)
	for _, c := range []struct {
		name, method, path, body, code string
	}{
		{"rename Global", http.MethodPut, "/api/v1/agencies/global", `{"name":"Everyone"}`, "builtin_agency"},
		{"delete Global", http.MethodDelete, "/api/v1/agencies/global", ``, "builtin_agency"},
		{"create another Global", http.MethodPost, "/api/v1/agencies", `{"name":"gLOBAL"}`, "name_reserved"},
		{"rename an agency to Global", http.MethodPut, "/api/v1/agencies/ag:FIN", `{"name":"global"}`, "name_reserved"},
	} {
		rec := gateReq(t, h, c.method, c.path, gRoot, c.body)
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != c.code {
			t.Errorf("%s = %d %s, want 422 %s (%s)", c.name, rec.Code, errCode(rec.Body.Bytes()), c.code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agencies WHERE id='global' AND name='Global' AND builtin=1`); n != 1 {
		t.Fatal("the Global row did not survive")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agencies WHERE lower(name)='global'`); n != 1 {
		t.Fatalf("%d agencies are called Global", n)
	}
	// Its description is an ordinary field, for a global administrator.
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/global", gRoot, `{"name":"Global","description":"the platform team"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("describing Global = %d, want 200 (%s)", rec.Code, rec.Body)
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
// agency is Global's; naming a department moves it out of Global; and no write
// leaves a row in both, or in neither.
func TestGlobalAgency_ARowIsInGlobalOrADepartmentNeverBothNeverNeither(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)

	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gRoot, `{"key":"SHARED_TOKEN","source":"stored","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("root creating a secret = %d (%s)", rec.Code, rec.Body)
	}
	var sec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
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
			return `[{"runnerId":"` + id + `","agencyIds":` + agencies + `}]`
		}
		return `[{"id":"` + id + `","agencyIds":` + agencies + `}]`
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
		// And back, by a global administrator.
		rec = gateReq(t, h, http.MethodPut, rt.path, gRoot, body(rt.kind, rt.id, `["global"]`))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: moving back to Global = %d (%s)", rt.kind, rec.Code, rec.Body)
		}
		if got := agenciesOf(rt.table, rt.col, rt.id); got != "global" {
			t.Errorf("%s: after moving back it is in %q, want global", rt.kind, got)
		}
	}

	// The agency-shaped editor obeys the same two rules.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gRoot, body("secret", sec.ID, `["ag:FIN"]`))
	if rec.Code != http.StatusOK {
		t.Fatalf("moving the secret to FIN = %d (%s)", rec.Code, rec.Body)
	}
	// Removing FIN's only hold on the secret would leave it in no agency. The
	// owner rule speaks first when FIN owns the row, so take the owner away to
	// reach the rule under test.
	exec(`UPDATE secrets SET owner_agency = 'global' WHERE id = ?`, sec.ID)
	rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot, `{"members":[{"kind":"scope","id":"sc:fin"}]}`)
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "agency_required" {
		t.Errorf("emptying a secret's agencies through the agency editor = %d %s, want 422 agency_required (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if got := agenciesOf("secret_agencies", "secret_id", sec.ID); got != "ag:FIN" {
		t.Errorf("the refused save left the secret in %q", got)
	}
	// Adding a department's row to Global through Global's own editor is the
	// mixed write by another door.
	rec = gateReq(t, h, http.MethodPut, "/api/v1/agencies/global/members", gRoot,
		`{"members":[{"kind":"scope","id":"sc:shared"},{"kind":"runner","id":"r1"},{"kind":"secret","id":"`+sec.ID+`"}]}`)
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "global_mixed" {
		t.Errorf("adding FIN's secret to Global = %d %s, want 422 global_mixed (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
}

// LR-26 — making something Global is a global administrator's decision: a
// Global secret is usable by every agency's runs. A department's administrator
// can neither move their row into Global nor take a Global row for themselves.
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

	rec = gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gRoot, `{"key":"SHARED_TOKEN","source":"stored","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("root creating a secret = %d (%s)", rec.Code, rec.Body)
	}
	var shared struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &shared)
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
		exec(`INSERT INTO runs (id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
		      VALUES (?, ?, 'cronomicon', 'bash', ?, 'running', 'u', 'manual', 'runner', '2026-01-02T00:00:00Z')`, "run-"+job, job, scope)
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
		{gRoot, map[string]flags{
			"platform-idle": {true, true, true}, "platform-unbound-run": {true, true, true},
			"platform-fin-run": {true, true, true}, "fin-job": {true, true, true}, "fin-job-tax-run": {true, true, true},
		}},
		{gFinOperator, map[string]flags{
			"platform-idle":        {true, false, false}, // run it against FIN's scope; not theirs to pause
			"platform-unbound-run": {true, false, false}, // Global's run is not theirs to stop
			"platform-fin-run":     {true, true, false},  // their own run of it is
			"fin-job":              {true, true, true},
			"fin-job-tax-run":      {true, false, true}, // the active run is in TAX's scope
		}},
		{gFinViewer, map[string]flags{
			"platform-idle": {}, "platform-unbound-run": {}, "platform-fin-run": {}, "fin-job": {}, "fin-job-tax-run": {},
		}},
		// Reaches every scope, holds no verb: nothing — and the all-scopes viewer
		// who administers FIN is FIN's administrator, not Global's.
		{gAllViewer, map[string]flags{
			"platform-idle": {}, "platform-unbound-run": {}, "platform-fin-run": {}, "fin-job": {}, "fin-job-tax-run": {},
		}},
		{gMixed, map[string]flags{
			"platform-idle": {true, false, false}, "platform-unbound-run": {true, false, false},
			"platform-fin-run": {true, true, false}, "fin-job": {true, true, true}, "fin-job-tax-run": {true, false, true},
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
	}

	// The flags are a promise about the routes. Keep them honest on the two rows
	// where they differ from each other.
	id := func(name string) string { return rowID(t, pool, `SELECT rowid FROM jobs WHERE name=?`, name) }
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-fin-run")+"/pause", gFinOperator, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("fin operator pausing a job with no scope = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-unbound-run")+"/kill", gFinOperator, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("fin operator stopping Global's unbound run = %d, want 403 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("fin-job-tax-run")+"/kill", gFinOperator, `{}`); rec.Code/100 == 2 {
		t.Errorf("fin operator stopping a run in TAX's scope = %d, want a refusal (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+id("platform-fin-run")+"/kill", gFinOperator, `{}`); rec.Code/100 != 2 {
		t.Errorf("fin operator stopping their own run of a job with no scope = %d, want 2xx (%s)", rec.Code, rec.Body)
	}
}

// A row that is in no agency is refused by every gate, which would make it
// permanent: nothing could edit, move or delete it. The membership setters are
// the way out, for a global administrator only — and for nobody else, since a
// department claiming an orphan would be taking a row it cannot be shown to own.
func TestGlobalAgency_AGlobalAdministratorCanReHomeARowInNoAgency(t *testing.T) {
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
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at) VALUES ('r1','r1','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`UPDATE secrets SET owner_agency = 'global' WHERE id = ?`, sec.ID)
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
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gRoot, `[{"runnerId":"r1","agencyIds":["ag:FIN"]}]`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "serve_list_fixed" {
		t.Errorf("root giving an orphaned Global runner to FIN = %d %s, want 422 serve_list_fixed (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	for path, body := range map[string]string{"/api/v1/secret-agencies": secretBody, "/api/v1/runner-agencies": runnerBody} {
		if rec := gateReq(t, h, http.MethodPut, path, gRoot, body); rec.Code != http.StatusOK {
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
