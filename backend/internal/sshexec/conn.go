package sshexec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"net"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/notices"
	"github.com/ResetSmith/cronomicon/internal/runref"
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
//
// guard says on whose behalf the key is loaded (see keyGuard). With a checked
// guard every lookup — by credential id and by name alike — goes through the
// agency-checked resolver the runner path has always used, so the key a run
// connects with is one its agency may use.
func loadSigner(ctx context.Context, db *sql.DB, cfg *config.Config, sec *secrets.Service, credID, key string, guard keyGuard) (ssh.Signer, error) {
	// A vault-source ssh_credential resolves through this executor's configured
	// Vault client (P2.4); nil when Vault is unconfigured (stored keys still work).
	var vault secrets.VaultClient
	if sec != nil {
		vault = sec.Vault()
	}
	if credID != "" {
		if guard.checked {
			ok, err := runref.KeyIDUsable(ctx, db, credID, guard.agencies)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, errKeyNotUsable
			}
		}
		return sshkeys.ResolveSigner(ctx, db, cfg, vault, credID)
	}
	if key == "" {
		return nil, errNoAuth
	}
	if guard.checked {
		return loadSignerChecked(ctx, db, cfg, sec, vault, key, guard)
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

// keyGuard says on whose behalf a key is being loaded.
//
// Until v2.2.3 this path asked nobody: a credential id was loaded as given, and
// a name was looked up with `LIMIT 1` across every agency and every scope. A
// host record is written by the administrator of ITS scope's agency, and its
// key reference is free text (an id any session can list, or a name in the
// scope's inventory), so that administrator could point their host at another
// agency's key and their runs would authenticate with it. The runner path never
// had the hole — it resolves keys through runref with the run's agency
// snapshot. checked makes this path ask the same question.
//
// The zero value is unchecked and has no caller left in this package since
// 2.3.0: a bastion and a hand-written host record have an owner now, and their
// keys are checked against it (ownerKeyGuard).
type keyGuard struct {
	checked  bool
	scope    string   // the run's scope (the secret and variable lookups are scoped)
	agencies []string // the run's agency NAMES, as runs.agencies_json holds them
}

// errKeyNotUsable is deliberately the same sentence whether the key does not
// exist or belongs to another agency: a host record must not be usable as a
// probe for which keys another agency holds.
var errKeyNotUsable = errors.New("the SSH key this record names is not one its agency may use " +
	"(it belongs to another agency, or no longer exists) — use a key of the agency's own, or one that is Global's")

// loadSignerChecked is loadSigner's name path under a checked guard: the same
// routing by prefix, with each lookup made by runref.LookupEntityID — the row
// dispatch would inject for this run's scope and agencies, an agency's own row
// before a shared one, and nothing of another agency's.
func loadSignerChecked(ctx context.Context, db *sql.DB, cfg *config.Config, sec *secrets.Service, vault secrets.VaultClient, key string, guard keyGuard) (ssh.Signer, error) {
	lookup := func(kind runref.Kind, name string) (string, bool, error) {
		return runref.LookupEntityID(ctx, db, kind, name, guard.scope, guard.agencies)
	}
	fromSecret := func(name string) (ssh.Signer, bool, error) {
		id, found, err := lookup(runref.KindSecret, name)
		if err != nil || !found || sec == nil {
			return nil, false, err
		}
		pem, rerr := sec.Reveal(ctx, id)
		if rerr != nil {
			return nil, true, fmt.Errorf("reveal stored key %q: %w", name, rerr)
		}
		if pem == "" {
			return nil, false, nil
		}
		signer, perr := ssh.ParsePrivateKey([]byte(pem))
		return signer, true, perr
	}
	fromVar := func(name string) (ssh.Signer, bool, error) {
		id, found, err := lookup(runref.KindVar, name)
		if err != nil || !found {
			return nil, false, err
		}
		var val sql.NullString
		if err := db.QueryRowContext(ctx, `SELECT value FROM env_vars WHERE id = ?`, id).Scan(&val); err != nil || !val.Valid || val.String == "" {
			return nil, false, nil
		}
		signer, perr := ssh.ParsePrivateKey([]byte(val.String))
		return signer, true, perr
	}

	if section, bare, ok := envref.Split(key); ok {
		switch section {
		case envref.SectionKey:
			id, found, err := lookup(runref.KindKey, bare)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, errKeyNotUsable
			}
			return sshkeys.ResolveSigner(ctx, db, cfg, vault, id)
		case envref.SectionSecret:
			signer, found, err := fromSecret(bare)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, errKeyNotUsable
			}
			return signer, nil
		case envref.SectionVariable:
			signer, found, err := fromVar(bare)
			if err != nil {
				return nil, err
			}
			if !found {
				return nil, errKeyNotUsable
			}
			return signer, nil
		default:
			return nil, fmt.Errorf("reference %q (run context) does not resolve to key material", key)
		}
	}
	if signer, found, err := fromSecret(key); err != nil || found {
		return signer, err
	}
	if signer, found, err := fromVar(key); err != nil || found {
		return signer, err
	}
	return nil, errKeyNotUsable
}

// keyNameResolves reports whether a key NAME resolves to a row under a checked
// guard: loadSignerChecked's routing, without loading any material. It exists
// so "would this record's key load" can be asked of every record at once (the
// notices check below) by the same rule a connect applies.
func keyNameResolves(ctx context.Context, db *sql.DB, key string, guard keyGuard) (bool, error) {
	lookup := func(kind runref.Kind, name string) (bool, error) {
		_, found, err := runref.LookupEntityID(ctx, db, kind, name, guard.scope, guard.agencies)
		if errors.Is(err, runref.ErrAmbiguousReference) {
			return false, nil // refused at connect too
		}
		return found, err
	}
	if section, bare, ok := envref.Split(key); ok {
		switch section {
		case envref.SectionKey:
			return lookup(runref.KindKey, bare)
		case envref.SectionSecret:
			return lookup(runref.KindSecret, bare)
		case envref.SectionVariable:
			return lookup(runref.KindVar, bare)
		default:
			return false, nil
		}
	}
	if found, err := lookup(runref.KindSecret, key); err != nil || found {
		return found, err
	}
	return lookup(runref.KindVar, key)
}

// BastionKeyNameFindings is the by-name half of the notices check
// record_key_outside_owner (installed with notices.SetRecordKeyNameCheck): the
// bastions that name their key by NAME — a secret, a variable or a key label,
// as records did before SSH keys were first-class — where that name resolves to
// nothing the bastion's owner may use.
//
// Until 2.3.0 a bastion belonged to nobody and its key was looked up across
// every agency and every scope. It is loaded for its owner now, so a bastion
// that predates this and names a department's or a scoped row fails every run
// routed through it, and nothing else would say so.
func BastionKeyNameFindings(ctx context.Context, db *sql.DB) ([]notices.Finding, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT b.id, b.name, b.owner_agency, COALESCE((SELECT name FROM agencies WHERE id = b.owner_agency), b.owner_agency), b.auth_key_env_var
		  FROM bastions b
		 WHERE COALESCE(b.auth_credential_id, '') = '' AND COALESCE(b.auth_key_env_var, '') <> ''
		 ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	type rec struct{ id, name, owner, ownerName, key string }
	var recs []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.name, &r.owner, &r.ownerName, &r.key); err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // before the lookups: never a query inside an open cursor
	var out []notices.Finding
	for _, r := range recs {
		guard, err := ownerKeyGuard(ctx, db, []string{r.owner})
		if err != nil {
			return nil, err
		}
		ok, err := keyNameResolves(ctx, db, r.key, guard)
		if err != nil {
			return nil, err
		}
		if ok {
			continue
		}
		out = append(out, notices.Finding{
			AgencyID: r.owner,
			Subject:  "bastion:" + r.id,
			Detail: fmt.Sprintf("The bastion %s belongs to %s and names its SSH key as %s, which is not a key, secret or variable "+
				"that %s may use (its own or Global's, with no scope). Every run that routes through it fails. "+
				"Give the bastion a key of its own agency or one that is Global's, or give the bastion to the agency whose key it names.",
				r.name, r.ownerName, r.key, r.ownerName),
		})
	}
	return out, nil
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

// dial opens an SSH client to the target — directly, or jumped through its
// bastion (ProxyJump-style). The returned closer tears down both hops.
func (s *Service) dial(ctx context.Context, t target, signer ssh.Signer) (*ssh.Client, func(), error) {
	user := t.User
	if user == "" {
		user = "root"
	}
	cfg := &ssh.ClientConfig{
		User:    user,
		Auth:    []ssh.AuthMethod{ssh.PublicKeys(signer)},
		Timeout: dialTimeout,
	}
	cfg.HostKeyCallback, cfg.HostKeyAlgorithms = s.hostTrust(ctx, t)

	if t.Via == "" {
		client, err := dialContext(ctx, t.DialAddr(), cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("dial %s: %w", t.Name, err)
		}
		return client, func() { client.Close() }, nil
	}

	// Bastion hop: dial the bastion, then dial the target through it.
	b, err := s.bastionAddr(ctx, t.Via, t.Owners)
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
		// LR-72 at connect: a bastion's key is its owner's, or Global's. A
		// bastion belonged to no one until 2.3.0 and this load asked nothing.
		guard, gerr := ownerKeyGuard(ctx, s.db, b.Owners)
		if gerr != nil {
			return nil, nil, fmt.Errorf("dial bastion %s: %w", t.Via, gerr)
		}
		bSigner, err = loadSigner(ctx, s.db, s.cfg, s.sec, b.AuthCredentialID, b.AuthKeyEnvVar, guard)
		if err != nil {
			return nil, nil, fmt.Errorf("dial bastion %s: %w", t.Via, err)
		}
	}
	bCfg := &ssh.ClientConfig{
		User: bUser,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(bSigner)},
		// SU-4: verify the bastion hop like the target's, against the local
		// runner's approved keys (it was InsecureIgnoreHostKey once, and then
		// capture-on-first-connect).
		Timeout: dialTimeout,
	}
	bCfg.HostKeyCallback, bCfg.HostKeyAlgorithms = s.bastionTrust(ctx, *b, t.Via)
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

// bastionAddr resolves a host record's `via` to the bastion the engine hops
// through (execspec.BastionByRef: the record's own agency's bastion, or
// Global's — LR-70).
func (s *Service) bastionAddr(ctx context.Context, ref string, owners []string) (*target, error) {
	return execspec.BastionByRef(ctx, s.db, ref, owners)
}

// ownerKeyGuard is the keyGuard for a key that a RECORD names for itself — a
// bastion's, or a hand-written host record's under "Test connection": the key
// must belong to one of the record's owning agencies, or to Global (LR-72). No
// owner at all (a record that could not be resolved) leaves only Global's keys.
func ownerKeyGuard(ctx context.Context, db *sql.DB, ownerIDs []string) (keyGuard, error) {
	g := keyGuard{checked: true}
	for _, id := range ownerIDs {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return keyGuard{}, err // an unreadable owner must not read as "Global's"
		}
		g.agencies = append(g.agencies, name)
	}
	return g, nil
}
