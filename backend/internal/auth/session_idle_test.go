package auth

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// FX-E4 — the sessionPolicy idle cap, which round-tripped through settings for
// its whole life while nothing read it. An operator could set a 30-minute
// timeout, watch it persist, and believe their console locked. These drive the
// real seam: a stored settings row, a real cookie, a real request.

func idleCookieReq(t *testing.T, s *Service, id Identity) *http.Request {
	t.Helper()
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

func setSessionPolicy(t *testing.T, s *Service, jsonBlob string) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO settings (key, value) VALUES ('sessionPolicy', ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, jsonBlob); err != nil {
		t.Fatalf("store policy: %v", err)
	}
	// Bust the cache: tests must not wait out the 30s staleness window.
	s.idlePolicyMu.Lock()
	s.idlePolicyLoaded = time.Time{}
	s.idlePolicyMu.Unlock()
}

func TestIdleTimeoutExpiresAnAbandonedSession(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)

	now := time.Now().UTC()
	// Active five minutes ago: inside the cap, accepted.
	fresh := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-5 * time.Minute)})
	if _, ok := s.readSession(nil, fresh); !ok {
		t.Error("a session active 5 minutes ago was expired by a 30-minute idle cap")
	}
	// Abandoned 31 minutes ago: expired — even though the session cookie itself
	// (8h TTL) is still perfectly valid, which is the entire point of the knob.
	stale := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-31 * time.Minute)})
	if _, ok := s.readSession(nil, stale); ok {
		t.Error("a session idle for 31 minutes survived a 30-minute idle cap — the knob " +
			"an operator can set has done nothing since it shipped")
	}
}

// Activity slides the idle window: the cookie is re-stamped so the next
// request measures from now, not from login.
func TestActivitySlidesTheIdleWindow(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)

	now := time.Now().UTC()
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-10 * time.Minute)})
	rec := httptest.NewRecorder()
	if _, ok := s.readSession(rec, req); !ok {
		t.Fatal("in-window session rejected")
	}
	// The response must carry a refreshed cookie — and the refresh must be a
	// faithful copy with only LastSeen advanced. The first version of this test
	// checked merely that SOME cookie appeared, which a refresh writing
	// Identity{} (or one that re-stamped IssuedAt, quietly extending the 8h
	// ceiling) would also have satisfied.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	var found bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookieName && ck.Value != "" {
			found = true
			req2.AddCookie(ck)
		}
	}
	if !found {
		t.Fatal("no refreshed session cookie on the response — without the slide, a " +
			"continuously active operator is logged out N minutes after LOGIN, which is " +
			"an absolute cap wearing an idle cap's name")
	}
	got, ok := s.codec.read(req2)
	if !ok {
		t.Fatal("the refreshed cookie does not decode")
	}
	if got.Email != "a@b.com" {
		t.Errorf("refreshed identity email = %q — the slide rewrote the identity", got.Email)
	}
	if time.Since(got.LastSeen) > time.Minute {
		t.Errorf("refreshed LastSeen = %s — the slide did not advance it", got.LastSeen)
	}
	if !got.IssuedAt.Equal(now.Add(-2*time.Hour).Truncate(0)) && time.Since(got.IssuedAt) < 90*time.Minute {
		t.Errorf("refreshed IssuedAt = %s — the slide re-stamped login time, which would let "+
			"activity extend the 8h ceiling forever", got.IssuedAt)
	}
}

// The 8-hour ceiling holds even under an active slide. Before this check the
// ceiling existed only as securecookie ageing from the ENCODE timestamp — which
// the slide resets — so enabling the idle cap would have made any
// once-per-window session immortal, stolen cookies included.
func TestSessionCeilingSurvivesTheSlide(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)

	now := time.Now().UTC()
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-9 * time.Hour), LastSeen: now.Add(-time.Minute)})
	if _, ok := s.readSession(nil, req); ok {
		t.Error("a session logged in 9 hours ago survived the 8h ceiling because it was " +
			"recently active — the slide has turned the ceiling off")
	}
}

// reauth=true makes the cap ABSOLUTE: measured from login, activity does not
// extend it. The stricter reading, for installs whose policy says a credential
// may only live so long.
func TestReauthMakesTheCapAbsolute(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":true}`)

	now := time.Now().UTC()
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-31 * time.Minute), LastSeen: now.Add(-time.Minute)})
	if _, ok := s.readSession(nil, req); ok {
		t.Error("with reauth=true a session logged in 31 minutes ago must expire even " +
			"while active — that is what distinguishes re-auth from idle")
	}
}

// FX2-A — ENABLING the cap must not mass-revoke every session in the install.
// Before the fix, the LastSeen slide ran only while the cap was on, so with the
// cap off (the stored default) LastSeen froze at login; the moment an operator
// enabled a timeout, every session older than it — however active — was judged
// against that login-time anchor and revoked at once, the admin who clicked
// Save included.
func TestEnablingTheIdleCapDoesNotMassRevokeActiveSessions(t *testing.T) {
	s := testService(t)

	now := time.Now().UTC()
	// Phase 1: cap OFF. A request flows; the slide must refresh LastSeen even
	// with no policy — that is the half the first version got wrong.
	setSessionPolicy(t, s, `{"timeoutMinutes":0,"reauth":false}`)
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-2 * time.Hour)})
	rec := httptest.NewRecorder()
	if _, ok := s.readSession(rec, req); !ok {
		t.Fatal("cap-off session rejected")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	var slid bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookieName && ck.Value != "" {
			slid = true
			req2.AddCookie(ck)
		}
	}
	if !slid {
		t.Fatal("no refreshed cookie with the cap OFF — LastSeen freezes at login and " +
			"enabling the cap later judges every session against that stale anchor")
	}
	// Phase 2: the operator enables a 30-minute cap. The active session — whose
	// cookie now carries the slid LastSeen — must survive the next request.
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)
	if _, ok := s.readSession(nil, req2); !ok {
		t.Error("a continuously active session was revoked the moment the idle cap was " +
			"enabled — the mass-logout-on-enable transition")
	}
}

// FX2-A — the enable transition graces sessions whose cookie has NOT been
// re-stamped yet (LastSeen stale through no fault of the user): the idle clock
// starts at the enable instant, not retroactively.
func TestEnablingTheIdleCapAnchorsAtTheEnableInstant(t *testing.T) {
	s := testService(t)

	now := time.Now().UTC()
	// Observe the cap OFF once, so the flip below is a real 0 → N transition
	// rather than the first load after boot.
	setSessionPolicy(t, s, `{"timeoutMinutes":0,"reauth":false}`)
	warm := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-time.Minute)})
	if _, ok := s.readSession(nil, warm); !ok {
		t.Fatal("cap-off warm-up rejected")
	}
	// Flip the cap on. A session idle for 40 minutes at the flip must survive:
	// its clock starts NOW and it has the full 30 minutes to make a request.
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-40 * time.Minute)})
	if _, ok := s.readSession(nil, req); !ok {
		t.Error("a session 40 minutes idle at the enable instant was revoked immediately — " +
			"the enable transition judged it retroactively instead of starting its clock now")
	}
}

// FX2-A — but a service that BOOTS with the cap already on grants no such
// grace: the transition stamp is for the operator's flip, not for restarts,
// which would otherwise hand every idle session a fresh window each deploy.
func TestBootWithTheCapOnIsNotAnEnableTransition(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)

	now := time.Now().UTC()
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-40 * time.Minute)})
	if _, ok := s.readSession(nil, req); ok {
		t.Error("a 40-minute-idle session survived a 30-minute cap on a fresh boot — the " +
			"enable-transition grace must not fire on restart")
	}
}

// FX2-A — flipping reauth OFF has the same shape: while reauth was on the old
// code suppressed the slide, so LastSeen went stale and the flip to idle mode
// mass-revoked. With the unconditional slide the anchor stays fresh throughout.
func TestDisablingReauthDoesNotMassRevokeActiveSessions(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":480,"reauth":true}`)

	now := time.Now().UTC()
	// Active under reauth: the slide must still refresh LastSeen.
	req := idleCookieReq(t, s, Identity{Email: "a@b.com",
		IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-2 * time.Hour)})
	rec := httptest.NewRecorder()
	if _, ok := s.readSession(rec, req); !ok {
		t.Fatal("in-window reauth session rejected")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	var slid bool
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == sessionCookieName && ck.Value != "" {
			slid = true
			req2.AddCookie(ck)
		}
	}
	if !slid {
		t.Fatal("no refreshed cookie under reauth — LastSeen goes stale and flipping " +
			"reauth off mass-revokes active sessions")
	}
	// Flip to idle mode with a 30-minute cap: the active session survives.
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)
	if _, ok := s.readSession(nil, req2); !ok {
		t.Error("an active session was revoked when reauth flipped off — the stale-anchor " +
			"transition in its second form")
	}
}

// FX2-A — a transient failure to READ the policy must not mint a fresh
// enable-grace. The read path fails open for the duration of the error (a read
// error may never lock everyone out), but if recovery counted as an operator
// turning the cap back on, every stale session — stolen cookies included —
// would be granted a full timeout of immunity, and each recurrence would renew
// it.
func TestAPolicyReadErrorDoesNotMintAFreshGrace(t *testing.T) {
	s := testService(t)
	setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)

	now := time.Now().UTC()
	stale := func() *http.Request {
		return idleCookieReq(t, s, Identity{Email: "a@b.com",
			IssuedAt: now.Add(-2 * time.Hour), LastSeen: now.Add(-90 * time.Minute)})
	}
	// Baseline: the cap is live and the abandoned session is revoked.
	if _, ok := s.readSession(nil, stale()); ok {
		t.Fatal("baseline: a 90-minute-idle session survived a 30-minute cap")
	}

	// Simulate an unreadable settings row (a busy/locked DB) by pointing the
	// service at a closed handle for one refresh, then restoring it.
	good := s.db
	bad, err := sql.Open("sqlite3", "file:fx2a-closed?mode=memory")
	if err != nil {
		t.Fatalf("open probe db: %v", err)
	}
	bad.Close() // every query on this handle now errors
	s.idlePolicyMu.Lock()
	s.idlePolicyLoaded = time.Time{}
	s.idlePolicyMu.Unlock()
	s.db = bad
	if _, ok := s.readSession(nil, stale()); !ok {
		t.Error("during an unreadable policy read the cap should fail OPEN — a read error " +
			"must never lock everyone out")
	}

	// Recovery: the same stale session must still be revoked. If the recovery
	// were treated as an OFF → ON transition it would be graced instead.
	s.db = good
	s.idlePolicyMu.Lock()
	s.idlePolicyLoaded = time.Time{}
	s.idlePolicyMu.Unlock()
	if _, ok := s.readSession(nil, stale()); ok {
		t.Error("after the policy read recovered, a 90-minute-idle session was admitted — a " +
			"transient DB error masqueraded as the operator re-enabling the cap and bought " +
			"every stale session a fresh window")
	}
}

// No policy row, zero timeout, or a legacy cookie with no LastSeen: nothing new
// expires. The deploy that introduces the field must not idle-out every live
// session at once.
func TestIdleCapFailsOpen(t *testing.T) {
	s := testService(t)

	now := time.Now().UTC()
	t.Run("no policy stored", func(t *testing.T) {
		req := idleCookieReq(t, s, Identity{Email: "a@b.com",
			IssuedAt: now.Add(-7 * time.Hour), LastSeen: now.Add(-6 * time.Hour)})
		if _, ok := s.readSession(nil, req); !ok {
			t.Error("with no policy stored, an old-but-valid session was expired")
		}
	})
	t.Run("zero timeout disables", func(t *testing.T) {
		setSessionPolicy(t, s, `{"timeoutMinutes":0,"reauth":false}`)
		req := idleCookieReq(t, s, Identity{Email: "a@b.com",
			IssuedAt: now.Add(-7 * time.Hour), LastSeen: now.Add(-6 * time.Hour)})
		if _, ok := s.readSession(nil, req); !ok {
			t.Error("timeoutMinutes=0 must mean no cap, matching every other zero-disables knob")
		}
	})
	t.Run("legacy cookie anchors at IssuedAt", func(t *testing.T) {
		setSessionPolicy(t, s, `{"timeoutMinutes":30,"reauth":false}`)
		req := idleCookieReq(t, s, Identity{Email: "a@b.com", IssuedAt: now.Add(-5 * time.Minute)})
		if _, ok := s.readSession(nil, req); !ok {
			t.Error("a pre-FX-E4 cookie (no LastSeen) from 5 minutes ago was expired — the " +
				"deploy would log out every live session at once")
		}
	})
}
