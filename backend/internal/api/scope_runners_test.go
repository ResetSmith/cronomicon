package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// SB-1 — the scope↔runner binding routes (the scope-bound-runners plan, mig.
// 1180). What a binding does at dispatch is pinned in internal/runner and
// internal/execspec; these tests are about the write contract: what may be
// bound, by whom, and what the operator is told when it may not.

type boundRunnerJSON struct {
	RunnerID   string `json:"runnerId"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	Status     string `json:"status"`
	Eligible   bool   `json:"eligible"`
}

type scopeJSON struct {
	ID           string            `json:"id"`
	Scope        string            `json:"scope"`
	BoundRunners []boundRunnerJSON `json:"boundRunners"`
}

func mustExec(t *testing.T, pool *sql.DB) func(q string, args ...any) {
	return func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
}

func seedBindingRunner(exec func(string, ...any), id, name, agency string) {
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES (?,?,'online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, id, name)
	if agency != "" {
		exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?,?)`, id, agency)
	}
}

func decodeScope(t *testing.T, body []byte) scopeJSON {
	t.Helper()
	var sc scopeJSON
	if err := json.Unmarshal(body, &sc); err != nil {
		t.Fatalf("decode scope: %v\n%s", err, body)
	}
	return sc
}

func errCode(body []byte) string {
	var e struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Code != "" {
		return e.Code
	}
	return e.Error.Code
}

// TestSetScopeRunners walks the write contract as an unrestricted operator.
func TestSetScopeRunners(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-dmz','dmz-web','git','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s-dmz','ag-fin')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-pool','shared','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-plain','plain','cronomicon','2026-01-01T00:00:00Z')`)
	seedBindingRunner(exec, "r-fin", "runner-fin-01", "ag-fin")
	seedBindingRunner(exec, "r-fin2", "runner-fin-02", "ag-fin")
	seedBindingRunner(exec, "r-tax", "runner-tax-01", "ag-tax")
	seedBindingRunner(exec, "r-pool", "runner-pool-01", "")

	put := func(scopeID, body string) (int, []byte) {
		rec := reqAs(t, h, http.MethodPut, "/api/v1/scopes/"+scopeID+"/runners", "sec-admins", body)
		return rec.Code, rec.Body.Bytes()
	}

	// Bind a member runner. Works on a git-source scope: the binding is an
	// operator overlay, like the agency.
	code, body := put("s-dmz", `{"runnerIds":["r-fin"]}`)
	if code != http.StatusOK {
		t.Fatalf("bind = %d, want 200 (%s)", code, body)
	}
	sc := decodeScope(t, body)
	if len(sc.BoundRunners) != 1 || sc.BoundRunners[0].RunnerID != "r-fin" ||
		sc.BoundRunners[0].Name != "runner-fin-01" || !sc.BoundRunners[0].Registered || !sc.BoundRunners[0].Eligible {
		t.Errorf("boundRunners = %+v, want the registered, eligible runner-fin-01", sc.BoundRunners)
	}

	// Refusals: each names its own cause, and none of them changes the binding.
	for _, c := range []struct{ name, scope, body, want string }{
		{"a runner from another agency", "s-dmz", `{"runnerIds":["r-fin","r-tax"]}`, "runner_not_eligible"},
		{"a general-pool runner on an agency scope", "s-dmz", `{"runnerIds":["r-fin","r-pool"]}`, "runner_not_eligible"},
		{"an agency runner on a general-pool scope", "s-pool", `{"runnerIds":["r-fin"]}`, "runner_not_eligible"},
		{"a runner that does not exist", "s-dmz", `{"runnerIds":["r-fin","nope"]}`, "unknown_runner"},
	} {
		code, body := put(c.scope, c.body)
		if code != http.StatusUnprocessableEntity || errCode(body) != c.want {
			t.Errorf("%s = %d %q, want 422 %s (%s)", c.name, code, errCode(body), c.want, body)
		}
	}
	var n int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners`).Scan(&n); err != nil || n != 1 {
		t.Errorf("binding rows after the refusals = %d (err %v), want the one accepted binding", n, err)
	}
	if code, _ := put("no-such-scope", `{"runnerIds":[]}`); code != http.StatusNotFound {
		t.Errorf("unknown scope = %d, want 404", code)
	}
	// [] is the operation that REMOVES the restriction, so a body that merely
	// fails to say runnerIds must be refused, never read as "clear".
	for _, body := range []string{`{}`, `{"runnerIds":null}`, `{"runner_ids":["r-fin"]}`} {
		code, resp := put("s-dmz", body)
		if code != http.StatusUnprocessableEntity || errCode(resp) != "validation_error" {
			t.Errorf("body %s = %d %q, want 422 validation_error (%s)", body, code, errCode(resp), resp)
		}
	}
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE scope_id='s-dmz'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("s-dmz binding rows after the malformed bodies = %d (err %v), want it still bound", n, err)
	}
	if code, body := put("s-pool", `{"runnerIds":["r-pool"]}`); code != http.StatusOK {
		t.Errorf("general-pool runner on a general-pool scope = %d, want 200 (%s)", code, body)
	}

	// A binding outlives its runner, and a save must be able to keep the ghost
	// while adding its replacement — re-validating it would make that impossible.
	exec(`DELETE FROM runners WHERE id = 'r-fin'`)
	code, body = put("s-dmz", `{"runnerIds":["r-fin","r-fin2"]}`)
	if code != http.StatusOK {
		t.Fatalf("keep the ghost and add a replacement = %d, want 200 (%s)", code, body)
	}
	sc = decodeScope(t, body)
	if len(sc.BoundRunners) != 2 {
		t.Fatalf("boundRunners = %+v, want the replacement and the ghost", sc.BoundRunners)
	}
	live, ghost := sc.BoundRunners[0], sc.BoundRunners[1] // registered first
	if live.RunnerID != "r-fin2" || !live.Registered || !live.Eligible {
		t.Errorf("first bound runner = %+v, want the registered replacement", live)
	}
	if ghost.RunnerID != "r-fin" || ghost.Registered || ghost.Eligible || ghost.Name != "runner-fin-01" {
		t.Errorf("ghost = %+v, want r-fin unregistered, ineligible, under the name it was bound with", ghost)
	}

	// The list carries the bindings too, and an untouched scope has [] not null.
	rec := reqAs(t, h, http.MethodGet, "/api/v1/scopes", "sec-admins", "")
	var list []scopeJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var raw []map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	for _, row := range raw {
		if string(row["id"]) == `"s-plain"` && string(row["boundRunners"]) != "[]" {
			t.Errorf("an unbound scope lists boundRunners = %s, want []", row["boundRunners"])
		}
	}
	byID := map[string]scopeJSON{}
	for _, s := range list {
		byID[s.ID] = s
	}
	if _, ok := byID["s-plain"]; !ok {
		t.Fatalf("the list does not include the unbound scope: %s", rec.Body.String())
	}
	if len(byID["s-dmz"].BoundRunners) != 2 || len(byID["s-pool"].BoundRunners) != 1 {
		t.Errorf("list bindings: dmz=%+v pool=%+v", byID["s-dmz"].BoundRunners, byID["s-pool"].BoundRunners)
	}

	// Clearing returns the scope to unrestricted.
	code, body = put("s-dmz", `{"runnerIds":[]}`)
	if code != http.StatusOK || len(decodeScope(t, body).BoundRunners) != 0 {
		t.Errorf("clear = %d %s, want 200 with no bound runners", code, body)
	}
	var audited int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'runners-set'`).Scan(&audited); err != nil || audited == 0 {
		t.Errorf("change_log rows for the binding writes = %d (err %v), want some", audited, err)
	}
}

// TestBindingARunnerIsDepartmental — naming a runner on a scope is placing it,
// so it carries the RF-2 runner gate: configureApp on an agency the runner
// belongs to, unrestricted for the general pool. Removing a binding names no
// runner and needs only the route's ConfigureApp gate.
func TestBindingARunnerIsDepartmental(t *testing.T) {
	h, pool := secretRBACServer(t, map[string]string{"admin": "prod"})
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-fin','fin-hosts','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s-fin','ag-fin')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-pool','shared','cronomicon','2026-01-01T00:00:00Z')`)
	seedBindingRunner(exec, "r-own", "runner-own", "ag:prod")
	seedBindingRunner(exec, "r-fin", "runner-fin", "ag-fin")
	seedBindingRunner(exec, "r-pool", "runner-pool", "")

	put := func(scopeID, body string) int {
		return reqAs(t, h, http.MethodPut, "/api/v1/scopes/"+scopeID+"/runners", "sec-admins", body).Code
	}
	// sc:prod is the scope the fixture put in the admin's own agency.
	if code := put("sc:prod", `{"runnerIds":["r-own"]}`); code != http.StatusOK {
		t.Errorf("binding an own-agency runner = %d, want 200", code)
	}
	if code := put("s-fin", `{"runnerIds":["r-fin"]}`); code != http.StatusForbidden {
		t.Errorf("binding another department's runner = %d, want 403", code)
	}
	if code := put("s-pool", `{"runnerIds":["r-pool"]}`); code != http.StatusForbidden {
		t.Errorf("restricted binding of a GENERAL-POOL runner = %d, want 403 (RF-Q3)", code)
	}
	// An id that is not a runner must not become an oracle or a bypass: for a
	// restricted caller it reads as unowned, which is unrestricted-only.
	if code := put("sc:prod", `{"runnerIds":["r-own","nope"]}`); code != http.StatusForbidden {
		t.Errorf("restricted binding of an unknown runner id = %d, want 403", code)
	}
	if code := put("sc:prod", `{"runnerIds":[]}`); code != http.StatusOK {
		t.Errorf("clearing a binding = %d, want 200", code)
	}

	// Replace gates the REPLACEMENT: it is the runner being given work.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-fin','r-gone','runner-gone','t')`)
	rec := reqAs(t, h, http.MethodPost, "/api/v1/scope-runners/replace", "sec-admins",
		`{"fromRunnerId":"r-gone","toRunnerId":"r-fin"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("replace onto another department's runner = %d, want 403", rec.Code)
	}

	// ...and passes when the replacement is the caller's own.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('sc:prod','r-gone','runner-gone','t')`)
	rec = reqAs(t, h, http.MethodPost, "/api/v1/scope-runners/replace", "sec-admins",
		`{"fromRunnerId":"r-gone","toRunnerId":"r-own"}`)
	if rec.Code == http.StatusForbidden {
		t.Errorf("replace onto an own-agency runner = 403, want the gate to pass (%s)", rec.Body.String())
	}

	// The whole surface is ConfigureApp: a viewer reaches none of it.
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/scopes/sc:prod/runners", `{"runnerIds":[]}`},
		{http.MethodPost, "/api/v1/scope-runners/replace", `{"fromRunnerId":"a","toRunnerId":"b"}`},
		{http.MethodGet, "/api/v1/scope-binding-notices", ""},
		{http.MethodPost, "/api/v1/scope-binding-notices/dismiss", `{"ids":[1]}`},
	} {
		if rec := reqAs(t, h, c.method, c.path, "sec-viewers", c.body); rec.Code != http.StatusForbidden {
			t.Errorf("viewer %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}
}

// TestReplaceScopeRunner — "this host was replaced", in one step, across every
// scope the old runner served. All or nothing.
func TestReplaceScopeRunner(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-tax','Tax','2026-01-01T00:00:00Z')`)
	for _, s := range [][3]string{{"s-a", "fin-a", "ag-fin"}, {"s-b", "fin-b", "ag-fin"}, {"s-t", "tax-t", "ag-tax"}} {
		exec(`INSERT INTO scopes (id,name,source,created_at) VALUES (?,?,'cronomicon','2026-01-01T00:00:00Z')`, s[0], s[1])
		exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES (?,?)`, s[0], s[2])
	}
	seedBindingRunner(exec, "r-new", "runner-fin-02", "ag-fin")
	// r-old is deregistered — the common case: its bindings are all that is left.
	for _, sid := range []string{"s-a", "s-b"} {
		exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES (?,'r-old','runner-fin-01','t')`, sid)
	}
	// s-b is already bound to the replacement as well; the swap must absorb it.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-b','r-new','runner-fin-02','t')`)

	replace := func(body string) (int, []byte) {
		rec := reqAs(t, h, http.MethodPost, "/api/v1/scope-runners/replace", "sec-admins", body)
		return rec.Code, rec.Body.Bytes()
	}

	// A runner serving a scope the replacement cannot serve: nothing moves, and
	// the refusal names the scope standing in the way.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-t','r-old','runner-fin-01','t')`)
	code, body := replace(`{"fromRunnerId":"r-old","toRunnerId":"r-new"}`)
	if code != http.StatusUnprocessableEntity || errCode(body) != "runner_not_eligible" || !strings.Contains(string(body), "tax-t") {
		t.Fatalf("replace onto an ineligible runner = %d %s, want 422 runner_not_eligible naming tax-t", code, body)
	}
	var moved int
	if err := pool.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-old'`).Scan(&moved); err != nil || moved != 3 {
		t.Fatalf("after a refused replace r-old holds %d bindings (err %v), want all 3 untouched", moved, err)
	}
	exec(`DELETE FROM scope_runners WHERE scope_id='s-t'`)

	code, body = replace(`{"fromRunnerId":"r-old","toRunnerId":"r-new"}`)
	if code != http.StatusOK {
		t.Fatalf("replace = %d, want 200 (%s)", code, body)
	}
	var out struct {
		Scopes []string `json:"scopes"`
	}
	_ = json.Unmarshal(body, &out)
	if len(out.Scopes) != 2 || out.Scopes[0] != "fin-a" || out.Scopes[1] != "fin-b" {
		t.Errorf("scopes = %v, want [fin-a fin-b]", out.Scopes)
	}
	var onOld, onNew int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-old'`).Scan(&onOld)
	_ = pool.QueryRow(`SELECT COUNT(*) FROM scope_runners WHERE runner_id='r-new'`).Scan(&onNew)
	if onOld != 0 || onNew != 2 {
		t.Errorf("after replace: old=%d new=%d, want 0 and 2", onOld, onNew)
	}

	for _, c := range []struct {
		name, body, want string
		code             int
	}{
		{"a runner bound to nothing", `{"fromRunnerId":"r-old","toRunnerId":"r-new"}`, "no_bindings", http.StatusConflict},
		{"an unregistered replacement", `{"fromRunnerId":"r-new","toRunnerId":"nope"}`, "unknown_runner", http.StatusUnprocessableEntity},
		{"the same runner twice", `{"fromRunnerId":"r-new","toRunnerId":"r-new"}`, "validation_error", http.StatusUnprocessableEntity},
	} {
		code, body := replace(c.body)
		if code != c.code || errCode(body) != c.want {
			t.Errorf("%s = %d %q, want %d %s (%s)", c.name, code, errCode(body), c.code, c.want, body)
		}
	}
}

// TestScopeBindingNotices — the pins migration 1180 could not convert stay
// listed until an operator either fixes the cause (binds the scope) or says they
// have seen it (dismiss).
func TestScopeBindingNotices(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-mixed','mixed','cronomicon','2026-01-01T00:00:00Z')`)
	seedBindingRunner(exec, "r-pool", "runner-pool-01", "")
	notice := func(uid, name, scope, reason string) {
		exec(`INSERT INTO retired_runner_pins (job_uid,job_name,job_source,scope,runner_tag,reason,recorded_at)
		      VALUES (?,?,'cronomicon',?,'vlan-dmz',?,'2026-10-05T00:00:00Z')`, uid, name, scope, reason)
	}
	notice("j1", "deploy", "mixed", "mixed_pins")
	notice("j2", "restart", "mixed", "mixed_pins")
	notice("j3", "sweep", "", "no_scope")

	type noticeJSON struct {
		ID      int64  `json:"id"`
		JobName string `json:"jobName"`
		Scope   string `json:"scope"`
		Reason  string `json:"reason"`
	}
	list := func() []noticeJSON {
		t.Helper()
		rec := reqAs(t, h, http.MethodGet, "/api/v1/scope-binding-notices", "sec-admins", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
		}
		var out []noticeJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode notices: %v\n%s", err, rec.Body)
		}
		return out
	}
	if got := list(); len(got) != 3 {
		t.Fatalf("notices = %+v, want all three", got)
	}

	// Binding the scope is the resolution: its notices drop out unprompted.
	if rec := reqAs(t, h, http.MethodPut, "/api/v1/scopes/s-mixed/runners", "sec-admins", `{"runnerIds":["r-pool"]}`); rec.Code != http.StatusOK {
		t.Fatalf("bind = %d (%s)", rec.Code, rec.Body)
	}
	got := list()
	if len(got) != 1 || got[0].JobName != "sweep" || got[0].Reason != "no_scope" {
		t.Fatalf("after binding, notices = %+v, want only the no-scope job", got)
	}

	// A job with no scope has nothing to bind; it goes when dismissed, once.
	body := `{"ids":[` + jsonInt(got[0].ID) + `,999]}`
	rec := reqAs(t, h, http.MethodPost, "/api/v1/scope-binding-notices/dismiss", "sec-admins", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dismissed":1`) {
		t.Errorf("dismiss = %d %s, want 200 with dismissed:1 (the unknown id is not an error)", rec.Code, rec.Body)
	}
	if got := list(); len(got) != 0 {
		t.Errorf("after dismiss, notices = %+v, want none", got)
	}
	rec = reqAs(t, h, http.MethodPost, "/api/v1/scope-binding-notices/dismiss", "sec-admins", body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"dismissed":0`) {
		t.Errorf("second dismiss = %d %s, want 200 with dismissed:0", rec.Code, rec.Body)
	}
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestStuckRunHintNamesTheScopeBinding — the read-time hint on a queued run. A
// run that was claimable when it was queued carries no stored reason; if its
// scope's bound runner is then deregistered, the list must say THAT, not blame
// the run type because some other agency member happens to be online.
func TestStuckRunHintNamesTheScopeBinding(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO agencies (id,name,created_at) VALUES ('ag-fin','Finance','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('s-dmz','dmz-web','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_agencies (scope_id,agency_id) VALUES ('s-dmz','ag-fin')`)
	// An online Finance runner that can run bash, but is not the bound one.
	exec(`INSERT INTO runners (id,name,status,capabilities,registered_at,created_at)
	      VALUES ('r-other','runner-fin-02','online','["bash"]','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-other','ag-fin')`)
	// The bound runner is gone; its binding is not.
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-dmz','r-gone','runner-dmz-01','t')`)
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, scope, status, triggered_by, trigger_kind,
	                        executor, agencies_json, created_at)
	      VALUES ('run-stuck','deploy','git','bash','dmz-web','queued','seed','manual','runner','["Finance"]','2026-10-05T00:00:00Z')`)
	exec(`INSERT INTO run_agencies (run_id, agency) VALUES ('run-stuck','Finance')`)

	rec := reqAs(t, h, http.MethodGet, "/api/v1/runs/run-stuck", "sec-admins", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET run = %d (%s)", rec.Code, rec.Body)
	}
	var run struct {
		StatusReason string `json:"statusReason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v\n%s", err, rec.Body)
	}
	if !strings.Contains(run.StatusReason, "dmz-web") || !strings.Contains(run.StatusReason, "runner-dmz-01") {
		t.Errorf("statusReason = %q, want it to name the scope and its bound runner", run.StatusReason)
	}
	if strings.Contains(run.StatusReason, "run-type") {
		t.Errorf("statusReason = %q blames the run type; an online bash runner exists in the agency", run.StatusReason)
	}
}

// TestBoundScopeBusyIsAConflict — the API face of settings.ErrBoundScopeBusy:
// renaming or deleting a bound scope with runs waiting under its name is a 409
// that says why, not a 500 and not a silent success.
func TestBoundScopeBusyIsAConflict(t *testing.T) {
	h, pool := secretRBACServer(t, nil)
	exec := mustExec(t, pool)
	exec(`INSERT INTO scopes (id,name,source,created_by,created_at,last_modified_by,last_modified_at)
	      VALUES ('s-dmz','dmz','cronomicon','t','2026-01-01T00:00:00Z','t','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO scope_runners (scope_id,runner_id,runner_name,bound_at) VALUES ('s-dmz','r1','runner-1','t')`)
	exec(`INSERT INTO runs (id, job_name, run_type, scope, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run-1','j','bash','dmz','queued','seed','manual','runner','2026-10-05T00:00:00Z')`)

	rec := reqAs(t, h, http.MethodPatch, "/api/v1/scopes/s-dmz", "sec-admins", `{"scope":"dmz-web","hosts":[]}`)
	if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "scope_bound_busy" {
		t.Errorf("rename = %d %q, want 409 scope_bound_busy (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	rec = reqAs(t, h, http.MethodDelete, "/api/v1/scopes/s-dmz", "sec-admins", "")
	if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "scope_bound_busy" {
		t.Errorf("delete = %d %q, want 409 scope_bound_busy (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	var name string
	if err := pool.QueryRow(`SELECT name FROM scopes WHERE id='s-dmz'`).Scan(&name); err != nil || name != "dmz" {
		t.Errorf("after the refusals the scope is %q (err %v), want dmz", name, err)
	}
}
