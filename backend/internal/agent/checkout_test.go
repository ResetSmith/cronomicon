package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// initTestRepo creates a local git repo with a small ansible project (entry +
// roles/ + a .j2 template) and returns its path and the HEAD commit SHA. Skips
// if git is unavailable.
func initTestRepo(t *testing.T) (repoPath, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("site.yml", "- hosts: all\n  roles: [web]\n")
	write("roles/web/tasks/main.yml", "- debug: msg=hi\n")
	write("templates/nginx.conf.j2", "listen {{ port }};\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse: %v", err)
	}
	return dir, strings.TrimSpace(string(out))
}

func TestMaterializeCheckoutRefusals(t *testing.T) {
	const goodSHA = "0123456789abcdef0123456789abcdef01234567" // 40-hex, well-formed
	cases := []struct {
		name    string
		cfg     Config
		sha     string
		repo    string
		entry   string
		wantMsg string
	}{
		{"no allow-checkout flag", Config{AllowCheckout: false, CheckoutRepos: []string{"r"}}, goodSHA, "r", "site.yml", "-allow-checkout"},
		{"repo not in allowlist", Config{AllowCheckout: true, CheckoutRepos: []string{"other"}}, goodSHA, "r", "site.yml", "allowlist"},
		{"sha is a ref not a full sha", Config{AllowCheckout: true, CheckoutRepos: []string{"r"}}, "main", "r", "site.yml", "not a full commit SHA"},
		{"missing entry", Config{AllowCheckout: true, CheckoutRepos: []string{"r"}}, goodSHA, "r", "", "no entry playbook"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &runnerproto.ManifestResponse{
				RunType:  "ansible",
				Checkout: &runnerproto.ManifestCheckout{Repo: tc.repo, SHA: tc.sha, Entry: tc.entry},
			}
			err := materializeCheckout(context.Background(), m, tc.cfg, t.TempDir(), func(string) {})
			if err == nil {
				t.Fatalf("expected refusal, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

func TestMaterializeCheckoutShaNotFound(t *testing.T) {
	repo, _ := initTestRepo(t)
	// A well-formed SHA that does not exist in the repo.
	missing := "ffffffffffffffffffffffffffffffffffffffff"
	m := &runnerproto.ManifestResponse{
		RunType:  "ansible",
		Checkout: &runnerproto.ManifestCheckout{Repo: repo, SHA: missing, Entry: "site.yml"},
	}
	cfg := Config{AllowCheckout: true, CheckoutRepos: []string{repo}, StateDir: t.TempDir()}
	err := materializeCheckout(context.Background(), m, cfg, t.TempDir(), func(string) {})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected 'not found' for a missing commit, got: %v", err)
	}
}

func TestRunLocalToolchainCheckoutHappyPath(t *testing.T) {
	repo, sha := initTestRepo(t)
	stateDir := t.TempDir()
	// The fake ansible-playbook asserts the archived tree is the cwd, then echoes
	// its args so the test can confirm the entry (not /dev/stdin) was passed.
	emit, lines := fakeToolchain(t, `
if [ ! -f site.yml ]; then echo MISSING-SITE; fi
if [ ! -d roles ]; then echo MISSING-ROLES; fi
if [ ! -f templates/nginx.conf.j2 ]; then echo MISSING-TEMPLATE; fi
echo "ARGS: $@"
`)

	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		Body:           "- hosts: all\n", // display parity; must be IGNORED in checkout mode
		EnvPassthrough: []string{},       // scoped env engaged
		Checkout:       &runnerproto.ManifestCheckout{Repo: repo, SHA: sha, Entry: "site.yml"},
	}
	cfg := Config{AllowCheckout: true, CheckoutRepos: []string{repo}, StateDir: stateDir}
	code := runLocalToolchain(context.Background(), m, cfg, emit)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; lines:\n%s", code, strings.Join(lines(), "\n"))
	}
	out := strings.Join(lines(), "\n")
	for _, missing := range []string{"MISSING-SITE", "MISSING-ROLES", "MISSING-TEMPLATE"} {
		if strings.Contains(out, missing) {
			t.Errorf("archived tree incomplete (%s):\n%s", missing, out)
		}
	}
	if !strings.Contains(out, "ARGS: site.yml") {
		t.Errorf("entry playbook not passed to ansible-playbook (want the entry, not /dev/stdin):\n%s", out)
	}
	if !strings.Contains(out, "checkout verified") || !strings.Contains(out, sha) {
		t.Errorf("expected a provenance line naming the verified SHA:\n%s", out)
	}
	assertNoRunDirs(t, stateDir) // per-run workdir torn down after success
}

func TestRunLocalToolchainCheckoutRefusedNoFlag(t *testing.T) {
	repo, sha := initTestRepo(t)
	stateDir := t.TempDir()
	var mu sync.Mutex
	var got []string
	emit := func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() }
	m := &runnerproto.ManifestResponse{
		RunType:  "ansible",
		Checkout: &runnerproto.ManifestCheckout{Repo: repo, SHA: sha, Entry: "site.yml"},
	}
	// AllowCheckout defaults false ⇒ end-to-end refusal, non-zero exit.
	code := runLocalToolchain(context.Background(), m, Config{StateDir: stateDir}, emit)
	if code != -1 {
		t.Fatalf("exit = %d, want -1 (refusal)", code)
	}
	if !strings.Contains(strings.Join(got, "\n"), "-allow-checkout") {
		t.Errorf("refusal should name -allow-checkout: %v", got)
	}
	assertNoRunDirs(t, stateDir)
}

// TestEnsureMirrorKeepsTokenOutOfArgv (SU-3): the deploy token must reach git via
// GIT_ASKPASS (delivered in the git child's environment), never as a URL argv
// element (/proc/cmdline + auditd). A fake `git` on PATH records its argv and,
// separately, whatever it retrieves from $GIT_ASKPASS — proving the secret is
// deliverable yet absent from argv.
func TestEnsureMirrorKeepsTokenOutOfArgv(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	askpassLog := filepath.Join(dir, "askpass.log")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake git: record argv; if GIT_ASKPASS is set, invoke it and record what it
	// prints. Always exit 0 so ensureMirror's clone path completes.
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + argvLog + "\"\n" +
		"if [ -n \"$GIT_ASKPASS\" ]; then \"$GIT_ASKPASS\" \"Password for 'https://oauth2@host':\" >> \"" + askpassLog + "\"; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const token = "sup3r-s3cr3t-token"
	mirrorPath := filepath.Join(dir, "mirror.git") // does not exist → clone path
	if err := ensureMirror(context.Background(), mirrorPath, "https://gitlab.example.com/ops/repo.git", token); err != nil {
		t.Fatalf("ensureMirror: %v", err)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	if strings.Contains(string(argv), token) {
		t.Errorf("deploy token leaked into git argv:\n%s", argv)
	}
	if strings.Contains(string(argv), "oauth2:"+token) {
		t.Errorf("credential leaked into git argv in oauth2:<token> form:\n%s", argv)
	}
	// The clone URL carries the non-secret username only.
	if !strings.Contains(string(argv), "https://oauth2@gitlab.example.com/ops/repo.git") {
		t.Errorf("expected a username-only clone URL in argv:\n%s", argv)
	}
	// The password IS retrievable via the askpass helper — the plumbing works.
	pass, err := os.ReadFile(askpassLog)
	if err != nil || !strings.Contains(string(pass), token) {
		t.Errorf("token not delivered via GIT_ASKPASS: err=%v got=%q", err, pass)
	}
}

// TestScrubURLCreds (SU-3): a credential-bearing URL in git stderr is redacted
// before it can persist un-redacted (the runner deploy token is not in the server
// redactor dictionary).
func TestScrubURLCreds(t *testing.T) {
	cases := map[string]string{
		"fatal: unable to access 'https://oauth2:secret@host/repo.git/': 403": "fatal: unable to access 'https://[REDACTED]@host/repo.git/': 403",
		"https://user:pass@gitlab.example.com/x":                              "https://[REDACTED]@gitlab.example.com/x",
		"no url here":                                                         "no url here",
		"https://host/no-userinfo":                                            "https://host/no-userinfo",
		"cloned https://u:p@ss@host/r.git":                                    "cloned https://[REDACTED]@host/r.git",
	}
	for in, want := range cases {
		if got := scrubURLCreds(in); got != want {
			t.Errorf("scrubURLCreds(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitCheckoutCredential (SU-3): deploy-token vs PAT credential forms.
func TestSplitCheckoutCredential(t *testing.T) {
	if u, p := splitCheckoutCredential("gitlab+deploy-token-1:abc123"); u != "gitlab+deploy-token-1" || p != "abc123" {
		t.Errorf("deploy-token split = %q/%q, want gitlab+deploy-token-1/abc123", u, p)
	}
	if u, p := splitCheckoutCredential("glpat-xxxx"); u != "oauth2" || p != "glpat-xxxx" {
		t.Errorf("PAT split = %q/%q, want oauth2/glpat-xxxx", u, p)
	}
}
