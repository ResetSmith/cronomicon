package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// Host-key scan & approve — agent side (runner-install-update-2.md Phase 5).
//
// On a "keyscan" control op the agent dials each requested host from its OWN
// vantage, captures the presented host key WITHOUT verifying (like
// ssh-keyscan, but native — x/crypto/ssh, no external binary), and uploads what
// it saw for operator approval. On a "trust-hosts" op it appends the approved
// known_hosts lines to its trust store. Human-approved TOFU: strictly better
// than an operator pasting ssh-keyscan output blind, and auditable.

// scannedHostKey is one host's captured key, uploaded for approval. JSON tags
// match the server's upload contract (keyscan.go: scannedHostKey).
type scannedHostKey struct {
	Host           string `json:"host"`
	KeyType        string `json:"keyType"`
	Fingerprint    string `json:"fingerprint"`
	KnownHostsLine string `json:"knownHostsLine"`
}

// handleKeyscan scans each requested host and uploads the captured keys. Runs in
// its own goroutine (dispatched from handleControl) so it never blocks the poll
// loop. A host that can't be reached is logged and skipped — the others still
// upload.
func (a *Agent) handleKeyscan(ctx context.Context, hosts []string) {
	var scanned []scannedHostKey
	for _, h := range hosts {
		k, err := scanHostKey(ctx, h)
		if err != nil {
			a.log.Warn("host-key scan failed", "host", h, "error", err)
			continue
		}
		scanned = append(scanned, k)
	}
	if len(scanned) == 0 {
		a.log.Warn("host-key scan produced no keys", "requested", hosts)
		return
	}
	if err := a.client.UploadHostKeys(ctx, a.id, scanned); err != nil {
		a.log.Error("upload scanned host keys failed", "error", err)
		return
	}
	a.log.Info("uploaded scanned host keys for approval", "count", len(scanned))
}

// scanHostKey dials a target and captures its presented host key without
// verifying. target may be "host" (default port 22) or "host:port". The
// known_hosts line is keyed on the normalized dial address so it matches what
// the run-time verifier (knownhosts.New) checks.
func scanHostKey(ctx context.Context, target string) (scannedHostKey, error) {
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
		Timeout: sshDialTimeout,
	}

	d := net.Dialer{Timeout: sshDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return scannedHostKey{}, err
	}
	defer conn.Close()
	// The handshake returns our sentinel error once the host key is captured;
	// that's expected — we never proceed to auth.
	_, _, _, hErr := ssh.NewClientConn(conn, addr, cfg)
	if captured == nil {
		return scannedHostKey{}, fmt.Errorf("no host key presented by %s: %v", addr, hErr)
	}

	return scannedHostKey{
		Host:           target,
		KeyType:        captured.Type(),
		Fingerprint:    ssh.FingerprintSHA256(captured),
		KnownHostsLine: strings.TrimSpace(knownhosts.Line([]string{knownhosts.Normalize(addr)}, captured)),
	}, nil
}

// handleTrustHosts appends approved known_hosts lines to the agent's trust
// store. Idempotent — a line already present is skipped, so a re-delivered
// trust-hosts op never duplicates entries. Takes effect on the next run (the
// callback re-reads the file per dial).
func (a *Agent) handleTrustHosts(entries []string) {
	path := a.cfg.KnownHostsFile
	if path == "" {
		a.log.Error("trust-hosts: no known_hosts path configured")
		return
	}
	if err := appendKnownHosts(path, entries); err != nil {
		a.log.Error("trust-hosts: append known_hosts failed", "path", path, "error", err)
		return
	}
	a.log.Info("appended approved host keys to known_hosts", "count", len(entries), "path", path)
}

// appendKnownHosts appends each entry to the known_hosts file, skipping any line
// already present (dedupe). Creates the file (0640) if absent.
func appendKnownHosts(path string, entries []string) error {
	if err := ensureKnownHostsFile(path); err != nil {
		return err
	}
	existing, _ := os.ReadFile(path)
	have := map[string]bool{}
	for l := range strings.SplitSeq(string(existing), "\n") {
		if t := strings.TrimSpace(l); t != "" {
			have[t] = true
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range entries {
		e = strings.TrimSpace(e)
		// Defense-in-depth (the server already rejects control chars at upload):
		// never write a multi-line entry — a single approved line must not smuggle
		// extra trusted hosts into known_hosts.
		if e == "" || have[e] || strings.ContainsAny(e, "\n\r") {
			continue
		}
		if _, err := f.WriteString(e + "\n"); err != nil {
			return err
		}
		have[e] = true
	}
	return nil
}
