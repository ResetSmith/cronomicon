package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/envref"
	"github.com/ResetSmith/cronomicon/internal/remotecmd"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ensureKnownHostsFile creates an empty known_hosts file (0640) at path if it
// doesn't exist, so a fresh runner has a writable trust store for scan &
// approve (Phase 5). Existing files are left untouched.
func ensureKnownHostsFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o750)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

const sshDialTimeout = 15 * time.Second

// sshRunner holds the agent's OWN key custody (model b, D1): it resolves a
// target's AuthKeyEnvVar NAME to a local private key the agent holds — the
// server never ships key bytes.
type sshRunner struct {
	keyMap         map[string]string // authKeyEnvVar NAME → local key file path
	keyDir         string            // fallback dir searched by name
	knownHostsFile string            // optional OpenSSH known_hosts for verification
	fanOutN        int               // max parallel targets within a run
}

// resolveKeyPath resolves a credential NAME to a local private-key FILE path
// using the agent's own key custody: an explicit key-map entry first, then a
// key-dir file named after NAME (<name>, <name>.pem, <name>.key). It returns the
// resolved path and its source ("key-map" or "key-dir"). Unlike loadSigner it
// deliberately does NOT consider an env-held PEM — a PEM is bytes, not a path, so
// it cannot be handed to ansible's ansible_ssh_private_key_file (D-A). This is
// the single resolver shared by the Go-SSH signer path (loadSigner, steps 1+3)
// and the ansible env bridge (buildChildEnv), so one provisioned key file serves
// every run type.
func resolveKeyPath(keyMap map[string]string, keyDir, name string) (path, src string, ok bool) {
	if name == "" {
		return "", "", false
	}
	if p, s, ok := resolveKeyPathExact(keyMap, keyDir, name); ok {
		return p, s, true
	}
	// Derived reference (W3): an AMADEUS_KEY_<bare> reference resolves to the same
	// key-map entry / key-dir file as its BARE name — files and map entries are
	// never renamed, only reference sites move to the prefixed form.
	if bare, stripped := envref.StripKey(name); stripped {
		return resolveKeyPathExact(keyMap, keyDir, bare)
	}
	return "", "", false
}

// resolveKeyPathExact is resolveKeyPath without the derived-reference strip — the
// literal key-map/key-dir lookup for one exact NAME.
func resolveKeyPathExact(keyMap map[string]string, keyDir, name string) (path, src string, ok bool) {
	if p, ok := keyMap[name]; ok {
		return p, "key-map", true
	}
	if keyDir != "" {
		for _, cand := range []string{name, name + ".pem", name + ".key"} {
			p := filepath.Join(keyDir, cand)
			if _, err := os.Stat(p); err == nil {
				return p, "key-dir", true
			}
		}
	}
	return "", "", false
}

// defaultedConfig applies the runtime defaults New() computes but Resolve()
// deliberately does not — currently the known_hosts path beside the identity
// file. Centralized so the live agent and the doctor auth-probe observe the
// SAME trust store rather than drifting.
func defaultedConfig(cfg Config) Config {
	if cfg.KnownHostsFile == "" && cfg.IdentityFile != "" {
		cfg.KnownHostsFile = filepath.Join(filepath.Dir(cfg.IdentityFile), "known_hosts")
	}
	return cfg
}

// sshRunnerFor builds the agent's Go-SSH runner from a (defaulted) config. Shared
// by New() and the doctor auth-probe so a `doctor --auth` dial uses byte-for-byte
// the same key custody + trust store a real run would.
func sshRunnerFor(cfg Config) *sshRunner {
	return &sshRunner{
		keyMap:         cfg.KeyMap,
		keyDir:         cfg.KeyDir,
		knownHostsFile: cfg.KnownHostsFile,
		fanOutN:        cfg.FanOut,
	}
}

// keyNameFromFile maps a key-dir FILENAME back to the credential NAME the
// resolver would match it under — the inverse of resolveKeyPath's candidate list
// {name, name.pem, name.key}. It strips at most ONE of .pem/.key (never both), so
// a file "svc.key.pem" yields "svc.key" (which resolveKeyPath finds via its .pem
// candidate), not "svc". Shared by the doctor key-names check and the declared
// key-names blob so both agree with the resolver.
func keyNameFromFile(fn string) string {
	if before, ok := strings.CutSuffix(fn, ".pem"); ok {
		return before
	}
	if before, ok := strings.CutSuffix(fn, ".key"); ok {
		return before
	}
	return fn
}

// loadSigner resolves the private key for a target by its authKeyEnvVar NAME,
// using ONLY local key custody: an explicit key-map entry, an env var holding a
// PEM, or a file in key-dir named after the var. Never receives key bytes from
// the server.
func (r *sshRunner) loadSigner(authKeyEnvVar string) (ssh.Signer, error) {
	if authKeyEnvVar == "" {
		return nil, fmt.Errorf("target has no authKeyEnvVar configured")
	}
	// 1. Explicit key-map: NAME → local file path (shared resolver, key-map only).
	if path, _, ok := resolveKeyPath(r.keyMap, "", authKeyEnvVar); ok {
		return signerFromFile(path)
	}
	// 2. Env var of that NAME holding a PEM (the agent's local environment). This
	// is Go-SSH only: a PEM in env cannot be bridged to ansible (see resolveKeyPath).
	if pem := os.Getenv(authKeyEnvVar); pem != "" {
		signer, err := ssh.ParsePrivateKey([]byte(pem))
		if err != nil {
			return nil, fmt.Errorf("parse key from env %s: %w", authKeyEnvVar, err)
		}
		return signer, nil
	}
	// 3. key-dir search (shared resolver, key-dir only): <name>, <name>.pem, <name>.key.
	if path, _, ok := resolveKeyPath(nil, r.keyDir, authKeyEnvVar); ok {
		return signerFromFile(path)
	}
	// Nothing resolved: name every place we looked and the fix. Distinguish "no
	// key-dir configured" from "configured but the file is not there" — the two
	// have different remedies.
	if r.keyDir == "" {
		return nil, fmt.Errorf("no local key for %q: not in key-map, no PEM in env %s, and no key-dir configured — set -key-dir / AMADEUS_RUNNER_KEY_DIR (then drop a file named %s, %s.pem, or %s.key in it) or add a -key-map entry",
			authKeyEnvVar, authKeyEnvVar, authKeyEnvVar, authKeyEnvVar, authKeyEnvVar)
	}
	return nil, fmt.Errorf("no local key for %q: not in key-map, no PEM in env %s, and no file named %s, %s.pem, or %s.key under key-dir %q",
		authKeyEnvVar, authKeyEnvVar, authKeyEnvVar, authKeyEnvVar, authKeyEnvVar, r.keyDir)
}

func signerFromFile(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key file %q: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("parse key file %q: %w", path, err)
	}
	return signer, nil
}

// hostKeyCallback returns a strict callback. When a known_hosts file is
// configured the presented key must be in it (documented onboarding mode); when
// none is configured we refuse rather than fall open. There is no
// insecure-ignore path for the target hop.
func (r *sshRunner) hostKeyCallback() (ssh.HostKeyCallback, error) {
	if r.knownHostsFile == "" {
		// No path at all: refuse loudly (New defaults a path, so this is only
		// hit in tests / odd configs).
		return func(string, net.Addr, ssh.PublicKey) error {
			return fmt.Errorf("host-key verification not configured; refusing to connect")
		}, nil
	}
	// Ensure the file exists so a fresh runner (no -known-hosts pre-seed) can be
	// trust-seeded via scan & approve (Phase 5): an EMPTY known_hosts makes the
	// callback refuse an unknown host with a *knownhosts.KeyError — exactly the
	// host_key_unverified trigger — and trust-hosts appends the approved line.
	if err := ensureKnownHostsFile(r.knownHostsFile); err != nil {
		return nil, fmt.Errorf("prepare known_hosts %q: %w", r.knownHostsFile, err)
	}
	cb, err := knownhosts.New(r.knownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %q: %w", r.knownHostsFile, err)
	}
	return cb, nil
}

// isHostKeyError reports whether an SSH dial error is a host-key verification
// failure (unknown or mismatched host) vs any other connect error.
func isHostKeyError(err error) bool {
	if _, ok := errors.AsType[*knownhosts.KeyError](err); ok {
		return true
	}
	// The handshake wraps the callback error; match its stable text too.
	return strings.Contains(err.Error(), "knownhosts:") ||
		strings.Contains(err.Error(), "host-key verification not configured")
}

// hostResult is one target's outcome in a fan-out run.
type hostResult struct {
	host     string
	exitCode int
	err      error
	// hostKeyUnverified is set when the dial failed host-key verification
	// (Phase 5); scanTarget is the exact dial address the operator should scan
	// (what the known_hosts verifier keys on).
	hostKeyUnverified bool
	scanTarget        string
}

// fanOut runs cmd across the targets with bounded parallelism, prefixing each
// output line with "[host] ". It aggregates the exit per §"all-success→0,
// any-failure→nonzero".
func (r *sshRunner) fanOut(ctx context.Context, targets []runnerproto.ManifestTarget,
	cmd remotecmd.Rendered, emit func(line string)) (exitCode int, reason string) {

	if len(targets) == 0 {
		emit("amadeus: no hosts resolved for this run")
		return 1, ""
	}
	conc := r.fanOutN
	if conc <= 0 {
		conc = 4
	}
	results := make([]hostResult, len(targets))
	sem := make(chan struct{}, conc)
	var emitMu sync.Mutex
	prefixed := func(host, line string) {
		emitMu.Lock()
		defer emitMu.Unlock()
		emit("[" + host + "] " + line)
	}
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t runnerproto.ManifestTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = r.runTarget(ctx, t, cmd, prefixed)
		}(i, t)
	}
	wg.Wait()
	// Surface a host-key verification failure as a structured run reason (Phase
	// 5) so the run detail can offer "Scan & approve host keys". First offender
	// wins; the scanTarget is the exact address to scan.
	for _, res := range results {
		if res.hostKeyUnverified {
			return aggregateExit(results), "host_key_unverified: " + res.scanTarget
		}
	}
	return aggregateExit(results), ""
}

// runTarget connects to one host, runs the command, streams output line-by-line
// (each prefixed by the caller), and returns the host's exit code.
func (r *sshRunner) runTarget(ctx context.Context, t runnerproto.ManifestTarget,
	cmd remotecmd.Rendered, emit func(host, line string)) hostResult {

	if t.ResolveErr != "" {
		emit(t.Name, "amadeus: "+t.ResolveErr)
		return hostResult{host: t.Name, exitCode: -1, err: fmt.Errorf("%s", t.ResolveErr)}
	}
	signer, err := r.loadSigner(t.AuthKeyEnvVar)
	if err != nil {
		emit(t.Name, "amadeus: auth: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}
	client, closeFn, err := r.dial(ctx, t, signer)
	if err != nil {
		if isHostKeyError(err) {
			scan := dialAddr(t.Address, t.Name, t.Port)
			// A parseable marker + a per-host reason the run's envelope carries.
			emit(t.Name, "amadeus: host_key_unverified: "+scan+" — approve this host's key in the Runners view (Scan & approve), then retry")
			return hostResult{host: t.Name, exitCode: -1, err: err, hostKeyUnverified: true, scanTarget: scan}
		}
		emit(t.Name, "amadeus: connect: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}
	defer closeFn()

	session, err := client.NewSession()
	if err != nil {
		emit(t.Name, "amadeus: session: "+err.Error())
		return hostResult{host: t.Name, exitCode: -1, err: err}
	}
	defer session.Close()

	// Cancel the session if the run context is cancelled (kill).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Signal(ssh.SIGKILL)
			_ = session.Close()
		case <-done:
		}
	}()

	stdout, _ := session.StdoutPipe()
	stderr, _ := session.StderrPipe()
	// H1: env (incl. injected secret values) is delivered on stdin, never argv.
	// Attach the stream only when there is one so a no-injection body still sees
	// EOF on stdin exactly as before.
	//
	// Written through an explicit StdinPipe rather than session.Stdin, and its
	// write error DELIBERATELY DISCARDED. With session.Stdin, x/crypto/ssh adds a
	// copyFunc whose error Wait() returns whenever the exit status itself was clean:
	//
	//	waitErr := <-s.exitStatus            // nil on a clean exit
	//	for range s.copyFuncs { … copyError … }
	//	if waitErr != nil { return waitErr }
	//	return copyError                     // ← a successful run, reported as an error
	//
	// So if the remote closed the session while we were still writing stdin, the
	// copy failed with io.EOF, which is not an *ssh.ExitError — the run scored -1,
	// emitted "amadeus: EOF" and was marked FAILED even though the command exited 0.
	//
	// That is reachable in production, not just in tests. Since H1 the interpreter
	// is invoked in a form that reads its PROGRAM from stdin (`bash -s`, `python3 -`,
	// `perl`, `powershell -Command -`), and bash reads that program INCREMENTALLY —
	// a script with an early `exit 0` leaves the tail unread, and on a body large
	// enough to still be in flight the channel closes mid-write. Go's own os/exec
	// ignores exactly this class of error for exactly this reason (skipStdinCopyError,
	// issue 9173).
	//
	// A non-zero exit still surfaces normally: Wait() returns waitErr in preference
	// to any copy error. This discards only the case where the command succeeded by
	// its own account.
	if cmd.Stdin != "" {
		stdinPipe, perr := session.StdinPipe()
		if perr != nil {
			emit(t.Name, "amadeus: stdin: "+perr.Error())
			return hostResult{host: t.Name, exitCode: -1, err: perr}
		}
		go func() {
			defer stdinPipe.Close()
			_, _ = io.WriteString(stdinPipe, cmd.Stdin)
		}()
	}
	var streamWG sync.WaitGroup
	streamWG.Add(2)
	go func() { defer streamWG.Done(); scanLines(stdout, func(l string) { emit(t.Name, l) }) }()
	go func() { defer streamWG.Done(); scanLines(stderr, func(l string) { emit(t.Name, l) }) }()

	runErr := session.Run(cmd.Cmd)
	streamWG.Wait()

	exit := 0
	if runErr != nil {
		if ee, ok := runErr.(*ssh.ExitError); ok {
			exit = ee.ExitStatus()
		} else {
			exit = -1
			emit(t.Name, "amadeus: "+runErr.Error())
		}
	}
	return hostResult{host: t.Name, exitCode: exit, err: runErr}
}

// dial opens an SSH client to the target — directly, or jumped through its
// bastion (ProxyJump-style via Via). The returned closer tears down both hops.
// The bastion's address/user come from the manifest target's Via (a bastion
// host the agent can reach); the agent uses its OWN key for both hops.
func (r *sshRunner) dial(ctx context.Context, t runnerproto.ManifestTarget,
	signer ssh.Signer) (*ssh.Client, func(), error) {

	user := t.User
	if user == "" {
		user = "root"
	}
	hostCB, err := r.hostKeyCallback()
	if err != nil {
		return nil, nil, err
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostCB,
		Timeout:         sshDialTimeout,
	}

	if t.Via == "" {
		client, err := dialContext(ctx, dialAddr(t.Address, t.Name, t.Port), cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("dial %s: %w", t.Name, err)
		}
		return client, func() { client.Close() }, nil
	}

	// Bastion hop: Via is the bastion's dial address (host or host:port). The
	// agent reaches the bastion directly, then tunnels to the target through it.
	bAddr := t.Via
	if _, _, err := net.SplitHostPort(bAddr); err != nil {
		bAddr = net.JoinHostPort(bAddr, "22")
	}
	bCfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostCB,
		Timeout:         sshDialTimeout,
	}
	bClient, err := dialContext(ctx, bAddr, bCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("dial bastion %s: %w", t.Via, err)
	}
	targetAddr := dialAddr(t.Address, t.Name, t.Port)
	conn, err := bClient.DialContext(ctx, "tcp", targetAddr)
	if err != nil {
		bClient.Close()
		return nil, nil, fmt.Errorf("bastion %s → %s: %w", t.Via, t.Name, err)
	}
	ncc, chans, reqs, err := ssh.NewClientConn(conn, targetAddr, cfg)
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

// dialAddr builds host:port, defaulting address→name and port→22.
func dialAddr(address, name string, port int) string {
	host := address
	if host == "" {
		host = name
	}
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}

// aggregateExit folds per-host results: all success → 0; any failure → nonzero.
func aggregateExit(results []hostResult) int {
	for _, r := range results {
		if r.exitCode != 0 || r.err != nil {
			if r.exitCode > 0 {
				return r.exitCode
			}
			return 1
		}
	}
	return 0
}
