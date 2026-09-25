package runner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// setProtocol rewrites a runner's stored protocol. Since DM-2 its only use is
// to make a row look like one registered BEFORE a server upgrade — the poll
// floor reads this column, so a fixture that wants to be refused sets it low.
// (The v5 pins that used to sit in this file went with the keyscan protocol
// gate in v1.5.40; insertRunner now seeds the current protocol.)
func setProtocol(t *testing.T, svc *Service, runnerID string, v int) {
	t.Helper()
	if _, err := svc.db.Exec(`UPDATE runners SET protocol_version = ? WHERE id = ?`, v, runnerID); err != nil {
		t.Fatalf("setProtocol: %v", err)
	}
}

// keyscanReq drives HandleKeyscan (operator) and returns the status code.
func keyscanReq(t *testing.T, svc *Service, runnerID, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/"+runnerID+"/keyscan", strings.NewReader(body))
	req.SetPathValue("id", runnerID)
	rec := httptest.NewRecorder()
	svc.HandleKeyscan(rec, req)
	return rec.Code
}

// (The v4 → 409 agent_too_old case went with the protocol floor: a runner
// below the floor cannot register, so its row cannot exist.)
func TestKeyscanAccepted(t *testing.T) {
	svc := newTestService(t)
	id := "runner-k4"
	insertRunner(t, svc, id, "k4", "online", []string{"bash"})
	if code := keyscanReq(t, svc, id, `{"hosts":["web01","web02"]}`); code != http.StatusAccepted {
		t.Fatalf("keyscan should 202, got %d", code)
	}
	var raw *string
	_ = svc.db.QueryRow(`SELECT keyscan_requested FROM runners WHERE id = ?`, id).Scan(&raw)
	if raw == nil || !strings.Contains(*raw, "web01") || !strings.Contains(*raw, "web02") {
		t.Fatalf("keyscan_requested not set: %v", raw)
	}
}

func TestKeyscanDeliveredOnceOnPoll(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-kd", "crn_run_kd"
	insertRunner(t, svc, id, "kd", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	if code := keyscanReq(t, svc, id, `{"hosts":["web01:22"]}`); code != http.StatusAccepted {
		t.Fatalf("keyscan queue: %d", code)
	}
	// First poll delivers the keyscan op with the host list, immediately.
	pr, code := pollDecode(t, svc, as, id, tok, "settingsVersion=0")
	if code != http.StatusOK {
		t.Fatalf("poll: %d", code)
	}
	var got *runnerproto.PollControl
	for i := range pr.Control {
		if pr.Control[i].Op == "keyscan" {
			got = &pr.Control[i]
		}
	}
	if got == nil || len(got.Hosts) != 1 || got.Hosts[0] != "web01:22" {
		t.Fatalf("expected keyscan op with [web01:22], got %+v", pr.Control)
	}
	// The request was consumed (deliver-once): a claimable run makes the next
	// poll return immediately with no keyscan op.
	insertQueuedRun(t, svc, "q-kd", "kd-job", "bash", "")
	pr2, _ := pollDecode(t, svc, as, id, tok, "settingsVersion=0")
	for _, c := range pr2.Control {
		if c.Op == "keyscan" {
			t.Errorf("keyscan re-delivered after consumption")
		}
	}
}

// uploadKeys drives HandleUploadHostKeys through RequireRunner.
func uploadKeys(t *testing.T, svc *Service, as *auth.Service, id, tok, body string) int {
	t.Helper()
	h := as.RequireRunner(http.HandlerFunc(svc.HandleUploadHostKeys))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/"+id+"/hostkeys", strings.NewReader(body))
	req.SetPathValue("id", id)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHostKeyScanApproveTrustRoundTrip(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-rt", "crn_run_rt"
	insertRunner(t, svc, id, "rt", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	// Agent uploads a scanned key.
	line := "web01 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI"
	body := `{"entries":[{"host":"web01","keyType":"ssh-ed25519","fingerprint":"SHA256:abc123","knownHostsLine":"` + line + `"}]}`
	if code := uploadKeys(t, svc, as, id, tok, body); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}

	// It appears in the pending list.
	lreq := httptest.NewRequest(http.MethodGet, "/api/v1/runners/host-keys/pending", nil)
	lrec := httptest.NewRecorder()
	svc.HandleListPendingHostKeys(lrec, lreq)
	if lrec.Code != http.StatusOK || !strings.Contains(lrec.Body.String(), "SHA256:abc123") {
		t.Fatalf("pending list missing the key: %d %s", lrec.Code, lrec.Body.String())
	}
	var keyID string
	_ = svc.db.QueryRow(`SELECT id FROM pending_host_keys WHERE runner_id = ?`, id).Scan(&keyID)

	// Before approval, no trust-hosts op is delivered.
	if lines := svc.takeTrustHosts(context.Background(), id); len(lines) != 0 {
		t.Fatalf("trust-hosts delivered before approval: %v", lines)
	}

	// Approve.
	rreq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/host-keys/"+keyID+"/resolve", strings.NewReader(`{"action":"approve"}`))
	rreq.SetPathValue("keyId", keyID)
	rrec := httptest.NewRecorder()
	svc.HandleResolveHostKey(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rrec.Code, rrec.Body.String())
	}

	// The next poll delivers trust-hosts with the approved line...
	pr, _ := pollDecode(t, svc, as, id, tok, "settingsVersion=0")
	var trust *runnerproto.PollControl
	for i := range pr.Control {
		if pr.Control[i].Op == "trust-hosts" {
			trust = &pr.Control[i]
		}
	}
	if trust == nil || len(trust.Entries) != 1 || trust.Entries[0] != line {
		t.Fatalf("expected trust-hosts with the approved line, got %+v", pr.Control)
	}
	// ...once. It's marked trusted, so a later poll (with a claimable run) doesn't re-send.
	insertQueuedRun(t, svc, "q-rt", "rt-job", "bash", "")
	pr2, _ := pollDecode(t, svc, as, id, tok, "settingsVersion=0")
	for _, c := range pr2.Control {
		if c.Op == "trust-hosts" {
			t.Errorf("trust-hosts re-delivered after being marked trusted")
		}
	}
}

func TestHostKeyRejectNeverTrusts(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-rj", "crn_run_rj"
	insertRunner(t, svc, id, "rj", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	body := `{"entries":[{"host":"evil","keyType":"ssh-rsa","fingerprint":"SHA256:bad","knownHostsLine":"evil ssh-rsa AAAA"}]}`
	if code := uploadKeys(t, svc, as, id, tok, body); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	var keyID string
	_ = svc.db.QueryRow(`SELECT id FROM pending_host_keys WHERE runner_id = ?`, id).Scan(&keyID)

	rreq := httptest.NewRequest(http.MethodPost, "/api/v1/runners/host-keys/"+keyID+"/resolve", strings.NewReader(`{"action":"reject"}`))
	rreq.SetPathValue("keyId", keyID)
	rrec := httptest.NewRecorder()
	svc.HandleResolveHostKey(rrec, rreq)
	if rrec.Code != http.StatusOK {
		t.Fatalf("reject: %d", rrec.Code)
	}
	// A rejected key is never delivered as trust.
	if lines := svc.takeTrustHosts(context.Background(), id); len(lines) != 0 {
		t.Errorf("rejected key was delivered as trust: %v", lines)
	}
	// Re-resolving a resolved key is a conflict.
	rreq2 := httptest.NewRequest(http.MethodPost, "/api/v1/runners/host-keys/"+keyID+"/resolve", strings.NewReader(`{"action":"approve"}`))
	rreq2.SetPathValue("keyId", keyID)
	rrec2 := httptest.NewRecorder()
	svc.HandleResolveHostKey(rrec2, rreq2)
	if rrec2.Code != http.StatusConflict {
		t.Errorf("re-resolving should 409, got %d", rrec2.Code)
	}
}

func TestUploadRejectsNewlineInjection(t *testing.T) {
	svc := newTestService(t)
	as := authSvc(t, svc)
	id, tok := "runner-inj", "crn_run_inj"
	insertRunner(t, svc, id, "inj", "online", []string{"bash"})
	bindRunnerToken(t, svc, tok, id)

	// A malicious runner tries to smuggle a second trusted host via an embedded
	// newline in the known_hosts line — it must be dropped, not stored.
	body := `{"entries":[{"host":"good","keyType":"ssh-ed25519","fingerprint":"SHA256:ok","knownHostsLine":"good ssh-ed25519 AAAA\nevil.example.com ssh-rsa BBBB"}]}`
	if code := uploadKeys(t, svc, as, id, tok, body); code != http.StatusOK {
		t.Fatalf("upload: %d", code)
	}
	var n int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM pending_host_keys WHERE runner_id = ?`, id).Scan(&n)
	if n != 0 {
		t.Fatalf("newline-injected entry must be rejected, got %d rows", n)
	}
}

func TestHostKeyReasonSurfacesInRun(t *testing.T) {
	// A trailing envelope carrying a host_key_unverified reason is parsed (and
	// finalizeRun stores env.Reason as the run's status reason), so the run
	// detail can offer the action.
	env := parseTrailingEnvelope(`{"exitCode":1,"durationMs":5,"endedAt":"2026-07-10T00:00:00Z","reason":"host_key_unverified: web01:22"}`)
	if env == nil || env.Reason != "host_key_unverified: web01:22" {
		t.Fatalf("envelope reason not parsed: %+v", env)
	}
}
