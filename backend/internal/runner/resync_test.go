package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// postResync drives HandleResync directly (the operator-side flag setter).
func postResync(t *testing.T, svc *Service, runnerID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/"+runnerID+"/resync", nil)
	req.SetPathValue("id", runnerID)
	rec := httptest.NewRecorder()
	svc.HandleResync(rec, req)
	return rec
}

// TestResyncEndpoint: unknown runner → 404; a registered runner → 202 with the
// flag set. (The protocol < 4 → 409 agent_too_old case went with the protocol
// floor: a runner below the floor cannot register, so the row cannot exist.)
func TestResyncEndpoint(t *testing.T) {
	svc := newTestService(t)

	if got := postResync(t, svc, "nope").Code; got != http.StatusNotFound {
		t.Fatalf("unknown runner: got %d, want 404", got)
	}

	id := "runner-cur"
	insertRunner(t, svc, id, "cur", "online", []string{"bash"})
	mustExec(t, svc, `UPDATE runners SET protocol_version=? WHERE id=?`, runnerproto.ProtocolVersion, id)

	rec := postResync(t, svc, id)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	if flag := resyncFlag(t, svc, id); flag != 1 {
		t.Errorf("resync_requested = %d, want 1", flag)
	}
	var resp runnerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 202 body: %v", err)
	}
	if resp.ProtocolVersion != runnerproto.ProtocolVersion {
		t.Errorf("202 body protocolVersion = %d, want %d", resp.ProtocolVersion, runnerproto.ProtocolVersion)
	}
}

// TestPollDeliversResyncOnce: with resync_requested set on a v4 runner, the
// next poll returns the re-register control op immediately and clears the flag;
// the poll after that delivers nothing.
func TestPollDeliversResyncOnce(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	// No capabilities → the handler never enters the 30s long-poll: it returns
	// 200 when control is pending, else 204.
	id, tok := "runner-rs", "crn_run_rs"
	insertRunner(t, svc, id, "rs", "online", nil)
	mustExec(t, svc, `UPDATE runners SET capabilities='[]', protocol_version=?, resync_requested=1 WHERE id=?`, runnerproto.ProtocolVersion, id)
	bindRunnerToken(t, svc, tok, id)

	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	poll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/runners/"+id+"/poll", nil)
		req.SetPathValue("id", id)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := poll()
	if rec.Code != http.StatusOK {
		t.Fatalf("first poll: got %d, want 200 (re-register delivery)", rec.Code)
	}
	var pr runnerproto.PollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	if !hasOp(pr.Control, "re-register") {
		t.Fatalf("first poll control = %+v, want a re-register op", pr.Control)
	}
	if flag := resyncFlag(t, svc, id); flag != 0 {
		t.Errorf("after delivery: resync_requested = %d, want 0 (deliver-once)", flag)
	}

	// Second poll: nothing pending → 204, no re-delivery.
	if rec := poll(); rec.Code != http.StatusNoContent {
		t.Errorf("second poll: got %d, want 204 (op must not repeat)", rec.Code)
	}
}

// TestRedeclareUpdatesInPlace: the v4 redeclare updates the existing row —
// same id, same agency membership, same (unrevoked) runner token, no second
// row — and persists the newly declared config.
func TestRedeclareUpdatesInPlace(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	id, tok := "runner-rd", "crn_run_rd"
	insertRunner(t, svc, id, "rd", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	// Agency membership that must survive the redeclare.
	mustExec(t, svc, `INSERT INTO agencies(id, name, created_at) VALUES ('ag1','vlan40',?)`, now())
	mustExec(t, svc, `INSERT INTO runner_agencies(runner_id, agency_id) VALUES (?, 'ag1')`, id)

	h := as.RequireRunner(http.HandlerFunc(svc.HandleRedeclare))
	redeclare := func(targetID, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/runners/"+targetID+"/redeclare", strings.NewReader(body))
		req.SetPathValue("id", targetID)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	body := `{"name":"rd","os":"Linux","capabilities":["bash","ansible","checkout"],
	          "version":"2.0","maxConcurrent":8,"inventory":"local","protocolVersion":` + strconv.Itoa(runnerproto.ProtocolVersion) + `}`
	rec := redeclare(id, tok, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeclare: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// Still exactly one row, same id, new declared config.
	var n int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("runners count = %d (err %v), want 1 — no orphan row", n, err)
	}
	var caps, inventory, version string
	var maxC, pv int
	if err := svc.db.QueryRow(`
		SELECT capabilities, inventory, version, max_concurrent, protocol_version
		FROM runners WHERE id = ?`, id).Scan(&caps, &inventory, &version, &maxC, &pv); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if !strings.Contains(caps, "checkout") || inventory != "local" || version != "2.0" || maxC != 8 || pv != runnerproto.ProtocolVersion {
		t.Errorf("declared config not persisted: caps=%s inventory=%s version=%s maxConcurrent=%d pv=%d",
			caps, inventory, version, maxC, pv)
	}

	// Agency membership preserved.
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runner_agencies WHERE runner_id=?`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("agency membership lost on redeclare: count=%d (err %v)", n, err)
	}
	// Runner token NOT revoked — the whole point is keeping the key.
	if err := svc.db.QueryRow(`
		SELECT COUNT(*) FROM runner_tokens WHERE runner_id=? AND revoked_at IS NULL`, id).Scan(&n); err != nil || n != 1 {
		t.Errorf("runner token revoked by redeclare: active=%d (err %v)", n, err)
	}

	// A pending resync flag is cleared by the redeclare itself.
	mustExec(t, svc, `UPDATE runners SET resync_requested=1 WHERE id=?`, id)
	if rec := redeclare(id, tok, body); rec.Code != http.StatusOK {
		t.Fatalf("second redeclare: got %d, want 200", rec.Code)
	}
	if flag := resyncFlag(t, svc, id); flag != 0 {
		t.Errorf("resync_requested = %d after redeclare, want 0", flag)
	}
}

// TestRedeclareOwnershipAndVersion: a non-owning token → 404 with no side
// effects; a declared protocol < 4 → 426 (redeclare is a v4-only flow).
func TestRedeclareOwnershipAndVersion(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)

	aID, aTok := "runner-a", "crn_run_a"
	bID, bTok := "runner-b", "crn_run_b"
	insertRunner(t, svc, aID, "a", "online", []string{"bash"})
	insertRunner(t, svc, bID, "b", "online", []string{"bash"})
	bindRunnerToken(t, svc, aTok, aID)
	bindRunnerToken(t, svc, bTok, bID)

	h := as.RequireRunner(http.HandlerFunc(svc.HandleRedeclare))
	redeclare := func(targetID, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost,
			"/api/v1/runners/"+targetID+"/redeclare", strings.NewReader(body))
		req.SetPathValue("id", targetID)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	v4body := `{"name":"evil","os":"Linux","capabilities":["bash"],"version":"9.9","protocolVersion":` + strconv.Itoa(runnerproto.ProtocolVersion) + `}`

	// A's token redeclaring B → 404 (no existence leak), B unchanged.
	if got := redeclare(bID, aTok, v4body).Code; got != http.StatusNotFound {
		t.Fatalf("A redeclaring B: got %d, want 404", got)
	}
	var bName string
	if err := svc.db.QueryRow(`SELECT name FROM runners WHERE id=?`, bID).Scan(&bName); err != nil || bName != "b" {
		t.Errorf("B mutated by impersonator: name=%q (err %v)", bName, err)
	}

	// Owner declaring below the floor → 426.
	oldBody := `{"name":"b","os":"Linux","capabilities":["bash"],"version":"1.0","protocolVersion":` + strconv.Itoa(runnerproto.MinProtocolVersion-1) + `}`
	if got := redeclare(bID, bTok, oldBody).Code; got != http.StatusUpgradeRequired {
		t.Fatalf("below-floor redeclare: got %d, want 426", got)
	}
}

// resyncFlag reads runners.resync_requested for assertions.
func resyncFlag(t *testing.T, svc *Service, id string) int {
	t.Helper()
	var flag int
	if err := svc.db.QueryRow(`SELECT resync_requested FROM runners WHERE id=?`, id).Scan(&flag); err != nil {
		t.Fatalf("read resync_requested: %v", err)
	}
	return flag
}

// mustExec runs an exec statement, failing the test on error.
func mustExec(t *testing.T, svc *Service, q string, args ...any) {
	t.Helper()
	if _, err := svc.db.Exec(q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}

// hasOp reports whether the control slice carries the given op.
func hasOp(control []runnerproto.PollControl, op string) bool {
	for _, c := range control {
		if c.Op == op {
			return true
		}
	}
	return false
}
