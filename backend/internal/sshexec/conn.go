package sshexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"golang.org/x/crypto/ssh"
)

const dialTimeout = 15 * time.Second

// errNoAuth is returned by loadSigner when a row carries neither a credential id
// nor an auth-key name. A SENTINEL rather than a bare fmt.Errorf so any consumer
// matches it with errors.Is — the substring coupling this replaces is precisely
// what LB17 was: a matcher looked for "no authKeyEnvVar", the error said
// something else, and a keyless host was reported to operators for months as an
// unparseable private key with nothing failing anywhere.
//
// Only the EXECUTOR can currently produce it: sshexec.go's runTarget calls
// loadSigner without a guard, and the message lands in the run log. Every probe
// path guards the empty case before calling loadSigner (see
// TestNoAuthErrorNeverReachesCredMsg), which is why probe.go no longer has a
// branch for it — if a future caller drops its guard, that test fails first.
var errNoAuth = errors.New("host has no auth credential or authKeyEnvVar configured")

// loadSigner resolves a target's private key and parses it. Two permanent,
// coequal mechanisms (SK.17): a first-class ssh_credentials reference (credID)
// wins — it is resolved and any failure is fatal, a configured credential must
// never silently fall through to the name path (PP-M7). Only when no credential is
// referenced does it resolve auth_key_env_var by NAME (stored-secrets table first
// — a private key is a secret — then a plaintext env_vars value). The name path is
// NOT deprecated: it is how inventory/git-imported hosts (which name keys in their
// Ansible vars) authenticate in-app, and it mirrors how the out-of-process runner
// resolves keys by name locally. Never logs key material.
func loadSigner(ctx context.Context, db *sql.DB, cfg *config.Config, sec *secrets.Service, credID, key string) (ssh.Signer, error) {
	// A vault-source ssh_credential resolves through this executor's configured
	// Vault client (P2.4); nil when Vault is unconfigured (stored keys still work).
	var vault secrets.VaultClient
	if sec != nil {
		vault = sec.Vault()
	}
	if credID != "" {
		return sshkeys.ResolveSigner(ctx, db, cfg, vault, credID)
	}
	if key == "" {
		return nil, errNoAuth
	}

	// Derived-reference routing (W3): a prefixed name routes DETERMINISTICALLY to
	// one section (strip the prefix verbatim → bare row name), with no fallback —
	// a configured reference that does not resolve is a hard error, never a silent
	// fall-through (PP-M7). A bare name keeps today's secrets→env_vars fallback
	// chain for the transition.
	if section, bare, ok := envref.Split(key); ok {
		switch section {
		case envref.SectionKey: // SSH Keys section → ssh_credentials by label.
			var id string
			if err := db.QueryRowContext(ctx,
				`SELECT id FROM ssh_credentials WHERE label = ? LIMIT 1`, bare).Scan(&id); err != nil {
				return nil, fmt.Errorf("no ssh credential labelled %q (from reference %s)", bare, key)
			}
			return sshkeys.ResolveSigner(ctx, db, cfg, vault, id)
		case envref.SectionSecret: // Secrets section → secrets table by key.
			signer, found, err := signerFromSecret(ctx, db, sec, bare)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("no stored secret named %q (from reference %s)", bare, key)
			}
			return signer, nil
		case envref.SectionVariable: // Variables section → env_vars by key.
			signer, found, err := signerFromEnvVar(ctx, db, bare)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, fmt.Errorf("no variable named %q (from reference %s)", bare, key)
			}
			return signer, nil
		default:
			return nil, fmt.Errorf("reference %q (run context) does not resolve to key material", key)
		}
	}

	// Bare name (legacy/transition): stored secret first — Reveal/parse errors are
	// fatal — then the plaintext env var fallback.
	if signer, found, err := signerFromSecret(ctx, db, sec, key); err != nil || found {
		return signer, err
	}
	if signer, found, err := signerFromEnvVar(ctx, db, key); err != nil || found {
		return signer, err
	}
	return nil, fmt.Errorf("no key material found for %q", key)
}

// signerFromSecret resolves a private key stored in the secrets table by row key.
// found=false means no usable row (caller may fall through); a reveal or parse
// failure is returned with found=true so a corrupt stored key never silently
// falls through to a plaintext env var (PP-M7).
func signerFromSecret(ctx context.Context, db *sql.DB, sec *secrets.Service, key string) (ssh.Signer, bool, error) {
	var secretID string
	if err := db.QueryRowContext(ctx,
		`SELECT id FROM secrets WHERE key = ? ORDER BY (scope IS NULL) DESC LIMIT 1`, key).Scan(&secretID); err != nil || secretID == "" || sec == nil {
		return nil, false, nil
	}
	pem, rerr := sec.Reveal(ctx, secretID)
	if rerr != nil {
		return nil, true, fmt.Errorf("reveal stored key %q: %w", key, rerr)
	}
	if pem == "" {
		return nil, false, nil
	}
	signer, perr := ssh.ParsePrivateKey([]byte(pem))
	return signer, true, perr
}

// signerFromEnvVar resolves a plaintext PEM stored in env_vars by row key.
func signerFromEnvVar(ctx context.Context, db *sql.DB, key string) (ssh.Signer, bool, error) {
	var val sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM env_vars WHERE key = ? LIMIT 1`, key).Scan(&val); err != nil || !val.Valid || val.String == "" {
		return nil, false, nil
	}
	signer, perr := ssh.ParsePrivateKey([]byte(val.String))
	return signer, true, perr
}

// hostKeyCallback returns a strict callback when a host key is stored, or a
// TOFU-capture callback (writes the first-seen key, audited) when none is.
// There is no insecure-ignore path (EX-D3).
func (s *Service) hostKeyCallback(t target) ssh.HostKeyCallback {
	if t.HostKey != "" {
		expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
		if err != nil {
			// Stored key is unparseable → refuse rather than fall open.
			return func(string, net.Addr, ssh.PublicKey) error {
				return fmt.Errorf("stored host key for %q is unparseable", t.Name)
			}
		}
		return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
			if string(presented.Marshal()) != string(expected.Marshal()) {
				return fmt.Errorf("host key mismatch for %q (possible MITM)", t.Name)
			}
			return nil
		}
	}
	// TOFU: capture the first-seen key and persist it, loudly.
	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		authLine := string(ssh.MarshalAuthorizedKey(presented))
		// Qualify the capture to the SPECIFIC row HostByName resolved (its id), so
		// with M4 dual-source rows the key lands on the row the executor actually
		// dialed — not every row sharing the hostname (§9.4). A target with no row id
		// is a synthetic/unresolved one (no ssh_hosts row to write); skip the capture
		// rather than do the unbounded `WHERE hostname` write the milestone exists to
		// kill. (Bastion hops never use this callback — they pin no key.)
		if t.ID == "" {
			s.log.Warn("TOFU capture skipped: target carries no ssh_hosts row id", "host", t.Name)
			return nil
		}
		if _, err := s.db.Exec(`UPDATE ssh_hosts SET host_key = ? WHERE id = ?`, authLine, t.ID); err != nil {
			s.log.Error("TOFU host-key capture failed", "host", t.Name, "error", err)
		} else {
			s.log.Warn("TOFU host-key captured on first connect (verify out-of-band)", "host", t.Name, "type", presented.Type())
		}
		return nil
	}
}

// dial opens an SSH client to the target — directly, or jumped through its
// bastion (ProxyJump-style). The returned closer tears down both hops.
func (s *Service) dial(ctx context.Context, t target, signer ssh.Signer) (*ssh.Client, func(), error) {
	user := t.User
	if user == "" {
		user = "root"
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: s.hostKeyCallback(t),
		Timeout:         dialTimeout,
	}

	if t.Via == "" {
		client, err := dialContext(ctx, t.DialAddr(), cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("dial %s: %w", t.Name, err)
		}
		return client, func() { client.Close() }, nil
	}

	// Bastion hop: dial the bastion, then dial the target through it.
	b, err := s.bastionAddr(ctx, t.Via)
	if err != nil {
		return nil, nil, err
	}
	bUser := b.User
	if bUser == "" {
		bUser = user
	}
	// Authenticate the bastion hop with the BASTION's own key when configured
	// (PP-H4) — aligning job execution with ProbeBastion. Falling back to the
	// target signer only when the bastion has no key preserves single-key
	// deployments. Presenting the target's key to the bastion (the prior bug)
	// both failed auth for distinct-key bastions and leaked the target key.
	bSigner := signer
	if b.AuthKeyEnvVar != "" || b.AuthCredentialID != "" {
		bSigner, err = loadSigner(ctx, s.db, s.cfg, s.sec, b.AuthCredentialID, b.AuthKeyEnvVar)
		if err != nil {
			return nil, nil, fmt.Errorf("dial bastion %s: %w", t.Via, err)
		}
	}
	bCfg := &ssh.ClientConfig{
		User: bUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(bSigner)},
		// SU-4: verify the bastion host key (was InsecureIgnoreHostKey → first-connect
		// MITM + secret exfil). Strict compare when bastions.host_key is pinned, else
		// TOFU-capture the first-seen key — mirroring the target hostKeyCallback.
		HostKeyCallback: s.bastionHostKeyCallback(b.ID, t.Via, b.HostKey),
		Timeout:         dialTimeout,
	}
	bClient, err := dialContext(ctx, b.DialAddr(), bCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dial bastion %s: %w", t.Via, err)
	}
	conn, err := bClient.DialContext(ctx, "tcp", t.DialAddr())
	if err != nil {
		bClient.Close()
		return nil, nil, fmt.Errorf("bastion %s → %s: %w", t.Via, t.Name, err)
	}
	ncc, chans, reqs, err := ssh.NewClientConn(conn, t.DialAddr(), cfg)
	if err != nil {
		bClient.Close()
		return nil, nil, fmt.Errorf("ssh handshake to %s via bastion: %w", t.Name, err)
	}
	client := ssh.NewClient(ncc, chans, reqs)
	return client, func() { client.Close(); bClient.Close() }, nil
}

func dialContext(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	d := net.Dialer{Timeout: cfg.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// bastionAddr resolves a bastion reference to a dial address, user, and its own
// auth-key env-var name. A host's `via` stores the bastion *name*, so we match
// id / name / hostname, and prefer the `address` column for the actual dial
// target (hostname can be a display name). The bastion's auth_key_env_var (when
// set) lets dial() authenticate the hop with the BASTION's key rather than the
// target's — matching ProbeBastion (PP-H4).
func (s *Service) bastionAddr(ctx context.Context, ref string) (*target, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, hostname, address, port, username, auth_key_env_var, auth_credential_id, host_key FROM bastions
		WHERE id = ? OR name = ? OR hostname = ? LIMIT 1`, ref, ref, ref)
	var id, hostname string
	var address, username, authKey, authCredID, hostKey sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&id, &hostname, &address, &port, &username, &authKey, &authCredID, &hostKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("bastion %q not found", ref)
		}
		return nil, err
	}
	dialHost := address.String
	if dialHost == "" {
		dialHost = hostname
	}
	// Reuse the Target type as the bastion carrier: DialAddr() applies the
	// address→name and port→22 fallbacks, and ID/HostKey drive SU-4 pinning.
	return &target{
		ID:               id,
		Name:             hostname,
		Address:          dialHost,
		Port:             int(port.Int64),
		User:             username.String,
		AuthKeyEnvVar:    authKey.String,
		AuthCredentialID: authCredID.String,
		HostKey:          hostKey.String,
	}, nil
}

// bastionHostKeyCallback mirrors hostKeyCallback for the BASTION hop (SU-4): strict
// compare when a key is pinned in bastions.host_key (mismatch → MITM error;
// unparseable stored key → refuse), else TOFU-capture the first-seen key into the
// bastion row. Replaces the prior InsecureIgnoreHostKey (accept-any) callback.
func (s *Service) bastionHostKeyCallback(bastionID, name, storedKey string) ssh.HostKeyCallback {
	if storedKey != "" {
		expected, _, _, _, err := ssh.ParseAuthorizedKey([]byte(storedKey))
		if err != nil {
			return func(string, net.Addr, ssh.PublicKey) error {
				return fmt.Errorf("stored host key for bastion %q is unparseable", name)
			}
		}
		return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
			if string(presented.Marshal()) != string(expected.Marshal()) {
				return fmt.Errorf("bastion host key mismatch for %q (possible MITM)", name)
			}
			return nil
		}
	}
	// TOFU: capture the first-seen bastion key, loudly.
	return func(_ string, _ net.Addr, presented ssh.PublicKey) error {
		if bastionID == "" {
			s.log.Warn("TOFU capture skipped: bastion carries no row id", "bastion", name)
			return nil
		}
		authLine := string(ssh.MarshalAuthorizedKey(presented))
		if _, err := s.db.Exec(`UPDATE bastions SET host_key = ? WHERE id = ?`, authLine, bastionID); err != nil {
			s.log.Error("TOFU bastion host-key capture failed", "bastion", name, "error", err)
		} else {
			s.log.Warn("TOFU bastion host-key captured on first connect (verify out-of-band)", "bastion", name, "type", presented.Type())
		}
		return nil
	}
}
