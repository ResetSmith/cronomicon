// Package keyscan captures the SSH host key a target presents, without
// verifying it and without authenticating. It is the one scan: an agent runs it
// from its own network, and the server runs it in-process for the local runner
// (2.3.0, Phase C) — the same dial, the same known_hosts host form, the same
// line, so a key scanned by either reads the same on the review screen.
//
// A scanned key is a CANDIDATE. Nothing here trusts it; an operator approves a
// fingerprint the server computed from it.
package keyscan

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Key is one host's captured key. The JSON tags are the agent's upload
// contract (POST /runners/{id}/host-keys).
type Key struct {
	Host           string `json:"host"`
	KeyType        string `json:"keyType"`
	Fingerprint    string `json:"fingerprint"`
	KnownHostsLine string `json:"knownHostsLine"`
}

// Parallel bounds how many hosts are dialled at once. A scope scan can name
// hundreds; an unreachable one costs a full dial timeout.
const Parallel = 8

// Scan dials a target and captures its presented host key without verifying.
// target may be "host" (default port 22) or "host:port". The known_hosts line
// is keyed on the normalized dial address so it matches what the run-time
// verifier checks.
func Scan(ctx context.Context, target string, timeout time.Duration) (Key, error) {
	addr := target
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22") // bare host → default SSH port
	}

	var captured ssh.PublicKey
	captureDone := errors.New("host key captured") // sentinel to abort after the key arrives
	cfg := &ssh.ClientConfig{
		User: "cronomicon-keyscan", // never authenticates; we only want the host key
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return captureDone
		},
		Timeout: timeout,
	}

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Key{}, err
	}
	defer func() { _ = conn.Close() }()
	// A peer that accepts the connection and then says nothing must not hold a
	// scan slot past the dial timeout.
	_ = conn.SetDeadline(time.Now().Add(timeout))
	// The handshake returns our sentinel error once the host key is captured;
	// that's expected — we never proceed to auth.
	_, _, _, hErr := ssh.NewClientConn(conn, addr, cfg)
	if captured == nil {
		if hErr != nil {
			return Key{}, fmt.Errorf("no host key presented by %s: %w", addr, hErr)
		}
		return Key{}, fmt.Errorf("no host key presented by %s", addr)
	}

	return Key{
		Host:           target,
		KeyType:        captured.Type(),
		Fingerprint:    ssh.FingerprintSHA256(captured),
		KnownHostsLine: strings.TrimSpace(knownhosts.Line([]string{knownhosts.Normalize(addr)}, captured)),
	}, nil
}
