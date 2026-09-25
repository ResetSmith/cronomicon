package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// TestPollStatusToSentinelError pins the poll status→sentinel mapping the
// lifecycle loop switches on. The 401 case is the self-heal fix: a rejected
// runner token (deleted runner / reset token store) must surface as
// errIdentityRejected so the agent re-registers instead of polling a dead token
// forever. 404/204 are covered too so the mapping can't silently regress.
func TestPollStatusToSentinelError(t *testing.T) {
	cases := []struct {
		name string
		code int
		want error
	}{
		{"401 token rejected → re-register", http.StatusUnauthorized, errIdentityRejected},
		{"404 reaped → re-register", http.StatusNotFound, errReaped},
		{"204 no content → no work", http.StatusNoContent, errNoWork},
		// DM-2: the server now enforces its protocol floor on every poll, not
		// only at registration. 426 must reach the loop as its OWN sentinel —
		// mapping it onto errReaped/errIdentityRejected would make the agent
		// discard a perfectly good identity and re-register, which declares the
		// same old protocol and is refused identically.
		{"426 protocol too old → keep identity", http.StatusUpgradeRequired, errProtocolTooOld},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()

			c, err := NewClient(srv.URL, "")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = c.Poll(context.Background(), Identity{ID: "r1", APIKey: "crn_run_x"}, "", 0)
			if !errors.Is(err, tc.want) {
				t.Errorf("Poll on HTTP %d returned %v, want %v", tc.code, err, tc.want)
			}
		})
	}
}

// TestPollProtocolTooOldKeepsIdentity pins the RECOVERY for a 426, which is the
// half a status-mapping test cannot show: the agent must keep polling with the
// identity it has. Re-registering would be the wrong instinct (it is what a 404
// and a 401 both do) because the refusal is about this BINARY's wire protocol,
// not about who the runner is — a fresh registration declares the same number
// and is refused the same way, having thrown away a valid identity on the way.
func TestPollProtocolTooOldKeepsIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUpgradeRequired)
		_, _ = w.Write([]byte(`{"code":"protocol_too_old","message":"runner speaks protocol version 11; this server requires 12"}`))
	}))
	defer srv.Close()

	idFile := filepath.Join(t.TempDir(), "identity.json")
	id := Identity{ID: "r1", APIKey: "crn_run_x"}
	if err := saveIdentity(idFile, id); err != nil {
		t.Fatalf("saveIdentity: %v", err)
	}

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	a := &Agent{
		cfg:    Config{ServerURL: srv.URL, IdentityFile: idFile, Name: "stale"},
		client: c,
		id:     id,
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	a.pollOnce(context.Background())

	// The identity file survives: this is the assertion that would fail if
	// anyone routed 426 through reregister().
	got, err := loadIdentity(idFile)
	if err != nil {
		t.Fatalf("loadIdentity after a 426: %v", err)
	}
	if got == nil || got.ID != "r1" {
		t.Fatalf("identity after a 426 = %v, want it untouched — a version gap must not cost the runner its identity", got)
	}
	if a.id.ID != "r1" {
		t.Errorf("in-memory identity = %q, want r1 unchanged", a.id.ID)
	}

	// And the server's own sentence reaches the agent, because it is the one
	// place both version numbers appear.
	if err := func() error { _, e := c.Poll(context.Background(), id, "", 0); return e }(); err == nil ||
		!strings.Contains(err.Error(), "requires 12") {
		t.Errorf("poll error = %v, want it to carry the server's message naming both versions", err)
	}
}
