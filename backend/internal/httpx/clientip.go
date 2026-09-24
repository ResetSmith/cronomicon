package httpx

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// RemoteAddrIP returns the immediate peer's IP with any port stripped, or "" when
// r.RemoteAddr is unparseable.
//
// Behind a reverse proxy this is the PROXY's address, on every request. That is
// not a defect to work around — it is the honest answer to "who connected to
// this process" — but it is why it is recorded separately from ClientIP.
func RemoteAddrIP(r *http.Request) string {
	if r == nil || r.RemoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Some callers (and httptest) set a bare address with no port.
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
		return ip.String()
	}
	return ""
}

// ClientIP derives the originating client address from X-Forwarded-For by
// walking RIGHT to LEFT and discarding trusted-proxy hops, returning the first
// address that is not one of ours. It returns "" when no such address can be
// established.
//
// # Why right-most, and why this matters
//
// X-Forwarded-For is append-only and attacker-influenced: a client may send its
// own header, and every proxy in the chain appends to whatever arrived. So the
// LEFT-most entry — the one most code reaches for — is a value the caller chose.
// Recording it in an audit trail is strictly worse than recording nothing: it
// invites an investigator to trust an address the attacker wrote.
//
// The right-most entry, by contrast, was appended by the hop closest to us. If
// that hop is a proxy we trust, we can believe what it appended and step one
// further left; the moment we reach an address we do not recognise as our own
// infrastructure, that is the furthest left we are entitled to believe, and it is
// the answer.
//
// # Trust anchoring
//
// The walk only begins if the immediate peer (r.RemoteAddr) is itself a trusted
// proxy. If some arbitrary host connects to us directly and sends an XFF header,
// its header is meaningless and its own address is the client — so we return
// RemoteAddrIP instead. With no trusted proxies configured at all there is
// nothing to strip, and the peer is always the client.
//
// This mirrors the trust decision auth already makes for the Remote-* identity
// headers: those are honoured only from a peer in TrustedProxies, and the same
// set is passed here.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := RemoteAddrIP(r)
	if r == nil || peer == "" {
		return peer
	}
	if len(trusted) == 0 || !ipIn(peer, trusted) {
		// Direct connection, or a peer we have no reason to believe: whatever it
		// claims in a forwarding header is not evidence.
		return peer
	}
	// Collect every forwarded address, in order, across possibly-repeated headers.
	var chain []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for part := range strings.SplitSeq(v, ",") {
			if s := strings.TrimSpace(part); s != "" {
				chain = append(chain, s)
			}
		}
	}
	for _, c := range slices.Backward(chain) {
		ip := net.ParseIP(strings.TrimSpace(c))
		if ip == nil {
			// A malformed entry ends the walk. Skipping past it would let an
			// attacker hide a hop behind garbage and shift which entry we believe.
			return peer
		}
		if ipIn(ip.String(), trusted) {
			continue // another of our own hops; keep walking left
		}
		return ip.String()
	}
	// Every entry was a trusted proxy (or the header was absent): the closest
	// thing to a client we can name is the peer itself.
	return peer
}

func ipIn(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n != nil && n.Contains(parsed) {
			return true
		}
	}
	return false
}

// UserAgent returns a length-bounded User-Agent for storage.
//
// The header is entirely caller-controlled and unbounded, and it lands in a
// database column and a log line; capping it here keeps a hostile client from
// deciding how large an audit row is.
func UserAgent(r *http.Request) string {
	if r == nil {
		return ""
	}
	ua := strings.TrimSpace(r.UserAgent())
	const max = 512
	if len(ua) > max {
		return ua[:max]
	}
	return ua
}
