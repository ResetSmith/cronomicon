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
	sessionCookieName = "cronomicon_session_v3"
	// sessionTTL bounds how long a session survives (SU-5): dropped from 12h to 8h so
	// stale access is bounded even between epoch bumps. The session-epoch check
	// (auth.Service) is the primary revocation mechanism; the TTL is the backstop.
	sessionTTL = 8 * time.Hour
)

// sessionCodec encodes the Identity into a tamper-proof, encrypted cookie (T8).
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

func (c *sessionCodec) write(w http.ResponseWriter, id Identity) error {
	enc, err := c.sc.Encode(sessionCookieName, id)
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
	var id Identity
	if err := c.sc.Decode(sessionCookieName, cookie.Value, &id); err != nil {
		return Identity{}, false
	}
	return id, true
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
