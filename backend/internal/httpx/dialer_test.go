package httpx

import (
	"net"
	"testing"
)

// TestEgressBlocked (SU-7) locks the SSRF policy matrix: cloud-metadata and
// link-local are blocked regardless of posture; loopback is blocked unless
// AllowLoopback; RFC-1918/ULA are allowed by default and blocked only when
// AllowPrivate is false; public addresses are always allowed.
func TestEgressBlocked(t *testing.T) {
	def := EgressPolicy{AllowPrivate: true}                     // production default
	strict := EgressPolicy{}                                    // AllowPrivate=false
	lb := EgressPolicy{AllowPrivate: true, AllowLoopback: true} // loopback sidecar/tests
	cases := []struct {
		ip          string
		pol         EgressPolicy
		wantBlocked bool
	}{
		{"169.254.169.254", def, true}, // IPv4 cloud metadata (link-local)
		{"169.254.169.254", lb, true},  // still blocked even with AllowLoopback
		{"fd00:ec2::254", def, true},   // IPv6 cloud metadata (inside ULA)
		{"127.0.0.1", def, true},       // loopback blocked by default
		{"127.0.0.1", lb, false},       // loopback permitted with AllowLoopback
		{"::1", def, true},             // loopback v6
		{"::1", lb, false},             //
		{"169.254.1.1", def, true},     // link-local
		{"fe80::1", def, true},         // link-local v6
		{"0.0.0.0", def, true},         // unspecified
		{"10.0.0.5", def, false},       // private allowed by default
		{"10.0.0.5", strict, true},     // private blocked when AllowPrivate=false
		{"192.168.1.10", def, false},   //
		{"192.168.1.10", strict, true}, //
		{"172.16.5.5", strict, true},   //
		{"fd12:3456::1", def, false},   // ULA allowed by default
		{"fd12:3456::1", strict, true}, //
		{"8.8.8.8", def, false},        // public always allowed
		{"8.8.8.8", strict, false},     //
		{"1.1.1.1", strict, false},     //
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		got, why := egressBlocked(ip, c.pol)
		if got != c.wantBlocked {
			t.Errorf("egressBlocked(%s, %+v) = %v (%s), want %v", c.ip, c.pol, got, why, c.wantBlocked)
		}
	}
}

// TestGuardedDialContextRefusesMetadata proves the dialer refuses a literal
// metadata/loopback address before connecting.
func TestGuardedDialContextRefusesMetadata(t *testing.T) {
	dial := GuardedDialContext(EgressPolicy{AllowPrivate: true}) // still blocks metadata/loopback
	for _, addr := range []string{"169.254.169.254:80", "127.0.0.1:9999", "[::1]:80"} {
		if _, err := dial(nil, "tcp", addr); err == nil {
			t.Errorf("dial to %s should be refused, got nil error", addr)
		}
	}
}
