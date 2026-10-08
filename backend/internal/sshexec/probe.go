package sshexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Connection-test status values, persisted on ssh_hosts.status / bastions.status
// (ssh-update.md TC.1). A probe only ever yields the four terminal values;
// "unverified" is the never-tested default, never returned by a probe.
// "reachable" is the keyless tier of the two-tier test (TT): the SSH endpoint
// answered and its host key verified/pinned, but no key is configured on the
// row so authentication was not attempted — the expected good outcome for a
// target whose identity comes from a job/run "connect as" override.
const (
	StatusVerified   = "verified"
	StatusReachable  = "reachable"
	StatusCredError  = "cred_error"
	StatusConnError  = "conn_error"
	StatusUnverified = "unverified"
)

// probeTimeout caps a whole probe (covers a bastion hop + the target hop, each
// bounded internally by dialTimeout). TC-D2.
const probeTimeout = 35 * time.Second

// Errors that mean "the test could not be run" (vs. ran-and-failed, which is a
// ProbeResult). The handler maps these to 404 / 409.
var (
	ErrProbeNotFound = errors.New("ssh target not found")
	ErrProbeInFlight = errors.New("a connection test is already in progress for this target")
)

// ProbeResult is the outcome of a single "Test connection" probe. Message is
// operator-safe — it never carries key material or raw handshake dumps.
type ProbeResult struct {
	Status    string `json:"status"`    // verified | reachable | cred_error | conn_error
	Message   string `json:"message"`   // human-readable, safe to display
	LatencyMs int64  `json:"latencyMs"` // dial+auth round-trip
	CheckedAt string `json:"checkedAt"` // RFC3339 UTC
}

// ProbeHost dials an SSH host by id — directly or through its bastion, exactly
// as a job would (it reuses dial/loadSigner/hostKeyCallback) — and reports
// whether login succeeded. A ran-and-failed probe returns (result, nil); only
// not-found / in-flight return an error.
func (s *Service) ProbeHost(ctx context.Context, hostID string) (ProbeResult, error) {
	release, ok := s.probeAcquire("host:" + hostID)
	if !ok {
		return ProbeResult{}, ErrProbeInFlight
	}
	defer release()

	t, err := hostByID(ctx, s.db, hostID)
	if err != nil {
		return ProbeResult{}, err
	}
	if t == nil {
		return ProbeResult{}, ErrProbeNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	start := time.Now()
	// Two-tier test (TT): a target with no key of its own gets the
	// unauthenticated reachability tier instead of an up-front cred_error —
	// keyless is a legitimate config when identity comes from a job/run
	// "connect as" override, and the reachability tier still verifies/pins the
	// host key, which is the part only the target row can carry.
	if t.AuthCredentialID == "" && t.AuthKeyEnvVar == "" {
		status, msg := s.probeReachableHost(ctx, *t)
		return result(status, msg, start), nil
	}
	guard, err := hostKeyGuard(ctx, s.db, hostID)
	if err != nil {
		return ProbeResult{}, err
	}
	signer, err := loadSigner(ctx, s.db, s.cfg, s.sec, t.AuthCredentialID, t.AuthKeyEnvVar, guard)
	if err != nil {
		return result(StatusCredError, credMsg(err), start), nil
	}
	_, closer, err := s.dial(ctx, *t, signer)
	if err != nil {
		status, msg := classifyDialErr(err)
		return result(status, msg, start), nil
	}
	closer()
	return result(StatusVerified, verifiedMsg(signer), start), nil
}

// ProbeBastion dials the bastion itself, authenticating with the bastion's own
// username + auth key. Like job execution's bastion hop, it verifies the
// bastion's SSH host key (SU-4): a pinned bastions.host_key is strict-compared,
// and an empty one is TOFU-captured on this first connect. A changed key aborts
// the probe. The probe is the natural place to establish the pin.
func (s *Service) ProbeBastion(ctx context.Context, bastionID string) (ProbeResult, error) {
	release, ok := s.probeAcquire("bastion:" + bastionID)
	if !ok {
		return ProbeResult{}, ErrProbeInFlight
	}
	defer release()

	b, err := bastionByID(ctx, s.db, bastionID)
	if err != nil {
		return ProbeResult{}, err
	}
	if b == nil {
		return ProbeResult{}, ErrProbeNotFound
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	start := time.Now()
	// Two-tier test (TT), same as ProbeHost: a keyless bastion still gets its
	// reachability checked and its host key pinned.
	if b.AuthCredentialID == "" && b.AuthKeyEnvVar == "" {
		status, msg := s.probeReachableBastion(ctx, *b)
		return result(status, msg, start), nil
	}
	// LR-72: the bastion's key must be its owner's or Global's, here as at connect.
	guard, err := ownerKeyGuard(ctx, s.db, b.Owners)
	if err != nil {
		return ProbeResult{}, err
	}
	signer, err := loadSigner(ctx, s.db, s.cfg, s.sec, b.AuthCredentialID, b.AuthKeyEnvVar, guard)
	if err != nil {
		return result(StatusCredError, credMsg(err), start), nil
	}
	user := b.User
	if user == "" {
		user = "root"
	}
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		// SU-4: verify the bastion's host key. A test verifies and never
		// captures: an unknown key is reported, with where to approve it.
		Timeout: dialTimeout,
	}
	cfg.HostKeyCallback, cfg.HostKeyAlgorithms = s.bastionTrust(ctx, *b, "")
	client, err := dialContext(ctx, b.DialAddr(), cfg)
	if err != nil {
		status, msg := classifyDialErr(err)
		return result(status, msg, start), nil
	}
	client.Close()
	return result(StatusVerified, verifiedMsg(signer), start), nil
}

func result(status, msg string, start time.Time) ProbeResult {
	return ProbeResult{
		Status:    status,
		Message:   msg,
		LatencyMs: time.Since(start).Milliseconds(),
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// verifiedMsg names the key that authenticated so a passing test confirms WHICH
// key/credential the host or bastion is actually using (ssh-keys-update.md SK.12).
// Only the public type + SHA256 fingerprint — never key material.
func verifiedMsg(signer ssh.Signer) string {
	pub := signer.PublicKey()
	return "connection verified — authenticated with " + pub.Type() + " " + ssh.FingerprintSHA256(pub)
}

// ── Reachability tier (TT) ───────────────────────────────────────────────────
//
// The unauthenticated half of the two-tier connection test: complete the SSH
// handshake only as far as host-key verification — the same strict-compare /
// TOFU-capture callback job execution uses — then abort with a sentinel before
// authentication would begin (the agent's keyscan.go pattern). Proves the SSH
// endpoint is up and pins/verifies its identity without needing any credential.

// errReachableDone aborts the handshake once the host key has verified; seeing
// it means the probe succeeded. Detection is by the captured key, not by
// matching this error's text inside x/crypto's handshake wrapper.
var errReachableDone = errors.New("cronomicon: reachability probe complete")

// errBastionNoKey marks the one hop the reachability tier cannot skip auth on:
// a keyless target behind a bastion still needs the BASTION's own key to hop.
var errBastionNoKey = errors.New("bastion has no auth credential or key configured")

// reachCapture carries the host key out of the aborted handshake.
type reachCapture struct{ key ssh.PublicKey }

// reachConfig wraps inner so the handshake verifies/pins the host key as
// normal, records it, and then aborts before auth (no auth methods are offered).
func reachConfig(inner ssh.HostKeyCallback, cap *reachCapture) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		// Never authenticates — a recognizable name keeps the remote side's
		// auth log explicable (mirrors keyscan's "cronomicon-keyscan").
		User: "cronomicon-probe",
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			if err := inner(hostname, remote, key); err != nil {
				return err // pin mismatch / unparseable pin — surfaces as conn_error
			}
			cap.key = key
			return errReachableDone
		},
		Timeout: dialTimeout,
	}
}

// probeReachableHost runs the reachability tier against a host — directly or
// through its bastion (which authenticates normally with its own key).
func (s *Service) probeReachableHost(ctx context.Context, t target) (status, msg string) {
	var cap reachCapture
	cfg := reachConfig(s.hostKeyCallback(ctx, t), &cap)
	_, cfg.HostKeyAlgorithms = s.hostTrust(ctx, t)
	var err error
	if t.Via == "" {
		_, err = dialContext(ctx, t.DialAddr(), cfg)
	} else {
		err = s.reachViaBastion(ctx, t, cfg)
	}
	if cap.key != nil {
		return StatusReachable, reachableMsg(cap.key)
	}
	if errors.Is(err, errBastionNoKey) {
		return StatusCredError, "bastion " + strconv.Quote(t.Via) + " has no auth key — the bastion hop must authenticate before this keyless target can be reached; set an Auth Key on the bastion"
	}
	return classifyDialErr(err)
}

// probeReachableBastion runs the reachability tier against the bastion itself.
func (s *Service) probeReachableBastion(ctx context.Context, b target) (status, msg string) {
	var cap reachCapture
	cfg := reachConfig(s.bastionHostKeyCallback(ctx, b, ""), &cap)
	_, cfg.HostKeyAlgorithms = s.bastionTrust(ctx, b, "")
	_, err := dialContext(ctx, b.DialAddr(), cfg)
	if cap.key != nil {
		return StatusReachable, reachableMsg(cap.key)
	}
	return classifyDialErr(err)
}

// reachViaBastion performs the unauthenticated target handshake through the
// host's bastion. Unlike dial(), there is no target signer to fall back to for
// the hop, so the bastion must carry its own key.
func (s *Service) reachViaBastion(ctx context.Context, t target, cfg *ssh.ClientConfig) error {
	b, err := s.bastionAddr(ctx, t.Via, t.Owners)
	if err != nil {
		return err
	}
	if b.AuthCredentialID == "" && b.AuthKeyEnvVar == "" {
		return fmt.Errorf("bastion %q: %w", t.Via, errBastionNoKey)
	}
	guard, err := ownerKeyGuard(ctx, s.db, b.Owners)
	if err != nil {
		return fmt.Errorf("dial bastion %s: %w", t.Via, err)
	}
	bSigner, err := loadSigner(ctx, s.db, s.cfg, s.sec, b.AuthCredentialID, b.AuthKeyEnvVar, guard)
	if err != nil {
		return fmt.Errorf("dial bastion %s: %w", t.Via, err)
	}
	bUser := b.User
	if bUser == "" {
		bUser = "root"
	}
	bCfg := &ssh.ClientConfig{
		User:    bUser,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(bSigner)},
		Timeout: dialTimeout,
	}
	bCfg.HostKeyCallback, bCfg.HostKeyAlgorithms = s.bastionTrust(ctx, *b, t.Via)
	bClient, err := dialContext(ctx, b.DialAddr(), bCfg)
	if err != nil {
		return fmt.Errorf("dial bastion %s: %w", t.Via, err)
	}
	defer bClient.Close()
	conn, err := bClient.DialContext(ctx, "tcp", t.DialAddr())
	if err != nil {
		return fmt.Errorf("bastion %s → %s: %w", t.Via, t.Name, err)
	}
	defer conn.Close()
	_, _, _, hErr := ssh.NewClientConn(conn, t.DialAddr(), cfg)
	return hErr
}

// reachableMsg mirrors verifiedMsg for the keyless tier: it names the HOST key
// that was verified/pinned (never the missing auth key) and is explicit that
// authentication was not exercised.
func reachableMsg(pub ssh.PublicKey) string {
	return "host reachable — SSH endpoint answered and presented host key " +
		pub.Type() + " " + ssh.FingerprintSHA256(pub) +
		"; no auth key is configured on this entry, so authentication was not tested"
}

// probeAcquire takes the per-target lock without blocking. ok=false ⇒ a probe
// is already running for this target.
func (s *Service) probeAcquire(key string) (release func(), ok bool) {
	v, _ := s.probeMu.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// classifyDialErr splits a dial/handshake failure into cred_error (we reached
// the SSH server but auth failed) vs conn_error (couldn't reach it, or the host
// key didn't verify). The x/crypto/ssh package exports no typed auth error, so
// the auth case is matched by its sentinel substring — checked first, because a
// handshake-failed wrapper can carry an auth cause.
func classifyDialErr(err error) (status, msg string) {
	if err == nil {
		return StatusVerified, "connection verified"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusConnError, "timed out connecting to the target"
	}
	es := err.Error()
	var passErr *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &passErr),
		errors.Is(err, errKeyNotUsable),
		strings.Contains(es, "no key material"):
		// A key-load failure (PP-H4: the bastion's own key now loads inside dial,
		// wrapped as "dial bastion …: …"). Classify as a credential error and
		// surface the actionable loadSigner hint rather than a generic "could not
		// connect", matching the up-front target-key path.
		return StatusCredError, credMsg(err)
	case strings.Contains(es, "unable to authenticate"),
		strings.Contains(es, "no supported methods remain"),
		strings.Contains(es, "ssh: rejected"):
		return StatusCredError, "authentication rejected — verify the key is authorized for the remote user"
	case strings.Contains(es, HostKeyUnverified):
		// Reached, and its key is not one an operator has approved. A test never
		// captures a key (2.3.0): it says so, WHICH hop it was (a target behind a
		// bastion can fail on either), and where the key is approved.
		msg := "the local runner has no approved host key for it — scan it and approve its key under Runners → Local runner → Host keys"
		if i := strings.Index(es, HostKeyUnverified+": "); i >= 0 {
			msg = es[i+len(HostKeyUnverified)+2:]
		}
		return StatusUnverified, "reached, but " + msg
	case strings.Contains(es, "host key mismatch"),
		strings.Contains(es, "possible MITM"),
		strings.Contains(es, "unparseable"):
		return StatusConnError, "host key verification failed — possible MITM"
	default:
		// DNS, connection refused, network unreachable, dial timeout, or a
		// failing bastion hop — the wrapped error names the hop, which is what
		// the operator needs. Net errors carry no secrets.
		return StatusConnError, "could not connect: " + err.Error()
	}
}

// credMsg turns a loadSigner failure into an operator-safe hint without echoing
// any key material. The parse-failure branches are deliberately specific: a
// passphrase-protected key is the most common "valid key that still won't load"
// because x/crypto/ssh refuses encrypted keys (typed PassphraseMissingError).
//
// There is deliberately NO branch for loadSigner's errNoAuth (a row with neither
// a credential nor a key name). Every probe path guards that case before calling
// loadSigner — a keyless target or bastion takes the reachability tier, and a
// keyless bastion under a keyless target is refused by the errBastionNoKey
// sentinel — so the error cannot arrive here. LB17 was a branch for it that
// matched a string nothing emitted; DD-3 corrected the string on the mistaken
// belief that a bastion key loading inside dial could produce it, but dial()
// only calls loadSigner when the bastion actually has a credential or key name.
// TestNoAuthErrorNeverReachesCredMsg pins each guard, so the day one is removed
// the test says so and this branch can come back as an errors.Is check.
func credMsg(err error) string {
	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		return "the SSH key is passphrase-protected (encrypted); Cronomicon needs an unencrypted private key — re-export the key without a passphrase"
	}
	// A key the record's agency may not use is not a malformed key, and saying
	// so sent people to re-export a key that was fine.
	if errors.Is(err, errKeyNotUsable) {
		return errKeyNotUsable.Error()
	}
	es := err.Error()
	switch {
	case strings.Contains(es, "no key material"):
		return "the configured Auth Key has no stored value (check the env var / secret name)"
	default:
		return "the configured SSH key value is not a parseable private key — it must be an unencrypted PEM/OpenSSH private key, not a public key (.pub), certificate, or PuTTY .ppk"
	}
}

// hostByID loads an ssh_hosts row as a dial target, keyed by id (the API works
// in ids; execspec.HostByName is the executor's by-name path).
// hostKeyGuard is the keyGuard for "Test connection" on a host record. A record
// imported for a scope is edited and tested by that scope's agency, so its key
// is checked against that agency exactly as a run's would be: the test must
// not be a way to authenticate with a key a run could not use. A record written
// by hand is its owner agency's (LR-69), and its key is checked against that
// owner (LR-72); until 2.3.0 it had none and was not checked at all.
func hostKeyGuard(ctx context.Context, db *sql.DB, hostID string) (keyGuard, error) {
	var scope sql.NullString
	var owner string
	err := db.QueryRowContext(ctx, `
		SELECT s.name, h.owner_agency FROM ssh_hosts h LEFT JOIN scopes s ON s.id = h.scope_id WHERE h.id = ?`, hostID).Scan(&scope, &owner)
	if errors.Is(err, sql.ErrNoRows) {
		return keyGuard{checked: true}, nil // no such host: nothing but Global's keys
	}
	if err != nil {
		return keyGuard{}, err
	}
	if scope.String == "" {
		return ownerKeyGuard(ctx, db, []string{owner})
	}
	agencies, err := execspec.ScopeAgencies(ctx, db, scope.String)
	if err != nil {
		return keyGuard{}, err // an unreadable membership must not read as "no agency"
	}
	return keyGuard{checked: true, scope: scope.String, agencies: agencies}, nil
}

// hostOwners returns the ids of the agencies a host record answers to (LR-69,
// LR-70): its scope's agencies when it was imported for a scope, its own owner
// when it was written by hand.
func hostOwners(ctx context.Context, db *sql.DB, hostID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sa.agency_id FROM ssh_hosts h JOIN scope_agencies sa ON sa.scope_id = h.scope_id WHERE h.id = ?
		UNION
		SELECT h.owner_agency FROM ssh_hosts h WHERE h.id = ? AND h.scope_id IS NULL`, hostID, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func hostByID(ctx context.Context, db *sql.DB, id string) (*target, error) {
	row := db.QueryRowContext(ctx, `
		SELECT hostname, address, port, username, via, auth_key_env_var, auth_credential_id
		FROM ssh_hosts WHERE id = ? LIMIT 1`, id)
	var name string
	var address, user, via, authKeyEnvVar, authCredentialID sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&name, &address, &port, &user, &via, &authKeyEnvVar, &authCredentialID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	owners, err := hostOwners(ctx, db, id)
	if err != nil {
		return nil, err
	}
	return &target{
		ID:               id,
		Name:             name,
		Address:          address.String,
		Port:             int(port.Int64),
		User:             user.String,
		Via:              via.String,
		AuthKeyEnvVar:    authKeyEnvVar.String,
		AuthCredentialID: authCredentialID.String,
		Owners:           owners,
	}, nil
}

// bastionByID loads a bastions row as a direct-dial target (no nested via). The
// `address` column is the dial address; `name` is the display identifier.
func bastionByID(ctx context.Context, db *sql.DB, id string) (*target, error) {
	row := db.QueryRowContext(ctx, `
		SELECT name, address, port, username, auth_key_env_var, auth_credential_id, owner_agency
		FROM bastions WHERE id = ? LIMIT 1`, id)
	var name, owner string
	var address, user, authKeyEnvVar, authCredentialID sql.NullString
	var port sql.NullInt64
	if err := row.Scan(&name, &address, &port, &user, &authKeyEnvVar, &authCredentialID, &owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &target{
		ID:               id,
		Name:             name,
		Address:          address.String,
		Port:             int(port.Int64),
		User:             user.String,
		AuthKeyEnvVar:    authKeyEnvVar.String,
		AuthCredentialID: authCredentialID.String,
		Owners:           []string{owner},
	}, nil
}
