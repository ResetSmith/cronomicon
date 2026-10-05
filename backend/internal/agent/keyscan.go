package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
)

// Host-key scan & approve — agent side (runner-install-update-2.md Phase 5).
//
// On a "keyscan" control op the agent dials each requested host from its OWN
// vantage, captures the presented host key WITHOUT verifying (like
// ssh-keyscan, but native — x/crypto/ssh, no external binary), and uploads what
// it saw for operator approval. On a "trust-hosts" op it appends the approved
// known_hosts lines to its trust store. Human-approved TOFU: strictly better
// than an operator pasting ssh-keyscan output blind, and auditable.
//
// Protocol 14 adds the other half. "untrust-hosts" removes lines the server no
// longer wants trusted (a key that was replaced, or removed by an operator),
// and "known-hosts-report" uploads what the file holds — each line's hosts, key
// type and fingerprint, never the file itself — so the server can show the
// file's keys beside the ones it approved, and tell whether a delivery landed.

// scannedHostKey is one host's captured key, uploaded for approval. JSON tags
// match the server's upload contract (keyscan.go: scannedHostKey).
type scannedHostKey struct {
	Host           string `json:"host"`
	KeyType        string `json:"keyType"`
	Fingerprint    string `json:"fingerprint"`
	KnownHostsLine string `json:"knownHostsLine"`
}

// keyscanParallel bounds how many hosts are dialled at once. A scope scan can
// name hundreds; an unreachable one costs a full dial timeout.
const keyscanParallel = 8

// handleKeyscan scans the requested hosts and uploads each captured key as soon
// as its host answers. Runs in its own goroutine (dispatched from handleControl)
// so it never blocks the poll loop.
//
// Hosts are dialled concurrently and uploaded one by one on purpose: an
// operator is watching the review screen fill, and a single unreachable host —
// which costs a whole dial timeout — must not hold back the keys of the hosts
// that answered at once. A host that can't be reached is logged and skipped.
func (a *Agent) handleKeyscan(ctx context.Context, hosts []string) {
	var (
		wg       sync.WaitGroup
		uploaded atomic.Int64
		slots    = make(chan struct{}, keyscanParallel)
	)
	for _, h := range hosts {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			k, err := scanHostKey(ctx, h)
			if err != nil {
				a.log.Warn("host-key scan failed", "host", h, "error", err)
				return
			}
			if err := a.client.UploadHostKeys(ctx, a.id, []scannedHostKey{k}); err != nil {
				a.log.Error("upload scanned host key failed", "host", h, "error", err)
				return
			}
			uploaded.Add(1)
		}()
	}
	wg.Wait()
	if n := uploaded.Load(); n == 0 {
		a.log.Warn("host-key scan produced no keys", "requested", hosts)
	} else {
		a.log.Info("uploaded scanned host keys for approval", "count", n, "requested", len(hosts))
	}
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

// handleUntrustHosts removes lines the server no longer wants trusted.
func (a *Agent) handleUntrustHosts(entries []string) {
	path := a.cfg.KnownHostsFile
	if path == "" {
		a.log.Error("untrust-hosts: no known_hosts path configured")
		return
	}
	n, err := removeKnownHosts(path, entries)
	if err != nil {
		a.log.Error("untrust-hosts: rewrite known_hosts failed", "path", path, "error", err)
		return
	}
	a.log.Info("removed host keys from known_hosts", "requested", len(entries), "removed", n, "path", path)
}

// removeKnownHosts deletes from the known_hosts file every line that is exactly
// one of entries: the same single host, the same key, no marker. It returns how
// many lines went.
//
// The match is on the parsed line, not its text, so spacing and a trailing
// comment (of any length — the line is read as the verifier reads it) do not
// hide a line. It is deliberately narrow in the other direction:
// a line naming SEVERAL hosts is left alone even if it carries the key, because
// removing it would untrust hosts nobody asked about. Such a line was not
// written by this agent (it appends one host per line); it stays, and the next
// report shows it as present.
//
// The file is replaced by rename so a run dialling at this moment reads either
// the old file or the new one, never a half-written one. Where the file is a
// single-file mount a rename cannot replace it, and it is rewritten in place.
func removeKnownHosts(path string, entries []string) (int, error) {
	type target struct {
		host string
		key  []byte
	}
	var targets []target
	for _, e := range entries {
		l, err := knownhostsline.Parse(e)
		if err != nil || l.Marker != "" || len(l.Hosts) != 1 {
			continue
		}
		targets = append(targets, target{l.Hosts[0], l.Key.Marshal()})
	}
	if len(targets) == 0 {
		return 0, nil
	}
	// A known_hosts that is a symlink is rewritten where it points: renaming
	// over the link itself would replace it with a regular file and silently
	// detach the runner from the file the link was there to share.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var kept []string
	removed := 0
	for l := range strings.SplitSeq(strings.TrimRight(string(existing), "\n"), "\n") {
		drop := false
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			if parsed, perr := knownhostsline.Parse(t); perr == nil && parsed.Marker == "" && len(parsed.Hosts) == 1 {
				for _, tg := range targets {
					if parsed.Hosts[0] == tg.host && bytes.Equal(parsed.Key.Marshal(), tg.key) {
						drop = true
						break
					}
				}
			}
		}
		if drop {
			removed++
			continue
		}
		kept = append(kept, l)
	}
	if removed == 0 {
		return 0, nil
	}
	out := []byte(strings.Join(kept, "\n"))
	if len(kept) > 0 {
		out = append(out, '\n')
	}
	mode := os.FileMode(0o640)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".known_hosts-*")
	if err == nil {
		tmpName := tmp.Name()
		_, werr := tmp.Write(out)
		if cerr := tmp.Close(); werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Chmod(tmpName, mode)
		}
		if werr == nil {
			werr = os.Rename(tmpName, path)
		}
		if werr == nil {
			return removed, nil
		}
		_ = os.Remove(tmpName)
	}
	if err := os.WriteFile(path, out, mode); err != nil {
		return 0, err
	}
	return removed, nil
}

// knownHostsEntry is one line of the known_hosts file as reported to the
// server. JSON tags match the server's upload contract (hostkeys.go).
type knownHostsEntry struct {
	Line        int    `json:"line"`
	Hosts       string `json:"hosts"`
	Hashed      bool   `json:"hashed"`
	Marker      string `json:"marker"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
}

// maxKnownHostsReport caps one report (the server applies the same cap); a
// longer file is reported truncated, and says so.
const maxKnownHostsReport = 5000

// readKnownHosts parses the known_hosts file into report entries, reading each
// line as the verifier reads it (knownhostsline) so that every line a run would
// trust is a line that is reported. A line even that parser refuses is skipped:
// the verifier refuses it too, and with it the whole file, which the runs
// themselves report. A hashed host is reported as hashed with no name: the hash
// is not sent.
func readKnownHosts(path string) (entries []knownHostsEntry, truncated bool, err error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for n := 1; sc.Scan(); n++ {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		l, perr := knownhostsline.Parse(t)
		if perr != nil {
			continue
		}
		if len(entries) == maxKnownHostsReport {
			return entries, true, nil
		}
		e := knownHostsEntry{Line: n, Marker: l.Marker, KeyType: l.Key.Type(), Fingerprint: ssh.FingerprintSHA256(l.Key)}
		if knownhostsline.IsHashed(l.Hosts[0]) {
			e.Hashed = true
		} else if e.Hosts = strings.Join(l.Hosts, ","); len(e.Hosts) > 1024 {
			e.Hosts = e.Hosts[:1024]
		}
		entries = append(entries, e)
	}
	return entries, false, sc.Err()
}

// reportKnownHosts uploads the current contents of the known_hosts file. Called
// on a "known-hosts-report" op (which the server sends after every change it
// makes, and when an operator asks) and once at startup, so a file seeded by
// hand is visible without anyone asking.
func (a *Agent) reportKnownHosts(ctx context.Context) {
	path := a.cfg.KnownHostsFile
	if path == "" {
		return
	}
	entries, truncated, err := readKnownHosts(path)
	if err != nil {
		a.log.Error("known-hosts report: read failed", "path", path, "error", err)
		return
	}
	if err := a.client.UploadKnownHosts(ctx, a.id, entries, truncated); err != nil {
		a.log.Error("known-hosts report: upload failed", "error", err)
		return
	}
	a.log.Info("reported known_hosts entries", "count", len(entries), "truncated", truncated)
}
