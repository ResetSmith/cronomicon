package sshexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
)

// Host-key verification for the local runner (2.3.0, Phase C).
//
// The server connects only to hosts whose key an operator has approved for the
// local runner: the keys in force in its ledger (host_key_ledger, approved and
// not superseded). That is the rule an agent has always lived by, with a
// known_hosts file where the server has the ledger itself — there is no file
// here and nothing to deliver.
//
// Until 2.3.0 the server kept a key on each host record and CAPTURED it on the
// first connection (trust on first use). That is gone. A host the local runner
// has no approved key for is not connected to: the run fails for that host with
// host_key_unverified, and the way forward is to scan the host and approve its
// key (Runners → Local runner → Host keys). The keys the server had captured
// were carried into the ledger by the upgrade, so a host it was already
// connecting to goes on working.

// HostKeyUnverified is the code in the error for a host (or bastion) the local
// runner has no approved key for. It is matched by text where the error has
// passed through the SSH handshake's own wrapping.
const HostKeyUnverified = "host_key_unverified"

// errHostKeyUnverified carries the code.
var errHostKeyUnverified = errors.New(HostKeyUnverified)

// trustedHostKeys returns the keys in force for the local runner under any of
// the given known_hosts host patterns.
func (s *Service) trustedHostKeys(ctx context.Context, patterns ...string) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for _, p := range patterns {
		if p == "" {
			continue
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT l.known_hosts_line FROM host_key_ledger l
			  JOIN runners rn ON rn.id = l.runner_id AND rn.kind = 'server'
			 WHERE l.host = ? AND l.decision = 'approved' AND l.superseded_at IS NULL`, p)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				return nil, err
			}
			// A stored line that does not parse is not a key: it is skipped, and
			// if it was the host's only one the host is unverified. Never "trust
			// what cannot be read".
			if l, perr := knownhostsline.Parse(line); perr == nil && l.Marker == "" {
				keys = append(keys, l.Key)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// verifyHostKey returns the callback for one hop. what names it for the error
// ("host db-01", "bastion jump-a"); patterns are the known_hosts hosts the key
// may be approved under.
func (s *Service) verifyHostKey(ctx context.Context, what string, patterns ...string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		// Under the dial's own context: a run that is killed mid-handshake
		// stops here too, and a cancelled read is an unreadable trust store,
		// which refuses.
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		keys, err := s.trustedHostKeys(ctx, patterns...)
		if err != nil {
			// Unreadable trust is not trust.
			return fmt.Errorf("could not read the approved host keys for %s: %w", what, err)
		}
		if len(keys) == 0 {
			return fmt.Errorf("%w: the local runner has no approved host key for %s (%s) — scan it and approve its key "+
				"under Runners → Local runner → Host keys", errHostKeyUnverified, what, patterns[0])
		}
		for _, k := range keys {
			if bytes.Equal(k.Marshal(), presented.Marshal()) {
				return nil
			}
		}
		return fmt.Errorf("host key mismatch for %s (possible MITM): it presented %s %s, which is not a key approved for the local runner",
			what, presented.Type(), ssh.FingerprintSHA256(presented))
	}
}

// hostKeyCallback verifies a target's key against the local runner's approved
// keys for the address it is dialled at. There is no insecure-ignore path
// (EX-D3) and, since 2.3.0, no capture-on-first-connect either.
func (s *Service) hostKeyCallback(ctx context.Context, t target) ssh.HostKeyCallback {
	return s.verifyHostKey(ctx, "host "+t.Name, hostkeys.Pattern(hostkeys.Target(t)))
}

// bastionHostKeyCallback verifies the BASTION hop (SU-4), under the address the
// bastion's record is dialled at — the same rule as a target. via is the name
// the host record gives its bastion, for the error only: a name is not a
// machine (two agencies may each have a "jump"), so a key is never accepted
// for this bastion because it was approved under a name.
func (s *Service) bastionHostKeyCallback(ctx context.Context, b target, via string) ssh.HostKeyCallback {
	name := via
	if name == "" {
		name = b.Name
	}
	return s.verifyHostKey(ctx, "bastion "+name, hostkeys.Pattern(hostkeys.Target(b)))
}

// hostKeyAlgorithms is the list of host-key algorithms to ask a hop for: those
// of the keys approved for it, and no others. Without it the client takes
// whichever key type the two sides prefer, so a host that has an ed25519 and
// an ecdsa key and one of them approved would be refused as a mismatch half
// the time — and a host whose trusted key is of one type could be made to
// present a key of another. It is what OpenSSH does with a known host. nil
// (the library's default) when nothing is approved: the callback then refuses.
func (s *Service) hostKeyAlgorithms(ctx context.Context, patterns ...string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys, err := s.trustedHostKeys(ctx, patterns...)
	if err != nil || len(keys) == 0 {
		return nil
	}
	var algos []string
	seen := map[string]bool{}
	add := func(a ...string) {
		for _, x := range a {
			if !seen[x] {
				seen[x] = true
				algos = append(algos, x)
			}
		}
	}
	for _, k := range keys {
		if k.Type() == ssh.KeyAlgoRSA {
			// One RSA key, three signature algorithms; the SHA-2 ones first.
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA)
			continue
		}
		add(k.Type())
	}
	return algos
}

// hostTrust is the pair every client config for a TARGET takes.
func (s *Service) hostTrust(ctx context.Context, t target) (ssh.HostKeyCallback, []string) {
	return s.hostKeyCallback(ctx, t), s.hostKeyAlgorithms(ctx, hostkeys.Pattern(hostkeys.Target(t)))
}

// bastionTrust is hostTrust for the bastion hop.
func (s *Service) bastionTrust(ctx context.Context, b target, via string) (ssh.HostKeyCallback, []string) {
	return s.bastionHostKeyCallback(ctx, b, via), s.hostKeyAlgorithms(ctx, hostkeys.Pattern(hostkeys.Target(b)))
}
