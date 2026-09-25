package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"golang.org/x/crypto/ssh"
)

// CheckStatus is the outcome of a single doctor check.
type CheckStatus string

const (
	CheckPass CheckStatus = "PASS"
	CheckWarn CheckStatus = "WARN"
	CheckFail CheckStatus = "FAIL"
)

// Check is one preflight result. Detail is a human-facing, actionable message.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

// Doctor runs environment preflight checks and returns the results plus whether
// all passed (no FAIL). Every probe is bounded, so it is safe to run as a
// systemd ExecStartPre INSIDE the unit sandbox: a broken environment (hung
// $PATH mount, unreachable/proxied server, a sandbox that stalls filesystem
// access) yields a labeled FAIL rather than a hang — the diagnosis this whole
// class of "installed but never registers" incident was missing.
//
// quick skips the two slowest checks (toolchain detection and the systemd-run
// probe) so an ExecStartPre doesn't add tens of seconds to every boot; the
// installer runs the full set once at install time.
func Doctor(ctx context.Context, cfg Config, quick bool) ([]Check, bool) {
	// Observe the same defaulted trust store (known_hosts beside the identity) a
	// live agent would, so the known-hosts check reports the real path.
	cfg = defaultedConfig(cfg)
	var checks []Check
	add := func(name string, status CheckStatus, detail string) {
		checks = append(checks, Check{Name: name, Status: status, Detail: detail})
	}

	// Server URL scheme (Resolve already normalized it; report for visibility).
	if u, err := url.Parse(cfg.ServerURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("server-url", CheckFail, fmt.Sprintf("%q is not a valid http(s):// URL with a host", cfg.ServerURL))
	} else {
		add("server-url", CheckPass, cfg.ServerURL)
	}

	// Server reachability + SSO-proxy-intercept detection.
	if client, err := NewClient(cfg.ServerURL, cfg.CACertPath); err != nil {
		add("server-reachable", CheckFail, "client init: "+err.Error())
	} else {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := client.CheckHealth(hctx); err != nil {
			add("server-reachable", CheckFail, err.Error())
		} else {
			add("server-reachable", CheckPass, cfg.ServerURL+"/healthz OK")
		}
		cancel()
	}

	// $PATH directories responsive. Catches a dir on a hung mount AND — when run
	// as an in-sandbox ExecStartPre — a systemd sandbox that stalls filesystem
	// access (the failure that took days to pin down).
	if hung := hungPathDirs(ctx); len(hung) > 0 {
		add("path-dirs", CheckFail, "stat timed out on $PATH dir(s) — hung mount, or a systemd sandbox stalling filesystem access: "+strings.Join(hung, ", "))
	} else {
		add("path-dirs", CheckPass, "all $PATH directories responsive")
	}

	// Identity file directory writable (persistence of the minted key).
	idDir := filepath.Dir(cfg.IdentityFile)
	if err := checkWritableDir(idDir); err != nil {
		add("identity-writable", CheckFail, err.Error())
	} else {
		add("identity-writable", CheckPass, idDir)
	}

	// Registration readiness: a resumed identity needs no token; a first
	// registration needs a present (single-use, 24h) token.
	if id, _ := loadIdentity(cfg.IdentityFile); id != nil {
		add("registration", CheckPass, "resuming identity "+id.ID)
	} else if cfg.RegistrationToken == "" {
		add("registration", CheckFail, "no identity yet and no registration token — set CRONOMICON_RUNNER_REGISTRATION_TOKEN (single-use, expires 24h; mint via Add Runner)")
	} else {
		add("registration", CheckPass, "registration token present (single-use, expires 24h)")
	}

	if !quick {
		// Toolchains — the slow one (bounded probes may each hit their deadline
		// on a broken host). detectCapabilities returns a clear error when it
		// finds nothing.
		if caps, _, err := detectCapabilities(ctx, cfg); err != nil {
			add("toolchains", CheckFail, err.Error())
		} else {
			add("toolchains", CheckPass, "detected: "+strings.Join(caps, ", "))
		}

		// Tier-2 sandbox is informational: unsandboxed is a supported mode.
		if cfg.NoSandbox {
			add("sandbox", CheckWarn, "disabled (NoSandbox)")
		} else if probeSandbox(ctx) {
			add("sandbox", CheckPass, "usable systemd-run scope")
		} else {
			add("sandbox", CheckWarn, "no usable systemd-run scope — runs execute unsandboxed")
		}

		// Key custody (Phase 2): enumerate the NAMES the agent can resolve to a
		// local key FILE (key-map + key-dir) and parse each to surface a
		// passphrase-protected or unreadable key BEFORE a run needs it. Names,
		// paths, and parse status only — never key material.
		names, warns := inspectKeys(cfg)
		okPart := ""
		if len(names) > 0 {
			okPart = "resolvable: " + strings.Join(names, ", ")
		}
		switch {
		case len(warns) > 0:
			// Still list the healthy names so one bad key doesn't hide the good ones.
			detail := strings.Join(warns, "; ")
			if okPart != "" {
				detail = okPart + "; " + detail
			}
			add("key-names", CheckWarn, detail)
		case len(names) == 0:
			add("key-names", CheckWarn, "no resolvable key files (key-map empty, key-dir empty/unset) — SSH & ansible runs fail until keys are provisioned (installer --generate-key, or drop files in the key-dir)")
		default:
			add("key-names", CheckPass, okPart)
		}

		// known_hosts is the trust store ansible now shares (Phase 1). Report its
		// path + entry count so an empty/stale store is visible before a dial.
		if cfg.KnownHostsFile == "" {
			add("known-hosts", CheckWarn, "no known_hosts configured — the agent refuses SSH without one")
		} else if n, err := countKnownHostsEntries(cfg.KnownHostsFile); err != nil {
			add("known-hosts", CheckWarn, cfg.KnownHostsFile+": "+err.Error())
		} else if n == 0 {
			add("known-hosts", CheckWarn, cfg.KnownHostsFile+" is empty — Scan & approve the targets, or ssh-keyscan them in, before the first run")
		} else {
			add("known-hosts", CheckPass, fmt.Sprintf("%s (%d host key(s))", cfg.KnownHostsFile, n))
		}
	}

	ok := true
	for _, c := range checks {
		if c.Status == CheckFail {
			ok = false
		}
	}
	return checks, ok
}

// hungPathDirs stats each $PATH directory under a per-dir deadline and returns
// those that don't respond. A fast error (dir simply absent) is NOT hung; only
// a stat that overruns the deadline counts — that is a stale mount or a sandbox
// stalling filesystem access. Bounded, so it never hangs the caller.
func hungPathDirs(ctx context.Context) []string {
	var hung []string
	seen := map[string]bool{}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		done := make(chan struct{}, 1)
		go func(dir string) { _, _ = os.Stat(dir); done <- struct{}{} }(d)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			hung = append(hung, d)
		case <-ctx.Done():
			return hung
		}
	}
	return hung
}

// checkWritableDir verifies dir exists and is writable by creating+removing a
// temp file (the same operation saveIdentity needs).
func checkWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".cronomicon-doctor-*")
	if err != nil {
		return fmt.Errorf("%s not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// inspectKeys enumerates the credential NAMES resolvable to a local key FILE
// (key-map entries first, then key-dir basenames stripped of .pem/.key) and
// parses each. It returns the OK names (sorted, deduped, key-map winning a name
// collision) and WARN messages for keys that are passphrase-protected (the agent
// can't use them non-interactively) or unreadable/not a key. It NEVER logs or
// returns key material — only names, paths, and parse status.
func inspectKeys(cfg Config) (okNames []string, warnings []string) {
	// A candidate key file. declared = the file explicitly announces key intent
	// (a key-map entry, or a .pem/.key suffix). A BARE key-dir filename is only a
	// possible authKeyEnvVar match, so an unparseable one is silently skipped
	// (the dir legitimately holds non-keys — a README, a copied known_hosts); a
	// declared-but-unparseable file, by contrast, is a real WARN.
	type cand struct {
		path     string
		declared bool
	}
	cands := map[string]cand{}
	order := []string{}
	remember := func(name, path string, declared bool) {
		if _, seen := cands[name]; seen {
			return // key-map first, so it wins a collision with a key-dir file.
		}
		cands[name] = cand{path: path, declared: declared}
		order = append(order, name)
	}
	for name, path := range cfg.KeyMap {
		remember(name, path, true)
	}
	if cfg.KeyDir != "" {
		if entries, err := os.ReadDir(cfg.KeyDir); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				fn := e.Name()
				// Public-key siblings (e.g. --generate-key writes NAME.pub next to
				// NAME) are never a private key the agent resolves — skip them so a
				// healthy generated key doesn't trip a false WARN on its own .pub.
				if strings.HasSuffix(fn, ".pub") {
					continue
				}
				declared := strings.HasSuffix(fn, ".pem") || strings.HasSuffix(fn, ".key")
				name := keyNameFromFile(fn)
				remember(name, filepath.Join(cfg.KeyDir, fn), declared)
			}
		}
	}
	sort.Strings(order)
	for _, name := range order {
		c := cands[name]
		pem, err := os.ReadFile(c.path)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s unreadable (%s)", name, c.path))
			continue
		}
		if _, err := ssh.ParsePrivateKey(pem); err != nil {
			var pme *ssh.PassphraseMissingError
			switch {
			case errors.As(err, &pme):
				warnings = append(warnings, fmt.Sprintf("%s is passphrase-protected — the agent can't use it non-interactively; provide an unencrypted key", name))
			case c.declared:
				warnings = append(warnings, fmt.Sprintf("%s is not a usable private key (%s)", name, c.path))
			}
			// A bare (undeclared) non-key file is silently skipped — not every file
			// in the key-dir is a key.
			continue
		}
		okNames = append(okNames, name)
	}
	return okNames, warnings
}

// countKnownHostsEntries counts non-blank, non-comment lines in an OpenSSH
// known_hosts file (a rough host-key entry count for the doctor report).
func countKnownHostsEntries(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
	}
	return n, sc.Err()
}

// DoctorAuth resolves a credential NAME exactly as a run would (key-map → env
// PEM → key-dir) and, when target ("user@host", "host", or "host:port") is
// non-empty, dials it with the agent's own Go-SSH stack — the same known_hosts +
// strict callback a real run uses — classifying the outcome as auth OK /
// host-key-unknown / permission-denied / connect-failed. Returns report Checks +
// overall ok (no FAIL). Never logs key material.
func DoctorAuth(ctx context.Context, cfg Config, name, target string) ([]Check, bool) {
	cfg = defaultedConfig(cfg)
	var checks []Check
	add := func(n string, s CheckStatus, d string) { checks = append(checks, Check{Name: n, Status: s, Detail: d}) }

	if name == "" {
		add("auth-name", CheckFail, "no credential NAME given (usage: doctor --auth NAME [user@host])")
		return checks, false
	}

	// Resolution report — in loadSigner's EXACT precedence (key-map → env PEM →
	// key-dir) so the reported source is the one the signer actually came from,
	// not a different file that also happens to exist.
	r := sshRunnerFor(cfg)
	signer, lerr := r.loadSigner(name)
	mapPath, _, mapOK := resolveKeyPath(cfg.KeyMap, "", name)
	dirPath, _, dirOK := resolveKeyPath(nil, cfg.KeyDir, name)
	switch {
	case mapOK:
		add("auth-resolve", CheckPass, fmt.Sprintf("%s → %s (key-map)", name, mapPath))
	case os.Getenv(name) != "":
		add("auth-resolve", CheckPass, name+" → PEM in agent env (Go-SSH runs only; ansible needs a key-map/key-dir file)")
	case dirOK:
		add("auth-resolve", CheckPass, fmt.Sprintf("%s → %s (key-dir)", name, dirPath))
	default:
		// Nothing resolved: loadSigner's message names every place checked.
		add("auth-resolve", CheckFail, lerr.Error())
		return checks, false
	}
	if lerr != nil {
		// Resolved to a path/PEM but it won't parse (passphrase, corrupt).
		add("auth-key", CheckFail, lerr.Error())
		return checks, false
	}
	add("auth-key", CheckPass, "private key parsed OK")

	if target == "" {
		return checks, true
	}

	// Dial the target with the same stack a run uses.
	user, host, port := splitAuthTarget(target)
	t := runnerproto.ManifestTarget{Name: host, Address: host, Port: port, User: user, AuthKeyEnvVar: name}
	dctx, cancel := context.WithTimeout(ctx, sshDialTimeout+5*time.Second)
	defer cancel()
	_, closeFn, derr := r.dial(dctx, t, signer)
	if derr != nil {
		switch {
		case isHostKeyError(derr):
			add("auth-dial", CheckFail, "host key not in known_hosts (or mismatch) — Scan & approve "+dialAddr(host, host, port)+" in the Runners view, then retry")
		case isAuthError(derr):
			add("auth-dial", CheckFail, "permission denied — the target's authorized_keys for this user does not accept this key: "+derr.Error())
		default:
			add("auth-dial", CheckFail, "connect failed: "+derr.Error())
		}
		return checks, false
	}
	closeFn()
	dispUser := user
	if dispUser == "" {
		dispUser = "root"
	}
	add("auth-dial", CheckPass, fmt.Sprintf("authenticated to %s@%s", dispUser, dialAddr(host, host, port)))
	return checks, true
}

// isAuthError reports whether an SSH dial failure is an authentication rejection
// (key not accepted) rather than a host-key or transport error.
func isAuthError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unable to authenticate") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "no supported methods remain")
}

// splitAuthTarget parses "user@host", "host", or either with a ":port" suffix on
// the host. A missing user yields "" (dial defaults it to root); a missing/bad
// port yields 0 (dial defaults it to 22).
func splitAuthTarget(s string) (user, host string, port int) {
	host = s
	if i := strings.LastIndex(s, "@"); i >= 0 {
		user, host = s[:i], s[i+1:]
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		if n, aerr := strconv.Atoi(p); aerr == nil {
			port = n
		}
	}
	return user, host, port
}
