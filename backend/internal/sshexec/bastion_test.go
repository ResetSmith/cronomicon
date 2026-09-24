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
	b, err := svc.bastionAddr(context.Background(), "jump-a")
	if err != nil {
		t.Fatalf("bastionAddr: %v", err)
	}
	if b.AuthKeyEnvVar != "JUMP_KEY" {
		t.Errorf("authKeyEnvVar = %q, want JUMP_KEY", b.AuthKeyEnvVar)
	}

	insertBastion(t, svc, "b2", "jump-b", "") // NULL/empty
	b2, err := svc.bastionAddr(context.Background(), "jump-b")
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

// TestBastionHostKeyCallback (SU-4): the bastion callback strict-compares a pinned
// key (mismatch → MITM error), refuses an unparseable stored key, and TOFU-captures
// the first-seen key into the bastion row when unpinned.
func TestBastionHostKeyCallback(t *testing.T) {
	svc, _ := newReaperService(t)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	pub := signer.PublicKey()
	authLine := string(ssh.MarshalAuthorizedKey(pub))
	_, priv2, _ := ed25519.GenerateKey(rand.Reader)
	signer2, _ := ssh.NewSignerFromKey(priv2)

	// Strict: pinned key matches → nil; a different key → MITM error.
	strict := svc.bastionHostKeyCallback("b1", "jump", authLine)
	if err := strict("", nil, pub); err != nil {
		t.Errorf("matching pinned bastion key rejected: %v", err)
	}
	if err := strict("", nil, signer2.PublicKey()); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("mismatched bastion key must be rejected as MITM, got %v", err)
	}

	// Unparseable stored key → refuse (fail-closed, no accept-any fallback).
	bad := svc.bastionHostKeyCallback("b1", "jump", "not-a-valid-key")
	if err := bad("", nil, pub); err == nil {
		t.Error("unparseable stored bastion key must refuse")
	}

	// TOFU: empty stored key → capture the first-seen key into the bastion row.
	insertBastion(t, svc, "b-tofu", "jump-tofu", "")
	tofu := svc.bastionHostKeyCallback("b-tofu", "jump-tofu", "")
	if err := tofu("", nil, pub); err != nil {
		t.Errorf("TOFU capture errored: %v", err)
	}
	var stored string
	_ = svc.db.QueryRow(`SELECT COALESCE(host_key,'') FROM bastions WHERE id='b-tofu'`).Scan(&stored)
	if strings.TrimSpace(stored) != strings.TrimSpace(authLine) {
		t.Errorf("TOFU did not persist the bastion key: got %q want %q", stored, authLine)
	}
	// After capture, a subsequent changed key strict-fails.
	if err := svc.bastionHostKeyCallback("b-tofu", "jump-tofu", stored)("", nil, signer2.PublicKey()); err == nil {
		t.Error("after TOFU, a changed bastion key must be rejected")
	}
}
