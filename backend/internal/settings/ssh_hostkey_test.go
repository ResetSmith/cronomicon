package settings

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The host key shown on a host record or a bastion is the one in force for the
// LOCAL RUNNER at the address the record is dialled at (2.3.0): the record no
// longer holds a key of its own. The known_hosts host is built here in SQL and
// must be the one the engine verifies against — the bare address on port 22,
// [address]:port otherwise, the record's name when it has no address.
func TestHostRecordsShowTheLocalRunnersApprovedKey(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := database.Exec(q, args...); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	localID, _, err := EnsureLocalRunner(ctx, database, false, 4)
	if err != nil {
		t.Fatal(err)
	}
	newKey := func() ssh.PublicKey {
		pub, _, _ := ed25519.GenerateKey(rand.Reader)
		k, err := ssh.NewPublicKey(pub)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	trust := func(runnerID, pattern string, k ssh.PublicKey, superseded bool) {
		var sup any
		if superseded {
			sup = "2026-01-02T00:00:00Z"
		}
		line := pattern + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
		exec(`INSERT INTO host_key_ledger (batch_id, runner_id, runner_name, host, key_type, fingerprint, known_hosts_line,
		                                   decision, source, actor, decided_at, delivered_at, confirmed_at, superseded_at)
		      VALUES ('t', ?, 'r', ?, ?, ?, ?, 'approved', 'pasted', 't', 't', 't', 't', ?)`,
			runnerID, pattern, k.Type(), ssh.FingerprintSHA256(k), line, sup)
	}
	std, alt, byName, old, agents, jump := newKey(), newKey(), newKey(), newKey(), newKey(), newKey()
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-std', 'web1', '10.0.0.5', 22, 't', 't', 't', 't')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-alt', 'db1', '10.0.0.6', 2222, 't', 't', 't', 't')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-name', 'named.example', 22, 't', 't', 't', 't')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-none', 'new1', '10.0.0.7', 22, 't', 't', 't', 't')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-old', 'rekeyed', '10.0.0.8', 22, 't', 't', 't', 't')`)
	exec(`INSERT INTO ssh_hosts (id, hostname, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('h-agent', 'agents-only', '10.0.0.9', 22, 't', 't', 't', 't')`)
	exec(`INSERT INTO bastions (id, hostname, name, address, port, created_by, created_at, last_modified_by, last_modified_at) VALUES ('b1', 'jump.example', 'jump', '10.0.1.1', 2200, 't', 't', 't', 't')`)
	trust(localID, "10.0.0.5", std, false)
	trust(localID, "[10.0.0.6]:2222", alt, false)
	trust(localID, "named.example", byName, false)
	trust(localID, "10.0.0.8", old, true) // replaced or removed: not in force
	// A key an AGENT trusts says nothing about what the server may connect to.
	exec(`INSERT INTO runners (id, name, status, registered_at, created_at) VALUES ('agent', 'agent', 'online', 't', 't')`)
	trust("agent", "10.0.0.9", agents, false)
	trust(localID, "[10.0.1.1]:2200", jump, false)

	hosts, err := ListSshHosts(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SshHost{}
	for _, h := range hosts {
		got[h.ID] = h
	}
	for id, want := range map[string]ssh.PublicKey{"h-std": std, "h-alt": alt, "h-name": byName} {
		h := got[id]
		if !h.HostKeyPinned || h.HostKeyFingerprint != ssh.FingerprintSHA256(want) || h.HostKeyType != want.Type() {
			t.Errorf("%s shows pinned=%v %s %s, want the local runner's approved key %s", id, h.HostKeyPinned, h.HostKeyType, h.HostKeyFingerprint, ssh.FingerprintSHA256(want))
		}
	}
	for _, id := range []string{"h-none", "h-old", "h-agent"} {
		if h := got[id]; h.HostKeyPinned || h.HostKeyFingerprint != "" {
			t.Errorf("%s shows a host key (%s) the local runner does not have in force", id, h.HostKeyFingerprint)
		}
	}
	one, err := GetSshHost(ctx, database, "h-alt")
	if err != nil || one == nil || one.HostKeyFingerprint != ssh.FingerprintSHA256(alt) {
		t.Errorf("GetSshHost(h-alt) = %+v, %v; want the approved key", one, err)
	}
	b, err := GetBastion(ctx, database, "b1")
	if err != nil || b == nil || !b.HostKeyPinned || b.HostKeyFingerprint != ssh.FingerprintSHA256(jump) {
		t.Errorf("the bastion shows %+v (%v), want the local runner's approved key for its address", b, err)
	}
}
