package runner

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// registerV4 registers a protocol-v4 runner through the real handler (so the
// config_digest is stored exactly as production computes it) and returns its
// id and API key. Capabilities are then blanked directly so polls return
// without entering the 30s long-poll — the drift check reads the STORED
// digest, not the capabilities column, so the digest stays authoritative.
func registerV4(t *testing.T, svc *Service, name string) (runnerID, apiKey string) {
	t.Helper()
	rec := registerWith(t, svc, "test-bootstrap-token", name)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Runner struct {
			ID string `json:"id"`
		} `json:"runner"`
		APIKey string `json:"apiKey"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	mustExec(t, svc, `UPDATE runners SET capabilities='[]' WHERE id=?`, resp.Runner.ID)
	bindRunnerToken(t, svc, "crn_run_"+name, resp.Runner.ID)
	return resp.Runner.ID, "crn_run_" + name
}

// driftPoll drives HandlePoll with an optional configDigest query param.
func driftPoll(t *testing.T, svc *Service, runnerID, token, digest string) *httptest.ResponseRecorder {
	t.Helper()
	as := authSvc(t, svc)
	h := as.RequireRunner(http.HandlerFunc(svc.HandlePoll))
	target := "/api/v1/runners/" + runnerID + "/poll"
	if digest != "" {
		target += "?configDigest=" + digest
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("id", runnerID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func pollHasReRegister(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	if rec.Code != http.StatusOK {
		return false
	}
	var pr runnerproto.PollResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pr); err != nil {
		t.Fatalf("decode poll response: %v", err)
	}
	return hasOp(pr.Control, "re-register")
}

// registerWith's body: name, os Linux, caps [bash], version 1.0, protocol 4 —
// the digest the real register handler stored for it.
func registeredDigest(name string) string {
	return runnerproto.ConfigDigest(name, "Linux", []string{"bash"}, 5, "cronomicon", "1.0", runnerproto.ProtocolVersion)
}

// TestPollDriftDetection: a matching digest is quiet; a mismatch delivers
// re-register immediately; an immediate repeat is suppressed by the flap
// guard (loud log, no op); after the cooldown it fires again; and after a
// redeclare the new digest converges to quiet.
func TestPollDriftDetection(t *testing.T) {
	svc := newTestService(t)
	id, tok := registerV4(t, svc, "driftr")

	// Matching digest → no op (204: no caps, no control).
	if rec := driftPoll(t, svc, id, tok, registeredDigest("driftr")); rec.Code != http.StatusNoContent {
		t.Fatalf("matching digest: got %d, want 204 (no drift)", rec.Code)
	}

	// Drifted digest (e.g. operator added a capability + restarted) → op now.
	drifted := runnerproto.ConfigDigest("driftr", "Linux", []string{"bash", "ansible"}, 5, "cronomicon", "1.0", runnerproto.ProtocolVersion)
	if rec := driftPoll(t, svc, id, tok, drifted); !pollHasReRegister(t, rec) {
		t.Fatalf("drifted digest: expected a re-register op, got %d %s", rec.Code, rec.Body.String())
	}

	// Same mismatch straight after → flap guard suppresses (no op).
	if rec := driftPoll(t, svc, id, tok, drifted); pollHasReRegister(t, rec) {
		t.Fatalf("flap guard: op re-delivered within the cooldown")
	}

	// Cooldown elapsed (rewind the in-memory stamp) → fires again.
	svc.driftMu.Lock()
	svc.driftLastOp[id] = time.Now().Add(-driftOpCooldown - time.Minute)
	svc.driftMu.Unlock()
	if rec := driftPoll(t, svc, id, tok, drifted); !pollHasReRegister(t, rec) {
		t.Fatalf("after cooldown: expected the op again")
	}

	// The agent redeclares (protocol v4 flow) — row + stored digest update.
	as := authSvc(t, svc)
	h := as.RequireRunner(http.HandlerFunc(svc.HandleRedeclare))
	body := `{"name":"driftr","os":"Linux","capabilities":["bash","ansible"],"version":"1.0","protocolVersion":` + strconv.Itoa(runnerproto.ProtocolVersion) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/"+id+"/redeclare", strings.NewReader(body))
	req.SetPathValue("id", id)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("redeclare: got %d (body %s)", rec.Code, rec.Body.String())
	}
	// Redeclare overwrote capabilities; blank them again so the poll below
	// doesn't enter the long-poll. The stored digest is what converges.
	mustExec(t, svc, `UPDATE runners SET capabilities='[]' WHERE id=?`, id)

	// Convergence: the digest of the NEW declared set is now quiet.
	svc.driftMu.Lock()
	delete(svc.driftLastOp, id)
	svc.driftMu.Unlock()
	if rec := driftPoll(t, svc, id, tok, drifted); rec.Code != http.StatusNoContent {
		t.Fatalf("converged digest: got %d, want 204 (no more drift)", rec.Code)
	}
}

// TestPollDriftGates: no digest (pre-Phase-5 agent) → never an op; a
// protocol < 4 row → never an op even on mismatch; an empty STORED digest
// (pre-550 row) + a digest-bearing poll → op (self-heal).
func TestPollDriftGates(t *testing.T) {
	svc := newTestService(t)
	id, tok := registerV4(t, svc, "gates")

	// Corrupt the stored digest so ANY digest-bearing poll would mismatch.
	mustExec(t, svc, `UPDATE runners SET config_digest='different' WHERE id=?`, id)

	// No digest param → no drift check → 204.
	if rec := driftPoll(t, svc, id, tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("pre-Phase-5 agent: got %d, want 204", rec.Code)
	}

	// Pre-550 row (NULL digest) → drift (self-heals via redeclare).
	mustExec(t, svc, `UPDATE runners SET config_digest=NULL WHERE id=?`, id)
	if rec := driftPoll(t, svc, id, tok, registeredDigest("gates")); !pollHasReRegister(t, rec) {
		t.Fatalf("NULL stored digest: expected the self-heal re-register op")
	}
}
