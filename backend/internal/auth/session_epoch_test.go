package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSessionEpochRevocation (SU-5): a session cookie stamped at the current epoch
// is accepted; after BumpSessionEpoch (an RBAC change) the same cookie is revoked;
// a fresh login at the new epoch is accepted again. Also confirms graceful upgrade —
// an epoch-0 cookie is valid while the epoch is still 0 (no forced re-login).
func TestSessionEpochRevocation(t *testing.T) {
	s := testService(t)
	s.loadSessionEpoch(context.Background())
	if s.currentSessionEpoch() != 0 {
		t.Fatalf("initial epoch = %d, want 0", s.currentSessionEpoch())
	}

	cookieFor := func(id Identity) *http.Request {
		rec := httptest.NewRecorder()
		if err := s.codec.write(rec, id); err != nil {
			t.Fatalf("write cookie: %v", err)
		}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
		return req
	}

	// A session at the current (0) epoch — also the legacy-cookie case — is accepted.
	stale := cookieFor(Identity{Email: "a@b.com", Epoch: s.currentSessionEpoch()})
	if _, ok := s.readSession(nil, stale); !ok {
		t.Error("current-epoch (and legacy epoch-0) session should be accepted")
	}

	// An RBAC change bumps the epoch; the prior session is now stale.
	s.BumpSessionEpoch(context.Background())
	if s.currentSessionEpoch() != 1 {
		t.Fatalf("epoch after bump = %d, want 1", s.currentSessionEpoch())
	}
	if _, ok := s.readSession(nil, stale); ok {
		t.Error("stale-epoch session must be revoked after a bump")
	}

	// A re-login at the new epoch is accepted again.
	fresh := cookieFor(Identity{Email: "a@b.com", Epoch: s.currentSessionEpoch()})
	if _, ok := s.readSession(nil, fresh); !ok {
		t.Error("re-login at the new epoch should be accepted")
	}
}
