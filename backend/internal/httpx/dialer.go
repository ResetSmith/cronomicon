package httpx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

// SSRF egress guard (SU-7). Cronomicon makes outbound HTTP to a handful of
// OPERATOR-configured targets (Vault, GitLab, S3/MinIO, Apprise, the OIDC issuer).
// A malicious or mistaken target — or a DNS name that resolves to an internal
// address — could otherwise coerce the server into reaching cloud-metadata or
// loopback services (SSRF). The guarded dialer resolves the host and refuses any
// resolved IP in a blocked range, dialing a checked IP directly so a DNS rebind
// between the check and the connect cannot slip through.
//
// Default posture (SU-Q4(a), matching this on-prem deployment whose Vault/GitLab
// are internal RFC-1918 hosts): block cloud-metadata, loopback, and link-local;
// ALLOW RFC-1918 / unique-local. AllowPrivate=false also blocks private ranges
// (the stricter posture). AllowLoopback re-permits loopback — for a Vault-agent
// loopback sidecar, and for httptest-based tests; it is NEVER set in a normal
// production config. The cloud-metadata addresses are blocked regardless.

// EgressPolicy is the SSRF posture applied to an outbound client.
type EgressPolicy struct {
	AllowPrivate  bool // permit RFC-1918 / ULA (default true — internal Vault/GitLab)
	AllowLoopback bool // permit loopback (loopback sidecar / tests only)
}

// imdsV6 is the IPv6 cloud-metadata address. It sits inside ULA fc00::/7 (which is
// permitted when AllowPrivate is true), so it must be blocked explicitly. The IPv4
// metadata address 169.254.169.254 is already covered by the link-local block.
var imdsV6 = net.ParseIP("fd00:ec2::254")

// egressBlocked reports whether an outbound dial to ip must be refused under the
// given policy, and a short reason for the error/log.
func egressBlocked(ip net.IP, p EgressPolicy) (bool, string) {
	switch {
	case ip.IsLoopback():
		if p.AllowLoopback {
			return false, ""
		}
		return true, "loopback"
	case ip.IsUnspecified():
		return true, "unspecified"
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 (incl. the 169.254.169.254 metadata address) and fe80::/10.
		return true, "link-local/metadata"
	case ip.Equal(imdsV6):
		return true, "cloud-metadata"
	case !p.AllowPrivate && ip.IsPrivate():
		// RFC-1918 (10/8, 172.16/12, 192.168/16) + ULA fc00::/7.
		return true, "private"
	}
	return false, ""
}

// GuardedDialContext returns a DialContext that enforces the SSRF egress policy.
// It resolves the host, refuses the whole dial if ANY resolved IP is blocked, then
// dials one of the (already-checked) IPs directly.
func GuardedDialContext(p EgressPolicy) func(context.Context, string, string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no addresses for host %q", host)
		}
		for _, ip := range ips {
			if bad, why := egressBlocked(ip.IP, p); bad {
				return nil, fmt.Errorf("outbound dial to %s (%s) blocked: %s address not permitted", host, ip.IP, why)
			}
		}
		var firstErr error
		for _, ip := range ips {
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			firstErr = derr
		}
		return nil, firstErr
	}
}

// SafeTransport clones base (or http.DefaultTransport when base is nil) and
// installs the guarded dialer, so an existing transport's TLS/proxy/pool settings
// are preserved while its egress is guarded. Only DialContext is replaced — the
// TLS handshake (and thus certificate/hostname verification against the request
// URL's host) is unchanged, so dialing the vetted IP does not weaken TLS.
//
// Caveat: the clone keeps ProxyFromEnvironment. When HTTP(S)_PROXY is set the
// transport hands the PROXY address to the dialer, so the guard vets the proxy and
// the real target is resolved at the proxy — the IP checks no longer apply to the
// target. This is moot for the primary targets (loopback / link-local IMDS are not
// reachable through a forward proxy), but note it if a proxy is in play.
func SafeTransport(base *http.Transport, p EgressPolicy) *http.Transport {
	var tr *http.Transport
	if base != nil {
		tr = base.Clone()
	} else {
		tr = http.DefaultTransport.(*http.Transport).Clone()
	}
	tr.DialContext = GuardedDialContext(p)
	return tr
}

// SafeClient builds an *http.Client with the given timeout and a guarded
// transport. It does NOT set CheckRedirect — callers that carry credentials add
// http.ErrUseLastResponse themselves (SU-8).
func SafeClient(timeout time.Duration, p EgressPolicy) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: SafeTransport(nil, p),
	}
}
