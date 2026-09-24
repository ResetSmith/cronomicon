package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// headerNames are the configurable Remote-* header names the trusted-header
// provider reads (defaults are the Remote-* names Authelia and similar proxies send).
type headerNames struct {
	user, email, name, groups string
}

// Service holds the auth dependencies: the session codec, and (when configured)
// the OIDC relying-party machinery. When OIDC is unconfigured or discovery
// failed, login is disabled but the rest of the server still runs (degraded).
type Service struct {
	db       *sql.DB
	log      *slog.Logger
	codec    *sessionCodec
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	// oidcHTTPClient is the SU-7 egress-guarded HTTP client used for OIDC
	// discovery, lazy JWKS refresh (via oidc.ClientContext) and token exchange
	// (via oauth2.HTTPClient). Nil unless the OIDC relying party is wired.
	oidcHTTPClient *http.Client
	enabled        bool
	devAuth        bool // local dev login bypass (config.DevAuth) — never set in prod

	// Trusted Header SSO (Phase A). mode selects how RequireSession resolves
	// identity; in trusted-header mode it is derived from headers on every
	// request (no app session cookie). trustedProxies is the allowlist that
	// gates whether Remote-* headers are honored at all (A.3).
	mode                string
	headers             headerNames
	trustedProxies      []*net.IPNet
	bootstrapAdminGroup string
	logoutRedirectURL   string

	// recent_logins (A3.2 Honest View) upsert throttle: in trusted-header mode
	// identity is resolved per request, so we record at most once per user per
	// window rather than on every request (A.5).
	loginSeenMu sync.Mutex
	loginSeen   map[string]time.Time

	// sessionEpoch is the in-memory mirror of auth_session_epoch (SU-5). Loaded at
	// boot, bumped in-process on any RBAC change, and compared against a cookie's
	// stamped epoch per request — server-side session revocation without a DB read
	// on the hot path.
	sessionEpoch atomic.Int64

	// idlePolicy caches the sessionPolicy settings blob (FX-E4), refreshed at
	// most every idlePolicyRefresh so the hot path pays no DB read. The same
	// keep-it-off-the-hot-path shape as sessionEpoch, but time-refreshed rather
	// than push-updated: settings writes happen in the api package, which must
	// not need a back-reference into auth just to poke a cache, and a policy
	// change taking up to thirty seconds to bite is invisible next to timeouts
	// measured in minutes.
	idlePolicyMu     sync.Mutex
	idlePolicy       sessionIdlePolicy
	idlePolicyLoaded time.Time
	// idleEnabledAt is when the cached timeout was last observed going from
	// zero to non-zero (FX2-A). Judging idleness against max(LastSeen,
	// idleEnabledAt) makes ENABLING the cap start everyone's idle clock at the
	// enable instant instead of retroactively — the third member of the
	// mass-logout-on-transition family (see the legacy-cookie and reauth notes
	// at the judgment site). In-memory only, deliberately: after a restart the
	// unconditional slide has been keeping anchors fresh for active sessions,
	// and revoking genuinely idle ones is the cap doing its job.
	idleEnabledAt time.Time
	// idlePolicySeen distinguishes the first load after boot from a refresh — a
	// separate bool rather than idlePolicyLoaded.IsZero(), because zeroing the
	// loaded time is also the cache-bust idiom (tests, and any future forced
	// refresh), and a bust must not make the next load read as boot.
	idlePolicySeen bool
	// idlePolicyObserved is the last policy actually READ from settings, which
	// is not the same as the last one served: an unreadable row serves a zero
	// policy (fail-open) without observing one. Transitions are judged on this,
	// so a transient DB error cannot masquerade as the operator disabling and
	// re-enabling the cap.
	idlePolicyObserved sessionIdlePolicy
}

// sessionIdlePolicy is the enforced shape of settings' sessionPolicy (FX-E4).
type sessionIdlePolicy struct {
	// timeout is the cap; zero disables it (the stored default).
	timeout time.Duration
	// reauth selects what the cap measures. False: IDLE — the clock runs from
	// the last request and activity slides it, so a session dies only when
	// abandoned. True: ABSOLUTE — the clock runs from login regardless of
	// activity, forcing a periodic full re-authentication; the stricter reading,
	// for installs whose policy says a credential may only live so long.
	reauth bool
}

// loginRecordWindow throttles recent_logins upserts per user (A.5).
const loginRecordWindow = 5 * time.Minute

// NewService wires the auth subsystem. It never hard-fails on OIDC problems so
// that migrations/health stay reachable; instead it logs and disables login.
func NewService(ctx context.Context, cfg *config.Config, db *sql.DB, log *slog.Logger) *Service {
	// SU-6: outside dev, a non-base64 key is rejected rather than used as raw bytes
	// (decodeKey), and a hash key shorter than 32 bytes is substituted rather than
	// weakening HMAC (newSessionCodec) — both surface via ephemeral=true below.
	allowRawKeys := cfg.DevAuth
	codec, ephemeral := newSessionCodec(decodeKey(cfg.SessionHashKey, allowRawKeys), decodeKey(cfg.SessionBlockKey, allowRawKeys), cfg.CookieSecure)
	if ephemeral {
		log.Warn("session keys unset, too short, or invalid — using ephemeral keys; sessions won't survive restart " +
			"(set AMADEUS_SESSION_HASH_KEY and AMADEUS_SESSION_BLOCK_KEY to base64-encoded 32-byte values)")
	}

	mode := cfg.AuthMode
	if mode == "" {
		mode = config.AuthModeTrustedHeader
	}
	s := &Service{
		db:      db,
		log:     log,
		codec:   codec,
		devAuth: cfg.DevAuth,
		mode:    mode,
		headers: headerNames{
			// Default to the conventional Remote-* header names when unset, so a Config built
			// directly (not via config.Load) still resolves identity correctly.
			user:   orDefault(cfg.TrustedHeaderUser, "Remote-User"),
			email:  orDefault(cfg.TrustedHeaderEmail, "Remote-Email"),
			name:   orDefault(cfg.TrustedHeaderName, "Remote-Name"),
			groups: orDefault(cfg.TrustedHeaderGroups, "Remote-Groups"),
		},
		trustedProxies:      cfg.TrustedProxyNets(),
		bootstrapAdminGroup: cfg.BootstrapAdminGroup,
		logoutRedirectURL:   cfg.LogoutRedirectURL,
		loginSeen:           make(map[string]time.Time),
	}
	log.Info("auth mode active", "mode", mode)
	s.loadSessionEpoch(ctx) // SU-5: mirror the persisted session epoch into memory

	if mode == config.AuthModeTrustedHeader && len(s.trustedProxies) == 0 && !cfg.DevAuth {
		// Load() refuses to boot in this state; this is belt-and-braces for
		// callers that build Config directly (tests). Without an allowlist no
		// peer is trusted, so all Remote-* headers are stripped and every
		// request fails closed (401) — safe, if non-functional.
		log.Warn("trusted-header mode with no AMADEUS_TRUSTED_PROXIES — all identity " +
			"headers will be stripped and every operator request will be unauthorized")
	}
	if s.bootstrapAdminGroup != "" {
		log.Warn("⚠️  BOOTSTRAP ADMIN ENABLED (AMADEUS_BOOTSTRAP_ADMIN_GROUP="+s.bootstrapAdminGroup+") — "+
			"any user in this group is granted admin regardless of ad_group_mappings. "+
			"Seed your real group→role mappings in Settings, then REMOVE this var and redeploy.",
			"group", s.bootstrapAdminGroup)
	} else if !cfg.DevAuth {
		// PP-B1: with server-side RBAC now enforced, admin-gated routes (roles,
		// settings, secrets, publish) require the caller to resolve to admin. If
		// no bootstrap group is set AND no admin grant exists, no one can administer
		// the system — warn loudly so first-boot lockout is obvious.
		//
		// v0.57.7: this counted ad_group_mappings, which has decided nothing since
		// v0.56.5 — so it was wrong in BOTH directions. An instance with admin
		// grants and no legacy rows was told it was locked out when it was fine;
		// an instance with legacy admin rows and no grants was genuinely locked out
		// and this stayed silent. A lockout warning that reads the wrong table is
		// worse than none, because it is trusted at exactly the moment it misleads.
		var admins int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM access_grants WHERE lower(role) = ?`, AdminRole).Scan(&admins)
		if admins == 0 {
			log.Warn("⚠️  NO ADMIN CONFIGURED — RBAC is enforced but no admin access grant exists " +
				"and AMADEUS_BOOTSTRAP_ADMIN_GROUP is unset. No operator can reach admin-gated routes. " +
				"Set AMADEUS_BOOTSTRAP_ADMIN_GROUP for the first login, then add an admin Access Grant in Settings.")
		}
	}

	if cfg.DevAuth {
		log.Warn("⚠️  DEV AUTH ENABLED (AMADEUS_DEV_AUTH=true) — /api/v1/auth/dev-login " +
			"mints a synthetic admin session with NO identity-provider check. For local preview " +
			"only; never enable this in a deployed/production environment.")
	}

	// OIDC relying-party machinery is only wired in oidc mode (the optional
	// path). In trusted-header mode the proxy authenticates, so we never mount
	// /login or /callback and don't attempt issuer discovery.
	if mode != config.AuthModeOIDC {
		return s
	}
	if !cfg.OIDC.Enabled() {
		log.Warn("OIDC mode selected but OIDC not configured — operator login disabled (set AMADEUS_OIDC_*)")
		return s
	}

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// SU-7: route OIDC discovery, the verifier's lazy JWKS refresh, and the token
	// exchange through an SSRF egress-guarded client. oidc.ClientContext threads gc
	// into discovery AND the keyset the Verifier fetches on first use; we also stash
	// it on the Service so Callback's token exchange reuses the same guarded client.
	policy := httpx.EgressPolicy{AllowPrivate: cfg.OutboundAllowPrivate, AllowLoopback: cfg.OutboundAllowLoopback}
	gc := httpx.SafeClient(10*time.Second, policy)
	s.oidcHTTPClient = gc
	dctx = oidc.ClientContext(dctx, gc)
	provider, err := oidc.NewProvider(dctx, cfg.OIDC.Issuer)
	if err != nil {
		// Degrade: the identity provider is hard-required for real use (T12), but discovery
		// flakiness at boot shouldn't block the process or migrations. /readyz
		// reflects this when OIDC is configured (see Enabled).
		log.Error("OIDC discovery failed — login disabled until issuer reachable",
			"issuer", cfg.OIDC.Issuer, "error", err)
		return s
	}
	s.oauth = &oauth2.Config{
		ClientID:     cfg.OIDC.ClientID,
		ClientSecret: cfg.OIDC.ClientSecret,
		RedirectURL:  cfg.OIDC.RedirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}
	s.verifier = provider.Verifier(&oidc.Config{ClientID: cfg.OIDC.ClientID})
	s.enabled = true
	log.Info("OIDC relying party ready", "issuer", cfg.OIDC.Issuer)
	return s
}

// Enabled reports whether the OIDC login flow is live.
func (s *Service) Enabled() bool { return s.enabled }

// OIDCMode reports whether the server runs the OIDC relying-party flow (so the
// router mounts /login + /callback only then).
func (s *Service) OIDCMode() bool { return s.mode == config.AuthModeOIDC }

// RequireSession gates operator endpoints: it resolves the operator Identity and
// attaches it to the request context. In oidc mode the identity comes from the
// session cookie (T8); in trusted-header mode it is derived from the forward-auth
// Remote-* headers on every request (A.4 — stateless, no app session cookie).
func (s *Service) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.authenticate(w, r)
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		s.touchCSRF(w, r) // ensure the SPA always has a CSRF token to submit
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// authenticate resolves the operator Identity for a request per the active mode.
// The dev-login session cookie is honored in any mode so the local preview
// bypass keeps working without a proxy (A.8).
func (s *Service) authenticate(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	if s.devAuth {
		if id, ok := s.readSession(w, r); ok {
			return id, true
		}
	}
	if s.mode == config.AuthModeOIDC {
		return s.readSession(w, r)
	}
	// Trusted-header mode resolves roles/scopes from the DB on every request, so it
	// is inherently epoch-safe — no session to revoke.
	return s.identityFromHeaders(r.Context(), r)
}

// readSession decodes the session cookie and enforces the SU-5 epoch: a cookie
// stamped below the current epoch — a session issued before an RBAC change — is
// treated as unauthenticated, forcing a re-login that re-resolves roles/scopes.
// The w parameter exists for the FX-E4 sliding stamp: refreshing LastSeen means
// re-writing the cookie, and only the request that crosses the refresh interval
// pays for it. Callers that have no response to write may pass nil and simply
// forgo the slide for that request.
func (s *Service) readSession(w http.ResponseWriter, r *http.Request) (Identity, bool) {
	id, ok := s.codec.read(r)
	if !ok {
		return Identity{}, false
	}
	if id.Epoch < s.currentSessionEpoch() {
		// LU-9: an epoch bump silently 401s every session issued before it, which
		// reads to the operator as "I was randomly logged out". Recording it is
		// what makes that diagnosable. Throttled per actor: the SPA will keep
		// retrying with the same dead cookie until the user re-logs in.
		s.auditThrottled(r.Context(), r, "revoked:"+id.Email, auditlog.AuthEventParams{
			Kind: auditlog.AuthSessionRevoked, Outcome: auditlog.OutcomeFailure,
			Actor: id.Email, Reason: "session_epoch_advanced",
		})
		return Identity{}, false
	}

	// FX-E4 — the 8-hour ceiling, now enforced in code rather than delegated to
	// cookie ageing. It always held before only as a side effect: securecookie
	// ages a cookie from its ENCODE timestamp, so the ceiling was real while
	// nothing ever re-encoded — and the idle slide below re-encodes. Without this
	// check, enabling the idle cap (a TIGHTENING action) would have made any
	// session touched once per window live forever, stolen cookies included,
	// exactly when policy is strictest. IssuedAt is HMAC-protected and never
	// re-stamped by the slide, so it is the one clock a refresh cannot move.
	if !id.IssuedAt.IsZero() && time.Since(id.IssuedAt) > sessionTTL {
		s.auditThrottled(r.Context(), r, "ceiling:"+id.Email, auditlog.AuthEventParams{
			Kind: auditlog.AuthSessionRevoked, Outcome: auditlog.OutcomeFailure,
			Actor: id.Email, Reason: "session_ttl_ceiling",
		})
		return Identity{}, false
	}

	// FX-E4 — the sessionPolicy idle cap, enforced at last. The knob has been
	// settable (and persisted, and documented) since General Settings shipped,
	// and nothing read it: an operator could set a 30-minute idle timeout, watch
	// it round-trip, and believe their console locked. A security control that
	// silently does nothing is worse than an absent one.
	pol, enabledAt := s.sessionIdlePolicyNow(r.Context())
	if pol.timeout > 0 {
		anchor := id.LastSeen
		if pol.reauth || anchor.IsZero() {
			// Absolute mode measures from login; a legacy cookie (no LastSeen)
			// anchors there too rather than being idled out by the deploy that
			// introduced the field.
			anchor = id.IssuedAt
		}
		// FX2-A — the ENABLE transition anchors at the enable instant. Without
		// this, flipping the cap on judges every session against an anchor from
		// before the cap existed and mass-revokes the whole install at once —
		// including the admin who clicked Save. (Idle mode only: absolute mode
		// measures from login by definition, and its anchor was always live.)
		if !pol.reauth && enabledAt.After(anchor) {
			anchor = enabledAt
		}
		if time.Since(anchor) > pol.timeout {
			s.auditThrottled(r.Context(), r, "idle:"+id.Email, auditlog.AuthEventParams{
				Kind: auditlog.AuthSessionRevoked, Outcome: auditlog.OutcomeFailure,
				Actor: id.Email, Reason: "session_idle_timeout",
			})
			return Identity{}, false
		}
	}
	// FX2-A — slide UNCONDITIONALLY, at most once a minute (the cookie write is
	// the cost, and a per-request rewrite would also make every response
	// Set-Cookie). The first version slid only while the cap was on and idle-mode,
	// which froze LastSeen at login for everyone else — so the cap-off → cap-on
	// and reauth-on → reauth-off transitions both judged active sessions against
	// a login-time anchor and mass-revoked them. Keeping the anchor fresh at all
	// times is what makes it trustworthy the moment any policy starts reading it.
	// The 8h ceiling above is unaffected by design: it anchors on IssuedAt, which
	// this slide never touches — do NOT merge the two anchors.
	//
	// Residual, accepted: a session minted before this fix DEPLOYS onto an
	// install that already has the cap enabled still carries a stale LastSeen for
	// its first judgment. No released deployment has the cap on (it shipped in
	// v1.0.5, unreleased), so the window is theoretical.
	if w != nil && time.Since(id.LastSeen) > time.Minute {
		id.LastSeen = time.Now().UTC()
		_ = s.codec.write(w, id)
	}
	return id, true
}

// sessionIdlePolicyNow returns the cached idle policy, refreshing from settings
// when stale (FX-E4), plus the instant the timeout was last observed going from
// zero to non-zero (FX2-A; zero time when it has been on since boot or is off).
func (s *Service) sessionIdlePolicyNow(ctx context.Context) (sessionIdlePolicy, time.Time) {
	s.idlePolicyMu.Lock()
	defer s.idlePolicyMu.Unlock()
	if time.Since(s.idlePolicyLoaded) < idlePolicyRefresh {
		return s.idlePolicy, s.idleEnabledAt
	}
	firstLoad := !s.idlePolicySeen
	s.idlePolicySeen = true
	s.idlePolicyLoaded = time.Now()
	// prevObserved is the last policy this cache actually READ from settings —
	// not necessarily the last one it served. The distinction is load-bearing:
	// an unreadable settings row serves a zero policy (fail-open, below) but
	// must not read as "the operator turned the cap off", or the next successful
	// refresh would see 0 → N and mint a fresh enable-grace, extending a
	// transient DB error into a full timeout of weakened enforcement for every
	// session — including the stolen cookie the cap exists to cut off.
	prevObserved := s.idlePolicyObserved
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'sessionPolicy'`).Scan(&raw); err != nil {
		// UNREADABLE: fail open for the duration of the error, for the same reason
		// the epoch does — a read error can only FAIL to expire a session, never
		// wrongly lock everyone out — but leave idlePolicyObserved alone so
		// recovery is not mistaken for an operator's flip.
		s.idlePolicy = sessionIdlePolicy{}
		return s.idlePolicy, s.idleEnabledAt
	}
	var sp struct {
		TimeoutMinutes int  `json:"timeoutMinutes"`
		Reauth         bool `json:"reauth"`
	}
	// ABSENT or malformed rows ARE an observation: no policy stored means no cap,
	// which is the shipped default and a genuine state to transition out of.
	observed := sessionIdlePolicy{}
	if raw.Valid && json.Unmarshal([]byte(raw.String), &sp) == nil && sp.TimeoutMinutes > 0 {
		observed = sessionIdlePolicy{timeout: time.Duration(sp.TimeoutMinutes) * time.Minute, reauth: sp.Reauth}
	}
	s.idlePolicy = observed
	s.idlePolicyObserved = observed
	// FX2-A — stamp the OFF → ON transition so the judgment can grace it. Only
	// that edge: a timeout tightened from 60m to 30m keeps its anchor, because
	// the sessions it now expires were idle under a live cap the whole time. The
	// FIRST load after boot is not a transition either — a cap that was already
	// on must not hand every idle session a fresh window at each restart.
	if !firstLoad && prevObserved.timeout == 0 && observed.timeout > 0 {
		s.idleEnabledAt = time.Now().UTC()
	}
	return s.idlePolicy, s.idleEnabledAt
}

// idlePolicyRefresh bounds how stale the cached sessionPolicy may be.
const idlePolicyRefresh = 30 * time.Second

// currentSessionEpoch returns the in-memory global session epoch (SU-5).
func (s *Service) currentSessionEpoch() int { return int(s.sessionEpoch.Load()) }

// loadSessionEpoch reads the persisted global session epoch into memory (SU-5). A
// missing/unreadable row leaves it at 0 — fail-open on a read error is safe: it can
// only FAIL to revoke, never wrongly lock everyone out.
func (s *Service) loadSessionEpoch(ctx context.Context) {
	var e int64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM auth_session_epoch WHERE id = 1`).Scan(&e); err != nil {
		s.log.Warn("session epoch load failed; starting at 0", "error", err)
		e = 0
	}
	s.sessionEpoch.Store(e)
}

// BumpSessionEpoch increments the global session epoch (SU-5), invalidating every
// OIDC session issued before now. Call on any RBAC change (AD-group mapping / scope
// grant). A DB failure is logged and the in-memory epoch is left unchanged.
func (s *Service) BumpSessionEpoch(ctx context.Context) {
	if _, err := s.db.ExecContext(ctx, `UPDATE auth_session_epoch SET epoch = epoch + 1 WHERE id = 1`); err != nil {
		s.log.Error("session epoch bump failed", "error", err)
		return
	}
	var e int64
	if err := s.db.QueryRowContext(ctx, `SELECT epoch FROM auth_session_epoch WHERE id = 1`).Scan(&e); err != nil {
		s.log.Error("session epoch reload failed after bump", "error", err)
		return
	}
	// Advance the in-memory mirror monotonically: under two concurrent bumps a plain
	// Store could regress the mirror one epoch below the DB (a brief under-revocation);
	// a CAS-max ensures the epoch only ever moves forward.
	for {
		cur := s.sessionEpoch.Load()
		if e <= cur || s.sessionEpoch.CompareAndSwap(cur, e) {
			break
		}
	}
	s.log.Info("session epoch bumped — prior OIDC sessions revoked", "epoch", e)
}

// RevokeOtherSessions bumps the epoch (revoking every session issued before now,
// SU-5) but re-issues the ACTING operator's OWN cookie at the new epoch, so the
// admin making an RBAC change is not logged out of their own session. The re-issued
// cookie keeps the actor's existing identity (their fresh grants apply on next
// login). In trusted-header mode there is no cookie to re-issue and identity is
// resolved per request, so nothing to do beyond the bump.
func (s *Service) RevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	s.BumpSessionEpoch(r.Context())
	if id, ok := s.codec.read(r); ok {
		id.Epoch = s.currentSessionEpoch()
		_ = s.codec.write(w, id)
	}
}

// RequireRole gates an endpoint behind a named role. Must be wrapped inside
// RequireSession so the identity is present.
func (s *Service) RequireRole(role string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := IdentityFrom(r.Context())
		if !ok {
			httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if !id.HasRole(role) {
			s.AuditDenied(r, id.Email, "insufficient_role", role, "")
			httpx.Fail(w, http.StatusForbidden, "forbidden", "requires role: "+role)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// decodeKey decodes a base64-encoded session key. A non-base64 value is tolerated
// as raw key bytes ONLY when allowRaw is true (dev convenience). In a production
// auth mode (allowRaw=false, SU-6) a non-base64 value is almost certainly a typo'd
// passphrase, so it is rejected (returns nil → newSessionCodec substitutes a
// random ephemeral key and warns) rather than silently becoming a short,
// low-entropy raw key derived from the passphrase text.
func decodeKey(s string, allowRaw bool) []byte {
	if s == "" {
		return nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	if allowRaw {
		return []byte(s)
	}
	return nil
}
