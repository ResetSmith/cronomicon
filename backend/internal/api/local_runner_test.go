package api_test

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
)

// Phase A of the local runner (LR-1, LR-17, LR-38, LR-43; MA-11, MA-14).
//
// The local runner is this server running shell jobs itself: one row of kind
// `server`, Global's, turned on and off by a global administrator, and the one
// runner whose serve list is edited.

type localRunnerJSON struct {
	RunnerID      string `json:"runnerId"`
	Enabled       bool   `json:"enabled"`
	Forbidden     bool   `json:"forbidden"`
	Status        string `json:"status"`
	MaxConcurrent int    `json:"maxConcurrent"`
	Serves        []struct {
		ID string `json:"id"`
	} `json:"serves"`
}

func getLocalRunner(t *testing.T, h http.Handler, who string) localRunnerJSON {
	t.Helper()
	rec := gateReq(t, h, http.MethodGet, "/api/v1/local-runner", who, ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s reading the local runner = %d (%s)", who, rec.Code, rec.Body)
	}
	var lr localRunnerJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &lr); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	return lr
}

func TestLocalRunner_TheRowExistsAndTheSwitchIsAGlobalAdministrators(t *testing.T) {
	h, pool := gateServer(t)

	// The server made its row when it started: Global's, off, serving Global.
	// Any session may read it — the runner list shows the row to everyone.
	lr := getLocalRunner(t, h, gFinViewer)
	if lr.RunnerID == "" || lr.Enabled || lr.Forbidden || lr.Status != "offline" || lr.MaxConcurrent != 4 ||
		len(lr.Serves) != 1 || lr.Serves[0].ID != "global" {
		t.Fatalf("the local runner of a new install = %+v, want off, offline, 4 at once, serving Global", lr)
	}

	put := func(who, body string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/local-runner", who, body)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	// Turning it on makes this server hold SSH keys and run jobs: install-wide.
	for _, who := range []string{gFinAdmin, gMixed, gAllViewer, gFinViewer} {
		if code, _ := put(who, `{"enabled":true}`); code != http.StatusForbidden {
			t.Errorf("%s turning the local runner on = %d, want 403", who, code)
		}
	}
	if getLocalRunner(t, h, gRoot).Enabled {
		t.Fatal("a refused request turned the local runner on")
	}
	if code, ec := put(gRoot, `{}`); code != 422 {
		t.Errorf("a body that changes nothing = %d %s, want 422", code, ec)
	}
	if code, ec := put(gRoot, `{"maxConcurrent":0}`); code != 422 {
		t.Errorf("concurrency 0 = %d %s, want 422", code, ec)
	}

	if code, ec := put(gRoot, `{"enabled":true,"maxConcurrent":6}`); code != 200 {
		t.Fatalf("a global administrator turning it on = %d %s", code, ec)
	}
	// The engine was told, not only the database: the row reads online at once.
	lr = getLocalRunner(t, h, gRoot)
	if !lr.Enabled || lr.Status != "online" || lr.MaxConcurrent != 6 {
		t.Errorf("after turning on = %+v, want on, online, 6 at once", lr)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM activity WHERE runner_name = 'Local runner' AND actor = 'root-admins@example.com'`); n != 2 {
		t.Errorf("%d activity rows for the change, want one for the switch and one for the concurrency", n)
	}
	if code, ec := put(gRoot, `{"enabled":false}`); code != 200 {
		t.Fatalf("turning it off = %d %s", code, ec)
	}
	if lr = getLocalRunner(t, h, gRoot); lr.Enabled || lr.Status != "offline" {
		t.Errorf("after turning off = %+v, want off and offline", lr)
	}
}

// LR-17: the host can forbid it. It then reads as off, and the app cannot turn
// it on.
func TestLocalRunner_TheHostCanForbidIt(t *testing.T) {
	h, _ := gateServerWith(t, func(c *config.Config) { c.LocalRunnerForbid = true })
	if lr := getLocalRunner(t, h, gRoot); lr.Enabled || !lr.Forbidden {
		t.Fatalf("under forbid = %+v, want off and forbidden", lr)
	}
	rec := gateReq(t, h, http.MethodPut, "/api/v1/local-runner", gRoot, `{"enabled":true}`)
	if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "local_runner_forbidden" {
		t.Errorf("turning it on under forbid = %d %s, want 409 local_runner_forbidden (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if lr := getLocalRunner(t, h, gRoot); lr.Enabled || lr.Status != "offline" {
		t.Errorf("a refused request changed it: %+v", lr)
	}
}

func localRunnerID(t *testing.T, pool *sql.DB) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(`SELECT id FROM runners WHERE kind = 'server'`).Scan(&id); err != nil {
		t.Fatalf("the local runner's row: %v", err)
	}
	return id
}

// The local runner has no agent: nothing to deregister, drain, re-declare,
// reconfigure or test, secret injection is fixed on, and until its host keys
// move to its own ledger the host-key routes are not its either. Each of those
// routes says so (422 `local_runner`), to whoever may manage the runner — and
// answers the ordinary 403 to anyone who may not.
func TestLocalRunner_AgentRoutesRefuseIt(t *testing.T) {
	h, pool := gateServer(t)
	id := localRunnerID(t, pool)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/runners/" + id + "/drain", `{}`},
		{"POST", "/api/v1/runners/" + id + "/resync", `{}`},
		{"PATCH", "/api/v1/runners/" + id + "/settings", `{}`},
		{"PUT", "/api/v1/runners/" + id + "/secret-injection", `{"allow":false}`},
		{"POST", "/api/v1/runners/" + id + "/test", `{}`},
		{"POST", "/api/v1/runners/" + id + "/keyscan", `{"hosts":["10.0.0.1"]}`},
		{"GET", "/api/v1/runners/" + id + "/host-keys", ``},
		{"POST", "/api/v1/runners/" + id + "/host-keys/provide", `{"lines":["h ssh-ed25519 AAAA"]}`},
		{"POST", "/api/v1/runners/" + id + "/known-hosts/refresh", `{}`},
		// It never enrols, so it is never offered an agent's old placement.
		{"POST", "/api/v1/runners/" + id + "/placement", `{"historyId":1}`},
		{"POST", "/api/v1/runners/" + id + "/placement/dismiss", `{"historyId":1}`},
		{"DELETE", "/api/v1/runners/" + id, ``},
	}
	for _, r := range routes {
		if code := gateReq(t, h, r.method, r.path, gFinAdmin, r.body).Code; code != http.StatusForbidden {
			t.Errorf("an agency admin — %s %s = %d, want 403 (it is Global's)", r.method, strings.Replace(r.path, id, "{local}", 1), code)
		}
		rec := gateReq(t, h, r.method, r.path, gRoot, r.body)
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "local_runner" {
			t.Errorf("a global admin — %s %s = %d %s, want 422 local_runner (%s)", r.method, strings.Replace(r.path, id, "{local}", 1), rec.Code, errCode(rec.Body.Bytes()), rec.Body)
		}
	}
	// It is still there, with secret injection on (LR-46).
	if n := count(t, pool, `SELECT COUNT(*) FROM runners WHERE id = ? AND allow_secret_injection = 1`, id); n != 1 {
		t.Error("the local runner's row was removed, or its secret injection turned off")
	}
	// Tags are labels, and it may carry them like any runner.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-tags/"+id, gRoot, `{"tags":["dc1"]}`); rec.Code/100 != 2 {
		t.Errorf("tagging the local runner = %d (%s)", rec.Code, rec.Body)
	}
	// And an agent's routes are unaffected.
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin", "fin-agent", "ag:FIN")
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-fin/drain", gFinAdmin, `{}`); rec.Code == http.StatusUnprocessableEntity {
		t.Errorf("an agent was refused as if it were the local runner: %s", rec.Body)
	}
}

// MA-11, MA-14: the local runner's serve list is whatever non-empty set a
// global administrator gives it — Global beside agencies included — and nobody
// else's to write.
//
// Binding a scope to it is refused for now, whoever asks and whatever it
// serves: a bound scope sends its jobs to the runner executor, and until Phase
// B the local runner claims only the SSH executor's rows, so the scope's jobs
// would wait for ever. (From Phase B, LR-62: an agency binds its own scope to
// the local runner when it serves them.)
func TestLocalRunner_ItsServeListIsAGlobalAdministratorsAndNoScopeBindsToItYet(t *testing.T) {
	h, pool := gateServer(t)
	id := localRunnerID(t, pool)
	set := func(who string, agencies ...string) (int, string) {
		t.Helper()
		body, _ := json.Marshal([]map[string]any{{"runnerId": id, "agencyIds": agencies}})
		rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", who, string(body))
		return rec.Code, errCode(rec.Body.Bytes())
	}
	bind := func(who, scope string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/"+scope+"/runners", who, `{"runnerIds":["`+id+`"]}`)
		return rec.Code, errCode(rec.Body.Bytes())
	}

	// It serves Global only: FIN cannot bind its scope to it.
	if code, ec := bind(gFinAdmin, "sc:fin"); code != 422 || ec != "runner_not_eligible" {
		t.Errorf("FIN binding its scope to a local runner that does not serve it = %d %s, want 422 runner_not_eligible", code, ec)
	}
	// Nor can FIN put itself on the serve list.
	if code, _ := set(gFinAdmin, "global", "ag:FIN"); code != 403 {
		t.Errorf("FIN adding itself to the local runner's serve list = %d, want 403", code)
	}
	if code, _ := set(gMixed, "ag:FIN"); code != 403 {
		t.Errorf("an all-scopes viewer who administers FIN writing the serve list = %d, want 403", code)
	}
	if got := served(t, pool, id); !slices.Equal(got, []string{"global"}) {
		t.Fatalf("a refused write changed the serve list to %v", got)
	}

	// A global administrator widens it, Global beside an agency.
	if code, ec := set(gRoot, "global", "ag:FIN"); code != 200 {
		t.Fatalf("a global admin adding FIN = %d %s", code, ec)
	}
	if got := served(t, pool, id); !slices.Equal(got, []string{"ag:FIN", "global"}) {
		t.Fatalf("it serves %v, want FIN and Global", got)
	}
	if code, ec := set(gRoot); code != 422 || ec != "agency_required" {
		t.Errorf("an empty serve list = %d %s, want 422 agency_required", code, ec)
	}
	// It is not a legacy placement, whatever it serves.
	for k := range listNotices(t, h, gRoot) {
		if k == "legacy_placement/"+id {
			t.Error("the local runner was reported as a legacy placement")
		}
	}

	// It serves FIN now, and FIN's scope still cannot be bound to it — not by
	// FIN, not by a global administrator, and not by handing an agent's
	// bindings over to it.
	for _, who := range []string{gFinAdmin, gRoot} {
		if code, ec := bind(who, "sc:fin"); code != 422 || ec != "runner_not_eligible" {
			t.Errorf("binding FIN's scope to the local runner as %s = %d %s, want 422 runner_not_eligible", who, code, ec)
		}
	}
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin", "fin-agent", "ag:FIN")
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:fin/runners", gFinAdmin, `{"runnerIds":["r-fin"]}`); rec.Code != 200 {
		t.Fatalf("fixture: binding FIN's scope to its agent = %d (%s)", rec.Code, rec.Body)
	}
	rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-runners/replace", gRoot, `{"fromRunnerId":"r-fin","toRunnerId":"`+id+`"}`)
	if rec.Code/100 == 2 {
		t.Errorf("an agent's bindings were handed to the local runner: %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE runner_id = ?`, id); n != 0 {
		t.Errorf("%d scope(s) are bound to the local runner", n)
	}
	// Its owner never changes.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/owner", gRoot, `{"agencyId":"ag:FIN"}`)
	if rec.Code != 422 || errCode(rec.Body.Bytes()) != "owner_change_refused" {
		t.Errorf("handing the local runner to an agency = %d %s, want 422 owner_change_refused", rec.Code, errCode(rec.Body.Bytes()))
	}
}
