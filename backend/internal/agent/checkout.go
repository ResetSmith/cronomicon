package agent

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// materializeCheckout implements runner-side pinned checkout (ansible-update.md
// RX.1/RX.2/RX.3): it enforces the opt-in + allowlist refusal gates, fetches the
// repo into a persistent bare mirror using the runner's OWN deploy credential
// (D1 — never shipped), verifies the pinned commit exists, and archives that
// commit's tree into workdir (no .git — a fresh, unpoisonable per-run tree). It
// emits a provenance line on success (RX.12). Any failure is loud and aborts the
// run — there is no fallback to an unpinned tree.
func materializeCheckout(ctx context.Context, m *runnerproto.ManifestResponse, cfg Config, workdir string, emit func(string)) error {
	c := m.Checkout

	// Gate 1 — opt-in (RX.6). An un-flagged runner refuses checkout entirely,
	// same defense-in-depth shape as the local-mode inventory refusal.
	if !cfg.AllowCheckout {
		return fmt.Errorf("refusing checkout run: this runner is not started with -allow-checkout")
	}
	// Gate 2 — repo allowlist (RX.6). A server compromise cannot point the runner
	// at an arbitrary repo.
	if !containsStr(cfg.CheckoutRepos, c.Repo) {
		return fmt.Errorf("refusing checkout run: repo %q is not in this runner's -checkout-repos allowlist", c.Repo)
	}
	// Gate 3 — the pin must be a full commit SHA, never a ref (RX.2).
	if !isFullCommitSHA(c.SHA) {
		return fmt.Errorf("refusing checkout run: %q is not a full commit SHA", c.SHA)
	}
	if strings.TrimSpace(c.Entry) == "" {
		return fmt.Errorf("refusing checkout run: manifest carries no entry playbook")
	}

	mirrorPath := filepath.Join(mirrorRoot(cfg), mirrorName(c.Repo))
	token := resolveCheckoutToken(cfg)
	if err := ensureMirror(ctx, mirrorPath, c.Repo, token); err != nil {
		return fmt.Errorf("checkout mirror fetch failed: %w", err)
	}
	// Verify the pinned commit is present after fetch (RX.2). A missing commit
	// fails loudly — never silently run a different tree.
	if err := gitCommitExists(ctx, mirrorPath, c.SHA); err != nil {
		return fmt.Errorf("pinned commit %s not found in %s after fetch: %w", c.SHA, c.Repo, err)
	}
	// Archive the tree at the pinned SHA into the fresh workdir (no .git — RX.3).
	if err := gitArchiveInto(ctx, mirrorPath, c.SHA, workdir); err != nil {
		return fmt.Errorf("checkout archive of %s failed: %w", c.SHA, err)
	}
	// The entry playbook must exist in the materialized tree.
	entryAbs := filepath.Join(workdir, filepath.FromSlash(c.Entry))
	if st, serr := os.Stat(entryAbs); serr != nil || st.IsDir() {
		return fmt.Errorf("checkout entry %q not found in the archived tree at %s", c.Entry, c.SHA)
	}

	// Provenance (RX.12): the verified SHA buys back the auditability run-time
	// materialization costs. (Dependency provenance lands with Phase 3.)
	emit(fmt.Sprintf("cronomicon: checkout verified — repo=%s sha=%s entry=%s", c.Repo, c.SHA, c.Entry))
	return nil
}

// mirrorRoot is where persistent bare mirrors live (RX.3): -mirror-dir, else
// <state-dir>/mirrors, else <tmp>/cronomicon-mirrors.
func mirrorRoot(cfg Config) string {
	if cfg.MirrorDir != "" {
		return cfg.MirrorDir
	}
	base := cfg.StateDir
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "cronomicon-mirrors")
}

// mirrorName maps a repo identity to a filesystem-safe mirror directory name.
// Non-alphanumeric runs collapse to '-'; a short suffix keeps distinct URLs that
// sanitize identically from colliding.
func mirrorName(repo string) string {
	var b strings.Builder
	for _, r := range repo {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "repo"
	}
	return name + "-" + shortHash(repo) + ".git"
}

// resolveCheckoutToken returns the deploy credential: the trimmed contents of
// CheckoutTokenFile if set (provisioned like secrets.env), else CheckoutToken.
func resolveCheckoutToken(cfg Config) string {
	if cfg.CheckoutTokenFile != "" {
		if data, err := os.ReadFile(cfg.CheckoutTokenFile); err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return cfg.CheckoutToken
}

// ensureMirror creates (clone --mirror) or refreshes (fetch --prune) a bare
// mirror of repo at mirrorPath. The deploy credential is delivered to git
// out-of-band via GIT_ASKPASS — the password comes from the askpass helper's OWN
// environment (SU-3), so the SECRET never appears in git's argv
// (/proc/<pid>/cmdline + auditd execve) and is never persisted into the mirror's
// remote config. Only the NON-secret username rides in the fetch/clone URL; a git
// error's stderr is scrubbed of any `://user:pass@` before it surfaces.
func ensureMirror(ctx context.Context, mirrorPath, repo, token string) error {
	fetchURL := repo
	runEnv := gitEnv()
	cleanup := func() {}
	defer func() { cleanup() }()
	if token != "" && isHTTPURL(repo) {
		user, pass := splitCheckoutCredential(token)
		askpass, cl, err := writeAskpassHelper()
		if err != nil {
			return fmt.Errorf("checkout askpass helper: %w", err)
		}
		cleanup = cl
		fetchURL = repoURLWithUser(repo, user) // username only — password via GIT_ASKPASS
		runEnv = gitAuthEnv(askpass, pass)
	}

	if isGitMirror(mirrorPath) {
		// Refresh from the URL directly, not the stored remote, so the persisted
		// origin URL stays credential-free.
		return runGitEnv(ctx, runEnv, "", "-C", mirrorPath, "fetch", "--prune", fetchURL, "+refs/*:refs/*")
	}
	if err := os.MkdirAll(filepath.Dir(mirrorPath), 0o700); err != nil {
		return err
	}
	if err := runGitEnv(ctx, runEnv, "", "clone", "--mirror", fetchURL, mirrorPath); err != nil {
		return err
	}
	// Scrub any credential from the persisted remote URL.
	_ = runGit(ctx, "", "-C", mirrorPath, "remote", "set-url", "origin", repo)
	return nil
}

// isHTTPURL reports whether s is an http(s) URL (the only form that carries an
// in-URL credential; ssh/local paths never do).
func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}

// splitCheckoutCredential splits the deploy credential into the (non-secret)
// username and the secret password. A token containing ':' is the GitLab
// deploy-token form user:pass; a bare token is a PAT / project access token, which
// uses the fixed username "oauth2".
func splitCheckoutCredential(token string) (user, pass string) {
	if before, after, ok := strings.Cut(token, ":"); ok {
		return before, after
	}
	return "oauth2", token
}

// repoURLWithUser sets the (non-secret) username on an http(s) repo URL and
// leaves the password empty — git obtains the password from GIT_ASKPASS, so the
// secret never enters the URL argv. A non-parseable URL is returned unchanged.
func repoURLWithUser(repo, user string) string {
	u, err := url.Parse(repo)
	if err != nil {
		return repo
	}
	u.User = url.User(user)
	return u.String()
}

// writeAskpassHelper writes a tiny GIT_ASKPASS helper to a private temp file. The
// helper prints the password from its own environment (CRONOMICON_GIT_ASKPASS_PASS,
// set on the git child and inherited here), so the secret is delivered via the
// environment — /proc/<pid>/environ is mode 0400 and execve does not log environ,
// unlike argv. The returned cleanup removes the file.
func writeAskpassHelper() (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "cronomicon-askpass-*.sh")
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	const script = "#!/bin/sh\nprintf '%s' \"$CRONOMICON_GIT_ASKPASS_PASS\"\n"
	if _, werr := f.WriteString(script); werr != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", func() {}, werr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return "", func() {}, cerr
	}
	if cherr := os.Chmod(name, 0o700); cherr != nil {
		_ = os.Remove(name)
		return "", func() {}, cherr
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// isGitMirror reports whether mirrorPath looks like an existing bare mirror.
func isGitMirror(mirrorPath string) bool {
	if _, err := os.Stat(filepath.Join(mirrorPath, "HEAD")); err == nil {
		return true
	}
	return false
}

// gitCommitExists returns nil iff sha resolves to a commit object in the mirror.
func gitCommitExists(ctx context.Context, mirrorPath, sha string) error {
	return runGit(ctx, "", "-C", mirrorPath, "cat-file", "-e", sha+"^{commit}")
}

// gitArchiveInto streams `git archive <sha>` from the mirror and extracts the
// tar into workdir with a path-traversal guard (RX.3: fresh tree, no .git).
func gitArchiveInto(ctx context.Context, mirrorPath, sha, workdir string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", mirrorPath, "archive", "--format=tar", sha)
	cmd.Env = gitEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractTar(stdout, workdir)
	// Drain any remaining stdout so the child never blocks on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if waitErr != nil {
		return fmt.Errorf("git archive: %v: %s", waitErr, scrubURLCreds(strings.TrimSpace(stderr.String())))
	}
	return extractErr
}

// extractTar writes tar entries under dest, rejecting any entry whose path
// escapes dest (defense-in-depth against a malicious tree). Regular files and
// directories are created; symlinks are skipped (a checkout tree needs none for
// ansible, and an escaping symlink is a traversal vector).
func extractTar(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean(hdr.Name)
		if clean == "." {
			continue
		}
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("archive entry %q escapes the workdir", hdr.Name)
		}
		target := filepath.Join(dest, clean)
		rel, err := filepath.Rel(dest, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("archive entry %q escapes the workdir", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil { //nolint:gosec // pinned, trusted tree (P1)
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks/devices/etc. skipped — see doc comment.
		}
	}
}

// runGit runs a git subcommand with the default (credential-free) environment.
// dir is unused (dir is passed via -C) but kept for signature clarity.
func runGit(ctx context.Context, dir string, args ...string) error {
	return runGitEnv(ctx, gitEnv(), dir, args...)
}

// runGitEnv runs a git subcommand with an explicit environment (so the two
// authenticated calls can carry the GIT_ASKPASS helper + token). Any stderr is
// scrubbed of `://user:pass@` before it enters the returned error — the runner's
// deploy token is a runner-local credential the SERVER redactor never sees, so a
// credential-bearing URL echoed by git would otherwise persist un-redacted (SU-3).
func runGitEnv(ctx context.Context, env []string, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, scrubURLCreds(strings.TrimSpace(stderr.String())))
	}
	return nil
}

// gitEnv returns a non-interactive git environment: never prompt for
// credentials (fail fast instead of hanging), and disable any askpass GUI.
func gitEnv() []string {
	return gitEnvAskpass("")
}

// gitEnvAskpass builds the non-interactive git env with a given GIT_ASKPASS
// program (empty = none), plus any extra KEY=VALUE entries (e.g. the token the
// askpass helper reads).
func gitEnvAskpass(askpass string, extra ...string) []string {
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS="+askpass,
		"GCM_INTERACTIVE=never",
	)
	return append(env, extra...)
}

// gitAuthEnv is the git env for a credential-bearing call: GIT_ASKPASS points at
// the helper, and the password is delivered via CRONOMICON_GIT_ASKPASS_PASS (env,
// never argv).
func gitAuthEnv(askpass, password string) []string {
	return gitEnvAskpass(askpass, "CRONOMICON_GIT_ASKPASS_PASS="+password)
}

// urlCredRe matches a `scheme://userinfo@` prefix so credential-bearing URLs in
// git stderr can be scrubbed to `scheme://[REDACTED]@` (SU-3 defence-in-depth).
// The userinfo class allows '@' and backtracks to the LAST '@' before a path or
// whitespace, so a password containing an unescaped '@' is fully redacted.
var urlCredRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/\s]+@`)

// scrubURLCreds redacts any `://user:pass@` userinfo from s.
func scrubURLCreds(s string) string {
	return urlCredRe.ReplaceAllString(s, "${1}[REDACTED]@")
}

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	return slices.Contains(list, s)
}

// shortHash is a tiny FNV-1a hex digest used to disambiguate mirror dir names.
func shortHash(s string) string {
	const off, prime = uint32(2166136261), uint32(16777619)
	h := off
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return fmt.Sprintf("%08x", h)
}

// isFullCommitSHA reports whether s is a full git commit SHA (40 hex for sha1,
// 64 for sha256) — never a ref (RX.2). Mirrors the server-side check.
func isFullCommitSHA(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
