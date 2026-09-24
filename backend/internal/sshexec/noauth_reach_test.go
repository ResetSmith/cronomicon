package sshexec

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestNoAuthErrorNeverReachesCredMsg is the reachability proof DM-5 is built on,
// and it is written as a test rather than settled by reading because LB17 was
// itself a reading error twice over.
//
// The history: credMsg matched the substring "no authKeyEnvVar", which no error
// in the codebase ever produced, so a keyless host was reported as an
// unparseable key. DD-3 (v1.5.36) "fixed" that by matching what conn.go
// actually emits — but the fix was based on the claim that the branch is
// reachable through classifyDialErr when a bastion's key loads inside dial.
// That claim is false: dial() only calls loadSigner for the bastion when the
// bastion HAS a credential or key name (conn.go:201), so it can never pass the
// empty/empty pair that produces this error.
//
// Every path into loadSigner from the probe guards the empty case first, which
// is what this test pins — one subtest per guard. If a future change removes a
// guard, the corresponding subtest fails and whoever removed it learns that
// credMsg's branch became live again, which is the moment to reinstate it.
//
// The error itself is NOT dead: sshexec.go's runTarget calls loadSigner with no
// guard, so a keyless target in a real run produces it and it is written to the
// run log. It simply never travels through the probe's message mapping.
func TestNoAuthErrorNeverReachesCredMsg(t *testing.T) {
	t.Run("loadSigner does produce it, for the executor", func(t *testing.T) {
		svc, _ := probeFixture(t)
		_, err := loadSigner(context.Background(), svc.db, svc.cfg, svc.sec, "", "")
		if err == nil {
			t.Fatal("loadSigner(\"\", \"\") returned no error — runTarget relies on this being an error")
		}
		if !strings.Contains(err.Error(), "no auth credential") {
			t.Fatalf("loadSigner error = %q, want the no-auth error", err)
		}
	})

	t.Run("ProbeHost routes a keyless target to the reachability tier", func(t *testing.T) {
		svc, pool := probeFixture(t)
		client, _ := newKey(t)
		addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
		host, port, _ := net.SplitHostPort(addr)
		// No key name on the row: the guard at the top of ProbeHost must send
		// this to probeReachableHost BEFORE loadSigner is ever called.
		insertHost(t, pool, "keyless", host, atoiPort(port), "tester", "",
			string(ssh.MarshalAuthorizedKey(hostKey)), "")

		res, err := svc.ProbeHost(context.Background(), "keyless")
		if err != nil {
			t.Fatalf("ProbeHost err: %v", err)
		}
		if res.Status != StatusReachable {
			t.Fatalf("status = %q (%s), want %q — a keyless target must take the reachability tier, not reach loadSigner",
				res.Status, res.Message, StatusReachable)
		}
	})

	t.Run("ProbeBastion routes a keyless bastion to the reachability tier", func(t *testing.T) {
		svc, pool := probeFixture(t)
		client, _ := newKey(t)
		addr, _ := testSSHServer(t, client.PublicKey(), "x")
		host, port, _ := net.SplitHostPort(addr)
		if _, err := pool.Exec(`
			INSERT INTO bastions(id, hostname, name, address, port, username, created_at)
			VALUES('bk','bk','bk',?,?,'tester','2026-09-18T00:00:00Z')`, host, atoiPort(port)); err != nil {
			t.Fatal(err)
		}

		res, err := svc.ProbeBastion(context.Background(), "bk")
		if err != nil {
			t.Fatalf("ProbeBastion err: %v", err)
		}
		if res.Status == StatusCredError && strings.Contains(res.Message, "not a parseable private key") {
			t.Fatalf("keyless bastion fell through to loadSigner: %s", res.Message)
		}
		if res.Status != StatusReachable {
			t.Fatalf("status = %q (%s), want %q", res.Status, res.Message, StatusReachable)
		}
	})

	t.Run("a keyless target via a keyless bastion names the bastion, by sentinel", func(t *testing.T) {
		svc, pool := probeFixture(t)
		client, _ := newKey(t)
		addr, hostKey := testSSHServer(t, client.PublicKey(), "x")
		host, port, _ := net.SplitHostPort(addr)
		if _, err := pool.Exec(`
			INSERT INTO bastions(id, hostname, name, address, port, username, created_at)
			VALUES('b0','b0','b0',?,?,'tester','2026-09-18T00:00:00Z')`, host, atoiPort(port)); err != nil {
			t.Fatal(err)
		}
		insertHost(t, pool, "viaKeyless", host, atoiPort(port), "tester", "",
			string(ssh.MarshalAuthorizedKey(hostKey)), "b0")

		res, err := svc.ProbeHost(context.Background(), "viaKeyless")
		if err != nil {
			t.Fatalf("ProbeHost err: %v", err)
		}
		// reachViaBastion guards with errBastionNoKey — a SENTINEL, matched with
		// errors.Is. This is the idiom DM-5 adopts for the sibling case, and it
		// is the reason this path has never suffered LB17's failure mode.
		if res.Status != StatusCredError || !strings.Contains(res.Message, "has no auth key") {
			t.Fatalf("status = %q (%s), want a cred_error naming the bastion's missing key",
				res.Status, res.Message)
		}
	})
}
