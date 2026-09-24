package httpx

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Why this file exists (LU-9).
//
// ClientIP decides what address lands in the audit trail, and an audit trail
// that records an address the attacker chose is worse than one that records no
// address at all — it actively misleads whoever reads it during an incident.
//
// The one mistake nearly everybody makes here is taking X-Forwarded-For's
// LEFT-most entry. The header is append-only and every hop appends to whatever
// arrived, so the left-most value is simply whatever the original caller decided
// to send. The tests below are written around that: several would PASS against a
// naive left-most implementation, so each one that distinguishes the two says so
// explicitly.

func mustNets(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	var out []*net.IPNet
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("parse %q: %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

func reqFrom(peer string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

// TestClientIPWalksRightToLeftPastTrustedHops is the core contract. With a chain
// written by a real proxy the client is the RIGHT-most entry that is not one of
// ours — here 203.0.113.9, appended by the outermost proxy we trust.
//
// A left-most implementation would return 198.51.100.7 for this input, which is
// a value the caller supplied. The two answers differ, which is the point.
func TestClientIPWalksRightToLeftPastTrustedHops(t *testing.T) {
	trusted := mustNets(t, "10.0.0.0/8")
	r := reqFrom("10.0.0.1:5555", "198.51.100.7, 203.0.113.9, 10.0.0.2")

	if got := ClientIP(r, trusted); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want 203.0.113.9 (right-most untrusted). "+
			"Getting 198.51.100.7 means the walk is left-most and is reporting a caller-supplied value.", got)
	}
}

// TestClientIPIgnoresForwardedHeaderFromAnUntrustedPeer is the anchor. If some
// arbitrary host connects to us directly, its forwarding header is not evidence
// of anything — it is just a string it chose to send. Its own address is the
// client.
func TestClientIPIgnoresForwardedHeaderFromAnUntrustedPeer(t *testing.T) {
	trusted := mustNets(t, "10.0.0.0/8")
	r := reqFrom("198.51.100.50:4444", "1.2.3.4")

	if got := ClientIP(r, trusted); got != "198.51.100.50" {
		t.Errorf("ClientIP = %q, want the peer 198.51.100.50: a direct caller's X-Forwarded-For is self-asserted", got)
	}
}

// TestClientIPWithNoTrustedProxiesUsesThePeer covers the default-deny posture.
// With no proxies configured there is no hop to strip and nothing to believe, so
// the peer is the answer — never the header.
func TestClientIPWithNoTrustedProxiesUsesThePeer(t *testing.T) {
	r := reqFrom("192.0.2.10:1234", "9.9.9.9")

	if got := ClientIP(r, nil); got != "192.0.2.10" {
		t.Errorf("ClientIP = %q, want 192.0.2.10", got)
	}
}

// TestClientIPStopsAtAMalformedEntry: skipping past garbage would let an
// attacker insert an unparseable element to shift which entry we believe. The
// walk stops instead and falls back to the peer.
func TestClientIPStopsAtAMalformedEntry(t *testing.T) {
	trusted := mustNets(t, "10.0.0.0/8")
	r := reqFrom("10.0.0.1:5555", "203.0.113.9, not-an-ip, 10.0.0.2")

	if got := ClientIP(r, trusted); got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want the peer 10.0.0.1: a malformed hop must end the walk, not be skipped", got)
	}
}

// TestClientIPWithAllHopsTrustedFallsBackToThePeer: every entry is our own
// infrastructure, so there is no client address in the chain to name.
func TestClientIPWithAllHopsTrustedFallsBackToThePeer(t *testing.T) {
	trusted := mustNets(t, "10.0.0.0/8")
	r := reqFrom("10.0.0.1:5555", "10.0.0.9, 10.0.0.2")

	if got := ClientIP(r, trusted); got != "10.0.0.1" {
		t.Errorf("ClientIP = %q, want the peer 10.0.0.1", got)
	}
}

// TestClientIPReadsRepeatedHeadersInOrder: a chain can arrive split across
// several header instances rather than one comma-joined value. Reading only the
// first would silently truncate the chain and return the wrong hop.
func TestClientIPReadsRepeatedHeadersInOrder(t *testing.T) {
	trusted := mustNets(t, "10.0.0.0/8")
	r := reqFrom("10.0.0.1:5555", "198.51.100.7", "203.0.113.9, 10.0.0.2")

	if got := ClientIP(r, trusted); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q, want 203.0.113.9 across repeated headers", got)
	}
}

// TestClientIPHandlesIPv6AndPortlessPeers: httptest and some proxies hand over a
// bare address, and an IPv6 peer's host:port split is not a naive strings.Split.
func TestClientIPHandlesIPv6AndPortlessPeers(t *testing.T) {
	trusted := mustNets(t, "::1/128")

	if got := ClientIP(reqFrom("[::1]:8080", "2001:db8::5"), trusted); got != "2001:db8::5" {
		t.Errorf("IPv6 peer: ClientIP = %q, want 2001:db8::5", got)
	}
	if got := RemoteAddrIP(reqFrom("192.0.2.10")); got != "192.0.2.10" {
		t.Errorf("portless peer: RemoteAddrIP = %q, want 192.0.2.10", got)
	}
	if got := RemoteAddrIP(reqFrom("garbage")); got != "" {
		t.Errorf("unparseable peer: RemoteAddrIP = %q, want empty", got)
	}
}

// TestUserAgentIsBounded: the header is caller-controlled and unbounded, and it
// lands in a database column and a log line. A hostile client must not get to
// decide how large an audit record is.
func TestUserAgentIsBounded(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'A'
	}
	r.Header.Set("User-Agent", string(long))

	if got := UserAgent(r); len(got) != 512 {
		t.Errorf("UserAgent length = %d, want it capped at 512", len(got))
	}
	if got := UserAgent(nil); got != "" {
		t.Errorf("UserAgent(nil) = %q, want empty", got)
	}
}
