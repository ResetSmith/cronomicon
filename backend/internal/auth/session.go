package auth

import (
	"crypto/rand"
	"net/http"
	"time"

	"github.com/gorilla/securecookie"
)

const (
	// sessionCookieName carries a version suffix so a deploy can force a one-time
	// re-login by bumping it. The A5 scope-model fix requires this: a session
	// established BEFORE the "*" backfill (migration 630) froze an Identity whose
	// AllowedScopes lack "*", which the post-fix guards read as ZERO access — a
	// previously-unrestricted admin would be locked out until their session cookie
	// expired. Bumping the name (…_v1 → …_v2) makes every stale cookie fail to
	// decode (the name is part of securecookie's HMAC), so everyone re-authenticates
	// once and re-resolves with the backfilled grants. See
	// the scoping-fix plan §5. Trusted-header mode resolves per request and
	// is unaffected.
	//
	// …_v2 → _v3 (RB-14, v0.56.2): Identity gained a Grants field, and the whole
	// Identity is encrypted into this cookie. A session frozen before this release
	// would decode with Grants nil, which is indistinguishable from "resolved to no
	// grants at all" — and that becomes an authorization answer the moment RB-15
	// makes the field authoritative. Bumping the name now means there are no legacy
	// cookies to reason about in THAT release: everyone re-authenticates once here,
	// while the field is still inert and a bad resolution cannot deny anyone.
	// Deliberately paid a release early, for exactly that reason.
	//
	// …_v3 → _v4 (LR-78, v2.3.0): the cookie stops carrying authority. It held the
	// whole Identity, grants expanded at login included; it now holds
	// sessionPayload — who the user is and which groups they are in — and the
	// grants are resolved per request from the snapshot (snapshot.go). A _v3
	// cookie decodes into nothing a _v4 reader wants, so everyone signs in once
	// at the upgrade. That is the last time an RBAC change signs anyone out.
	sessionCookieName = "cronomicon_session_v4"
	// sessionTTL bounds how long a session survives (SU-5): dropped from 12h to 8h so
	// stale access is bounded even between epoch bumps. The session-epoch check
	// (auth.Service) is the primary revocation mechanism; the TTL is the backstop.
	sessionTTL = 8 * time.Hour
)

// sessionPayload is what the cookie carries: identity and group membership as
// the identity provider asserted them at login, and the three session clocks.
// It deliberately has no roles, scopes or grants — see sessionCookieName. A
// field added here is a field an attacker with a stolen cookie keeps for eight
// hours, and one the 4 KB cookie ceiling has to fit.
type sessionPayload struct {
	Email       string
	DisplayName string
	Groups      []string
	IssuedAt    time.Time
	LastSeen    time.Time
	Epoch       int
}

func payloadOf(id Identity) sessionPayload {
	return sessionPayload{
		Email: id.Email, DisplayName: id.DisplayName, Groups: id.Groups,
		IssuedAt: id.IssuedAt, LastSeen: id.LastSeen, Epoch: id.Epoch,
	}
}

// sessionCodec encodes the session payload into a tamper-proof, encrypted
// cookie (T8).
type sessionCodec struct {
	sc     *securecookie.SecureCookie
	secure bool
}

// newSessionCodec builds the codec. If keys are empty, too short, or an invalid
// AES key size, ephemeral random keys are generated so local/dev boots — sessions
// then don't survive a restart, which we surface to the caller via ephemeral=true.
//
// SU-6: the hash key was previously only checked for emptiness, so a short (weak)
// but non-empty CRONOMICON_SESSION_HASH_KEY was passed through to HMAC as-is. Require
// at least 32 bytes (mirroring the block-key validation below); a shorter key is
// substituted with a random one rather than silently weakening session integrity.
func newSessionCodec(hashKey, blockKey []byte, secure bool) (codec *sessionCodec, ephemeral bool) {
	if len(hashKey) < 32 {
		hashKey = randomBytes(32)
		ephemeral = true
	}
	if len(blockKey) != 16 && len(blockKey) != 24 && len(blockKey) != 32 {
		blockKey = randomBytes(32)
		ephemeral = true
	}
	sc := securecookie.New(hashKey, blockKey)
	sc.MaxAge(int(sessionTTL.Seconds()))
	return &sessionCodec{sc: sc, secure: secure}, ephemeral
}

// write stores the session half of id. Its Roles, AllowedScopes and Grants are
// NOT written: read returns an Identity without them, and the caller resolves
// them for the request in hand (Service.readSession).
func (c *sessionCodec) write(w http.ResponseWriter, id Identity) error {
	enc, err := c.sc.Encode(sessionCookieName, payloadOf(id))
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    enc,
		Path:     "/",
		HttpOnly: true, // T8: not readable by JS
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode, // T8
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

func (c *sessionCodec) read(r *http.Request) (Identity, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return Identity{}, false
	}
	var p sessionPayload
	if err := c.sc.Decode(sessionCookieName, cookie.Value, &p); err != nil {
		return Identity{}, false
	}
	return Identity{
		Email: p.Email, DisplayName: p.DisplayName, Groups: p.Groups,
		IssuedAt: p.IssuedAt, LastSeen: p.LastSeen, Epoch: p.Epoch,
	}, true
}

func (c *sessionCodec) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return b
}
