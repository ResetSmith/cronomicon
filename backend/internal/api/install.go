package api

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// One-click install script (runner provisioning plan 2 Phase 2, D2: 2A).
//
// GET /install/{token} serves the published runner-install.sh with this
// server's URL and the registration token baked in, so the whole install is a
// single flagless pipe:
//
//	curl -fsSL https://amadeus.example.com/install/amt_reg_… | sudo bash
//
// A DUMB endpoint by design: it does NOT read the DB or validate the token
// against it. Registration remains the sole enforcement point (single-use,
// atomic, audited) — serving a script for a dead token yields a clean
// registration failure on the host, and the endpoint leaks nothing about token
// validity (no oracle). It only syntax-checks the token so a random path can't
// bake garbage into the script. Unauthenticated at root by design, same posture
// as /agents/ and /runner-install.sh: the token in the path IS the credential,
// and the script is not a secret (see deploy/security-review.md, incl. the
// token-in-URL access-log hygiene note).
//
// Three substitutions turn the generic script into a personalized one:
//   - SERVER_URL="" → the reconstructed external base URL of THIS request
//   - REG_TOKEN=""  → the path token
//   - DOWNLOAD=0    → 1, so the flagless pipe fetches + checksum-verifies the
//     agent binary from /agents/ (a bare host has no local binary otherwise).
//
// Capabilities are intentionally NOT baked (plan 2 Phase 1): the agent
// auto-detects the host's toolchains at startup.

// regTokenPathRe matches a syntactically valid registration token in the path:
// the amt_reg_ prefix + 64 lowercase hex chars (32 random bytes, see
// runner.generateToken). Restricting to hex makes shell-meta injection into the
// baked assignment impossible.
var regTokenPathRe = regexp.MustCompile(`^amt_reg_[0-9a-f]{64}$`)

// installHostRe bounds the reconstructed Host to URL-safe characters so it can
// be baked into a double-quoted shell assignment without escaping (no quotes,
// spaces, `$`, backticks, or backslashes can reach the script). Brackets are
// allowed for IPv6 literals (`[2001:db8::1]:443`) and `_` for container-DNS
// hostnames (`amadeus_backend`) — both inert inside double quotes.
var installHostRe = regexp.MustCompile(`^[A-Za-z0-9._\-:\[\]]+$`)

func (s *Server) handleInstallScript(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !regTokenPathRe.MatchString(token) {
		// Not a token-shaped path: 404 like any unknown root path, and reveal
		// nothing about token validity (2A: no oracle).
		httpx.Fail(w, http.StatusNotFound, "not_found",
			"not a runner registration token — mint one from the Runners view (Add Runner)")
		return
	}
	if s.webFS == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found",
			"installer script unavailable — this deployment has no bundled frontend")
		return
	}
	published, err := fs.ReadFile(s.webFS, "runner-install.sh")
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found",
			"installer script unavailable — this deployment has no bundled frontend")
		return
	}
	base := externalBaseURL(r)
	if base == "" {
		httpx.Fail(w, http.StatusBadRequest, "bad_host",
			"could not determine this server's URL from the request — install via the Provision helper's one-liner instead")
		return
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	// The script carries a single-use token — never let a proxy or the browser
	// cache it.
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(personalizeInstallScript(string(published), base, token)))
}

// personalizeInstallScript bakes the server URL, token, and download-on default
// into the published runner-install.sh (see the file header for the rationale).
// Exactly three substitutions and nothing else, so the served script is the
// published script modulo those values — the property the endpoint test pins.
// Pure + total so it is unit-testable without an HTTP round-trip.
func personalizeInstallScript(published, baseURL, token string) string {
	return strings.NewReplacer(
		`SERVER_URL=""`, `SERVER_URL="`+baseURL+`"`,
		`REG_TOKEN=""`, `REG_TOKEN="`+token+`"`,
		`DOWNLOAD=0`, `DOWNLOAD=1`,
	).Replace(published)
}

// externalBaseURL reconstructs this server's externally-reachable base URL
// (scheme://host) from the request, honoring the reverse proxy's forwarded
// headers. Returns "" when the scheme or host can't be trusted for baking into
// a shell script.
//
// Unlike the Remote-* identity headers (gated on the trusted-proxy allowlist in
// auth.StripUntrustedHeaders), X-Forwarded-* is read here ungated. That is safe
// because this value only shapes the caller's OWN per-request, no-store script:
// a direct client that spoofs X-Forwarded-Host merely bakes a bogus SERVER_URL
// into the bytes it itself receives — it cannot poison another operator's
// install, and it gains nothing it couldn't get by editing the script by hand.
func externalBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := firstForwarded(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	}
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := r.Host
	if h := firstForwarded(r.Header.Get("X-Forwarded-Host")); h != "" {
		host = h
	}
	if !installHostRe.MatchString(host) {
		return ""
	}
	return scheme + "://" + host
}

// firstForwarded returns the first comma-separated element of an X-Forwarded-*
// header value, trimmed (proxies may append a chain).
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
