package sshexec

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func insertBastion(t *testing.T, svc *Service, id, name, authKeyEnvVar string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := svc.db.Exec(`
		INSERT INTO bastions(id, name, hostname, address, port, username, auth_key_env_var, created_at)
		VALUES (?, ?, ?, '127.0.0.1', 2222, 'jump', ?, ?)`,
		id, name, name, authKeyEnvVar, now); err != nil {
		t.Fatalf("insert bastion: %v", err)
	}
}

// TestBastionAddr_ReturnsAuthKeyEnvVar (PP-H4 H4-1): bastionAddr surfaces the
// bastion's own auth_key_env_var (and empty string when NULL).
func TestBastionAddr_ReturnsAuthKeyEnvVar(t *testing.T) {
	svc, _ := newReaperService(t)

	insertBastion(t, svc, "b1", "jump-a", "JUMP_KEY")
	b, err := svc.bastionAddr(context.Background(), "jump-a", nil)
	if err != nil {
		t.Fatalf("bastionAddr: %v", err)
	}
	if b.AuthKeyEnvVar != "JUMP_KEY" {
		t.Errorf("authKeyEnvVar = %q, want JUMP_KEY", b.AuthKeyEnvVar)
	}

	insertBastion(t, svc, "b2", "jump-b", "") // NULL/empty
	b2, err := svc.bastionAddr(context.Background(), "jump-b", nil)
	if err != nil {
		t.Fatalf("bastionAddr: %v", err)
	}
	if b2.AuthKeyEnvVar != "" {
		t.Errorf("authKeyEnvVar = %q, want empty for unset key", b2.AuthKeyEnvVar)
	}
}

// TestDial_BastionKeyLoadError (PP-H4 H4-2): when the bastion has its own
// auth_key_env_var but no key material is resolvable, dial fails with a wrapped
// "dial bastion" error (→ cred_error via classifyDialErr) BEFORE any network —
// proving dial now loads the BASTION's key, not the target's.
func TestDial_BastionKeyLoadError(t *testing.T) {
	svc, _ := newReaperService(t)
	insertBastion(t, svc, "b1", "jump", "MISSING_BASTION_KEY")

	// A valid target signer that must NOT be used for the bastion hop.
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)

	tgt := target{Name: "web01", Address: "127.0.0.1", Port: 2222, User: "deploy", Via: "jump"}
	_, _, err := svc.dial(context.Background(), tgt, signer)
	if err == nil {
		t.Fatal("dial succeeded; want a bastion key-load error")
	}
	if !strings.Contains(err.Error(), "dial bastion") {
		t.Errorf("error = %q, want it to wrap 'dial bastion ...' (bastion key load failed)", err.Error())
	}
	// And it must classify as a credential error (not a generic conn_error), so
	// the operator gets the actionable key hint (PP-H4 review).
	if status, _ := classifyDialErr(err); status != StatusCredError {
		t.Errorf("classifyDialErr = %q, want %q (bastion key-load is a credential failure)", status, StatusCredError)
	}
}

// TestBastionHostKeyCallback (SU-4; 2.3.0): the bastion hop is verified against
// the local runner's approved keys, under the address the bastion's record is
// dialled at. The approved key passes; a different key is a mismatch; with no
// approved key the hop is refused, host_key_unverified — nothing is captured
// on first connect — and a key approved under the bastion's NAME does not count.
func TestBastionHostKeyCallback(t *testing.T) {
	svc, pool := newReaperService(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	pub := signer.PublicKey()
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	signer2, _ := ssh.NewSignerFromKey(priv2)
	bastion := target{ID: "b1", Name: "jumphost", Address: "10.9.9.9", Port: 2222}

	// Nothing approved: refused, and nothing written.
	err := svc.bastionHostKeyCallback(context.Background(), bastion, "jump")("", nil, pub)
	if err == nil || !strings.Contains(err.Error(), HostKeyUnverified) {
		t.Fatalf("a bastion with no approved key = %v, want %s", err, HostKeyUnverified)
	}
	var rows int
	_ = pool.QueryRow(`SELECT COUNT(*) FROM host_key_ledger`).Scan(&rows)
	if rows != 0 {
		t.Fatalf("the callback wrote %d ledger row(s): nothing is captured on first connect", rows)
	}

	// Approved under its dial address.
	trustHostKey(t, pool, "10.9.9.9", 2222, pub)
	cb := svc.bastionHostKeyCallback(context.Background(), bastion, "jump")
	if err := cb("", nil, pub); err != nil {
		t.Errorf("the approved bastion key was refused: %v", err)
	}
	if err := cb("", nil, signer2.PublicKey()); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("a different bastion key must be refused as a mismatch, got %v", err)
	}

	// A key approved under a NAME is not a key for this bastion: a name is not a
	// machine (two agencies may each have a "jump"), and the hop is verified
	// under the address its record is dialled at.
	other := target{ID: "b2", Name: "otherhost", Address: "10.9.9.10", Port: 22}
	trustHostKey(t, pool, "edge-jump", 22, signer2.PublicKey())
	if err := svc.bastionHostKeyCallback(context.Background(), other, "edge-jump")("", nil, signer2.PublicKey()); err == nil || !strings.Contains(err.Error(), HostKeyUnverified) {
		t.Errorf("a key approved only under the bastion's name = %v, want %s", err, HostKeyUnverified)
	}
	// The algorithms asked of a hop are those of its approved keys.
	if got := svc.hostKeyAlgorithms(context.Background(), "[10.9.9.9]:2222"); len(got) != 1 || got[0] != pub.Type() {
		t.Errorf("algorithms for a hop with one approved %s key = %v", pub.Type(), got)
	}
	if got := svc.hostKeyAlgorithms(context.Background(), "10.9.9.10"); got != nil {
		t.Errorf("algorithms for a hop with nothing approved = %v, want the default (nil)", got)
	}
}
