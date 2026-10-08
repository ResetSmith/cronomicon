package api_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

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
// reconfigure or test, secret injection is fixed on, it never enrols, and it
// has no known_hosts file to be sent a line or asked to report. Each of those
// routes says so (422 `local_runner`), to whoever may manage the runner — and
// answers the ordinary 403 to anyone who may not. Its host keys, though, are
// reviewed and recorded like an agent's.
func TestLocalRunner_AgentRoutesRefuseIt(t *testing.T) {
	h, pool := gateServer(t)
	id := localRunnerID(t, pool)
	routes := []struct{ method, path, body string }{
		{"POST", "/api/v1/runners/" + id + "/drain", `{}`},
		{"POST", "/api/v1/runners/" + id + "/resync", `{}`},
		{"PATCH", "/api/v1/runners/" + id + "/settings", `{}`},
		{"PUT", "/api/v1/runners/" + id + "/secret-injection", `{"allow":false}`},
		{"POST", "/api/v1/runners/" + id + "/test", `{}`},
		{"POST", "/api/v1/runners/" + id + "/host-keys/1/resend", `{}`},
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
	// Its host keys are its owner's to read and decide, like an agent's — and
	// an agency administrator's only by LR-63's exception, which is not this.
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/runners/"+id+"/host-keys", gRoot, ""); rec.Code != http.StatusOK {
		t.Errorf("a global admin reading the local runner's host keys = %d (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/runners/"+id+"/host-keys", gFinAdmin, ""); rec.Code != http.StatusForbidden {
		t.Errorf("an agency admin reading the local runner's host keys = %d, want 403", rec.Code)
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
// else's to write. LR-62: an agency binds its own scope to the local runner
// when it serves them, and cannot when it does not.
func TestLocalRunner_ItsServeListIsAGlobalAdministratorsAndAgenciesBindToIt(t *testing.T) {
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

	// Now it serves FIN: FIN binds its own scope to it, with no authority over
	// the runner; TAX still cannot bind theirs.
	if code, ec := bind(gFinAdmin, "sc:fin"); code != 200 {
		t.Errorf("FIN binding its scope to the local runner that serves it = %d %s, want 200", code, ec)
	}
	if code, ec := bind(gTaxAdmin, "sc:tax"); code != 422 || ec != "runner_not_eligible" {
		t.Errorf("TAX binding its scope to a local runner that does not serve TAX = %d %s, want 422 runner_not_eligible", code, ec)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE runner_id = ? AND scope_id = 'sc:fin'`, id); n != 1 {
		t.Errorf("FIN's scope has %d binding(s) to the local runner, want 1", n)
	}
	// Its owner never changes.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/owner", gRoot, `{"agencyId":"ag:FIN"}`)
	if rec.Code != 422 || errCode(rec.Body.Bytes()) != "owner_change_refused" {
		t.Errorf("handing the local runner to an agency = %d %s, want 422 owner_change_refused", rec.Code, errCode(rec.Body.Bytes()))
	}
}

// A run that waits with no stored reason gets one when it is read. For the
// causes 2.3.0 added, the older hints would misname it: a Global shell run with
// the local runner turned off is not "no online runner in Global" in any way an
// operator can act on — the runner is there, and it is switched off.
func TestLocalRunner_AWaitingRunSaysTheLocalRunnerIsOff(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run-waits', 'loose', 'git', 'bash', 'queued', 'seed', 'manual', 'runner', '2026-10-07T00:00:00Z')`)
	// A row still frozen onto the SSH executor, which nothing claims any more.
	exec(`INSERT INTO runs (id, job_name, job_source, run_type, status, triggered_by, trigger_kind, executor, created_at)
	      VALUES ('run-ssh', 'loose', 'git', 'bash', 'queued', 'seed', 'manual', 'ssh', '2026-10-07T00:00:00Z')`)

	reason := func(id string) string {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/runs/"+id, gRoot, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET run %s = %d (%s)", id, rec.Code, rec.Body)
		}
		var run struct {
			StatusReason string `json:"statusReason"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
			t.Fatalf("decode run: %v\n%s", err, rec.Body)
		}
		return run.StatusReason
	}
	if got := reason("run-waits"); !strings.Contains(got, "local runner") || !strings.Contains(got, "not running") {
		t.Errorf("statusReason = %q, want it to say the local runner would take the run and is not running", got)
	}
	if got := reason("run-ssh"); !strings.Contains(got, "SSH executor") {
		t.Errorf("statusReason of a row frozen onto the SSH executor = %q, want it to say nothing will claim it", got)
	}
}

// ecdsaHostKey is realHostKey with a key of another type.
func ecdsaHostKey(t *testing.T, host string) (line, keyType, fingerprint string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), key.Type(), ssh.FingerprintSHA256(key)
}

// LR-63 lets an administrator of an agency a runner SERVES, but does not own,
// approve the first key for a host of their own scope. "First" means the host
// has no trusted key at all: a runner accepts any key in force for a host,
// whichever type it negotiates, so a second key of ANOTHER type is not a first
// key — it is a second way to be that host, and the owner's to decide.
func TestHostKeyGuest_ASecondKeyOfAnotherTypeIsNotAFirstKey(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	approve := func(who, id string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-legacy/host-keys/resolve-batch", who, `{"approve":["`+id+`"]}`)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	pending := func(id string, mk func(*testing.T, string) (string, string, string)) string {
		line, keyType, fp := mk(t, "fin9.example")
		exec(`INSERT INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at, scope_id, scope_name, host_name)
		      VALUES (?, 'r-legacy', 'fin9.example', ?, ?, ?, '2026-01-01T00:00:00Z', 'sc:fin', 'fin-hosts', 'fin9.example')`, id, keyType, fp, line)
		return fp
	}
	pending("pk-ed", realHostKey)
	if code, ec := approve(gFinAdmin, "pk-ed"); code != 200 {
		t.Fatalf("the guest approving the first key = %d %s, want 200", code, ec)
	}
	// An on-path attacker offers only ecdsa to the next scan.
	pending("pk-ec", ecdsaHostKey)
	if code, ec := approve(gFinAdmin, "pk-ec"); code != 403 || ec != "owner_required" {
		t.Errorf("the guest adding a second key of another type = %d %s, want 403 owner_required", code, ec)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = 'r-legacy' AND host = 'fin9.example' AND decision = 'approved' AND superseded_at IS NULL`); n != 1 {
		t.Errorf("%d keys in force for the host after the refusal, want the 1 the guest first approved", n)
	}
	// The owner may add it.
	if code, ec := approve(gRoot, "pk-ec"); code != 200 {
		t.Errorf("the owner adding the second key = %d %s, want 200", code, ec)
	}
}

// The local runner's keys are a global administrator's to decide, with no
// guest exception. The server's trust in a host is keyed by its address and is
// the same for every agency whose runs it takes — it holds all their secrets —
// so a key one agency's administrator approved would be the key the server
// accepts when it connects to that address for any other. A guest may still
// scan their own scope with it; what the scan finds waits for a global
// administrator.
func TestLocalRunner_ItsHostKeysAreAGlobalAdministratorsToDecide(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	id := localRunnerID(t, pool)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, 'ag:FIN')`, id) // it serves FIN
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, scope_id, source, created_by, created_at, last_modified_by, last_modified_at)
	      VALUES ('h-fin1', 'fin1', '10.5.0.1', 22, 'sc:fin', 'git', 't', 't', 't', 't')`)
	exec(`INSERT OR IGNORE INTO scope_hosts (scope_id, host) VALUES ('sc:fin', 'fin1')`)

	// FIN may queue a scan of its own scope with it...
	rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/keyscan", gFinAdmin, `{"scopeId":"sc:fin"}`)
	if rec.Code != http.StatusAccepted {
		t.Errorf("FIN scanning its own scope with the local runner = %d (%s), want 202", rec.Code, rec.Body)
	}
	// ...and nothing else: not typed hosts, not another agency's scope.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/keyscan", gFinAdmin, `{"hosts":["10.5.0.9"]}`); rec.Code != http.StatusForbidden {
		t.Errorf("FIN scanning typed hosts from the server = %d, want 403", rec.Code)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/keyscan", gFinAdmin, `{"scopeId":"sc:tax"}`); rec.Code == http.StatusAccepted {
		t.Errorf("FIN scanning TAX's scope with the local runner = %d", rec.Code)
	}

	// A key found for FIN's host: FIN sees it and cannot approve it — not even
	// a first key.
	line, keyType, fp := realHostKey(t, "10.5.0.1")
	exec(`INSERT OR REPLACE INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at, scope_id, scope_name, host_name)
	      VALUES ('pk-local', ?, '10.5.0.1', ?, ?, ?, '2026-01-01T00:00:00Z', 'sc:fin', 'fin-hosts', 'fin1')`, id, keyType, fp, line)
	if rec := gateReq(t, h, http.MethodGet, "/api/v1/runners/"+id+"/host-keys/pending", gFinAdmin, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), fp) {
		t.Errorf("FIN listing what its scan found = %d (%s), want the key", rec.Code, rec.Body)
	}
	rec = gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/host-keys/resolve-batch", gFinAdmin, `{"approve":["pk-local"]}`)
	if rec.Code != http.StatusForbidden || errCode(rec.Body.Bytes()) != "owner_required" {
		t.Fatalf("FIN approving a key on the local runner = %d %s, want 403 owner_required (%s)", rec.Code, errCode(rec.Body.Bytes()), rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ?`, id); n != 0 {
		t.Fatalf("a refused approval wrote %d ledger row(s)", n)
	}
	for _, route := range []struct{ path, body string }{
		{"/host-keys/provide", `{"lines":["` + line + `"],"dryRun":true}`},
		{"/host-keys", ``},
	} {
		method := http.MethodPost
		if route.body == "" {
			method = http.MethodGet
		}
		if rec := gateReq(t, h, method, "/api/v1/runners/"+id+route.path, gFinAdmin, route.body); rec.Code != http.StatusForbidden {
			t.Errorf("FIN on the local runner's %s = %d, want 403", route.path, rec.Code)
		}
	}

	// A global administrator approves it, and it is in force at once.
	rec = gateReq(t, h, http.MethodPost, "/api/v1/runners/"+id+"/host-keys/resolve-batch", gRoot, `{"approve":["pk-local"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a global administrator approving it = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM host_key_ledger WHERE runner_id = ? AND host = '10.5.0.1' AND decision = 'approved'
	                         AND superseded_at IS NULL AND delivered_at IS NOT NULL AND confirmed_at IS NOT NULL`, id); n != 1 {
		t.Errorf("after the approval, %d key(s) in force and settled for the host, want 1", n)
	}
	// The host record shows it.
	rec = gateReq(t, h, http.MethodGet, "/api/v1/ssh/hosts", gRoot, "")
	if !strings.Contains(rec.Body.String(), fp) {
		t.Errorf("the host record does not show the key the local runner now trusts: %s", rec.Body)
	}
}
