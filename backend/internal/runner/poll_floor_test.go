package runner

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// TestPollRefusesSubFloorProtocol pins DM-2: the protocol floor is enforced on
// every POLL, not only at registration.
//
// This is the case the DD band's protocol-floor phase left open. An agent
// registers exactly once and then resumes a saved identity on every restart, so
// a runner registered before a server upgrade keeps polling at its old protocol
// and never passes the register-time check again. With the per-feature
// `agent_too_old` gates deleted there was nothing else left to refuse it, and the
// server would hand it fields its binary silently drops — a check-mode run would
// apply for real.
func TestPollRefusesSubFloorProtocol(t *testing.T) {
	svc := newTestService(t)
	id, tok := registerV4(t, svc, "stale")

	// It polls fine at the current protocol: registerV4 registered at the floor.
	if rec := driftPoll(t, svc, id, tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("baseline poll: got %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	// Now make the row look like one registered before the floor was raised —
	// exactly what an in-place server upgrade produces for a running fleet.
	setProtocol(t, svc, id, runnerproto.MinProtocolVersion-1)

	rec := driftPoll(t, svc, id, tok, "")
	if rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("stale-protocol poll: got %d, want 426 (body %s)", rec.Code, rec.Body.String())
	}
	var e struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "protocol_too_old" {
		t.Errorf("error code = %q, want protocol_too_old", e.Code)
	}
	// The message must name BOTH numbers: the operator's next action depends on
	// knowing which agent is behind and how far.
	for _, want := range []string{strconv.Itoa(runnerproto.MinProtocolVersion - 1), strconv.Itoa(runnerproto.MinProtocolVersion)} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("message %q does not name %q", e.Message, want)
		}
	}

	// A refused runner is still ALIVE, not reaped: last_seen_at is refreshed
	// before the check, so the Runners page shows it online-but-refused rather
	// than letting it age into "offline" and look like a different fault.
	var seen string
	if err := svc.db.QueryRow(`SELECT last_seen_at FROM runners WHERE id=?`, id).Scan(&seen); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	if seen == "" {
		t.Error("last_seen_at is empty after a refused poll — the runner would drift into 'offline' and hide the real reason")
	}

	// A row that pre-dates the handshake carries migration 390's DEFAULT 1 (the
	// column is NOT NULL, so there is no "unknown" to be lenient about) and is
	// refused the same way.
	setProtocol(t, svc, id, 1)
	if rec := driftPoll(t, svc, id, tok, ""); rec.Code != http.StatusUpgradeRequired {
		t.Fatalf("pre-handshake row (protocol 1): got %d, want 426", rec.Code)
	}

	// And upgrading the agent fixes it with no operator action on the server:
	// the row goes back to the floor and the next poll is served.
	setProtocol(t, svc, id, runnerproto.ProtocolVersion)
	if rec := driftPoll(t, svc, id, tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("after upgrade: got %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
}
