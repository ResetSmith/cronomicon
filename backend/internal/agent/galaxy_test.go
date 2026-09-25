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

// initTestRepoWithReq builds a local git repo whose project (scripts/proj/)
// carries an entry playbook AND a pinned requirements.yml. Returns repo + SHA.
func initTestRepoWithReq(t *testing.T) (repoPath, sha string) {
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
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("scripts/proj/site.yml", "- hosts: all\n  tasks: []\n")
	write("scripts/proj/requirements.yml", "collections:\n  - name: community.general\n    version: 5.0.0\n")
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

// fakeToolchainAndGalaxy installs fake ansible-playbook (runs playbookScript),
// ansible-galaxy (installs anything, empty base), and ansible (--version) on one
// PATH dir. Returns a collector.
func fakeToolchainAndGalaxy(t *testing.T, playbookScript string) (emit func(string), lines func() []string) {
	t.Helper()
	binDir := t.TempDir()
	files := map[string]string{
		"ansible-playbook": "#!/bin/sh\n" + playbookScript + "\n",
		"ansible-galaxy": `#!/bin/sh
if [ "$1" = "collection" ] && [ "$2" = "list" ]; then echo '{}'; exit 0; fi
if [ "$1" = "collection" ] && [ "$2" = "install" ]; then echo "installed $3"; exit 0; fi
if [ "$1" = "role" ] && [ "$2" = "install" ]; then echo "installed roles"; exit 0; fi
exit 0
`,
		"ansible": "#!/bin/sh\necho 'ansible [core 2.16.3]'\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	var mu sync.Mutex
	var got []string
	emit = func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() }
	lines = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
	return emit, lines
}

// fakeGalaxy installs a fake `ansible-galaxy` (and `ansible`) on PATH. The galaxy
// script: `collection list --format json` prints baseJSON; `collection install`
// / `role install` succeed unless the requested name is in failNames. Returns a
// collector.
func fakeGalaxy(t *testing.T, baseJSON string, failNames ...string) (emit func(string), lines func() []string) {
	t.Helper()
	binDir := t.TempDir()
	fail := strings.Join(failNames, " ")
	galaxy := `#!/bin/sh
if [ "$1" = "collection" ] && [ "$2" = "list" ]; then
  cat <<'JSON'
` + baseJSON + `
JSON
  exit 0
fi
if [ "$1" = "collection" ] && [ "$2" = "install" ]; then
  for f in ` + fail + `; do
    case "$3" in "$f"*) echo "ERROR: $3 unreachable" 1>&2; exit 1;; esac
  done
  echo "installed $3"
  exit 0
fi
if [ "$1" = "role" ] && [ "$2" = "install" ]; then
  echo "installed roles"
  exit 0
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "ansible-galaxy"), []byte(galaxy), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "ansible"), []byte("#!/bin/sh\necho 'ansible [core 2.16.3]'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	var mu sync.Mutex
	var got []string
	emit = func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() }
	lines = func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }
	return emit, lines
}

func writeReq(t *testing.T, workdir, rel, body string) {
	t.Helper()
	p := filepath.Join(workdir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGalaxyInstallSkipIfSatisfied(t *testing.T) {
	// Base already has community.vmware:3.5.0 → must be SKIPPED. ansible.posix is
	// absent → must be INSTALLED.
	base := `{"/usr/base": {"community.vmware": {"version": "3.5.0"}}}`
	emit, lines := fakeGalaxy(t, base)
	workdir := t.TempDir()
	writeReq(t, workdir, "scripts/proj/requirements.yml", `
collections:
  - name: community.vmware
    version: 3.5.0
  - name: ansible.posix
    version: 1.5.4
`)
	env, err := galaxyInstall(context.Background(), Config{}, workdir, "scripts/proj/requirements.yml", emit)
	if err != nil {
		t.Fatalf("galaxyInstall: %v", err)
	}
	out := strings.Join(lines(), "\n")
	if !strings.Contains(out, "skip community.vmware:3.5.0") {
		t.Errorf("exact-pinned base collection should be skipped:\n%s", out)
	}
	if !strings.Contains(out, "installed ansible.posix:1.5.4") {
		t.Errorf("unsatisfied collection should be installed:\n%s", out)
	}
	// The child env points ansible at the per-run deps dir (workdir first).
	if !strings.HasPrefix(env["ANSIBLE_COLLECTIONS_PATH"], filepath.Join(workdir, ".deps")) {
		t.Errorf("ANSIBLE_COLLECTIONS_PATH = %q, want workdir/.deps first", env["ANSIBLE_COLLECTIONS_PATH"])
	}
}

func TestGalaxyInstallLoudFailureNoFallback(t *testing.T) {
	// Nothing satisfied in base; the install of community.vmware FAILS → the whole
	// run must fail loudly (no fallback to an unpinned/local copy).
	emit, _ := fakeGalaxy(t, `{}`, "community.vmware")
	workdir := t.TempDir()
	writeReq(t, workdir, "scripts/proj/requirements.yml", `
collections:
  - name: community.vmware
    version: 3.5.0
`)
	_, err := galaxyInstall(context.Background(), Config{}, workdir, "scripts/proj/requirements.yml", emit)
	if err == nil || !strings.Contains(err.Error(), "no fallback") {
		t.Fatalf("expected a loud no-fallback failure, got: %v", err)
	}
}

// End-to-end: a checkout run with a requirements.yml in the pinned tree installs
// deps, then runs the entry with the deps env wired in.
func TestRunLocalToolchainCheckoutWithRequirements(t *testing.T) {
	repo, sha := initTestRepoWithReq(t)
	stateDir := t.TempDir()
	emit, lines := fakeToolchainAndGalaxy(t, `echo "ACP=$ANSIBLE_COLLECTIONS_PATH"`)

	m := &runnerproto.ManifestResponse{
		RunType:        "ansible",
		EnvPassthrough: []string{},
		Checkout: &runnerproto.ManifestCheckout{
			Repo: repo, SHA: sha, Entry: "scripts/proj/site.yml",
			ReqPath: "scripts/proj/requirements.yml",
		},
	}
	cfg := Config{AllowCheckout: true, CheckoutRepos: []string{repo}, StateDir: stateDir}
	code := runLocalToolchain(context.Background(), m, cfg, emit)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; lines:\n%s", code, strings.Join(lines(), "\n"))
	}
	out := strings.Join(lines(), "\n")
	if !strings.Contains(out, "installed community.general:5.0.0") {
		t.Errorf("expected the pinned collection to be installed:\n%s", out)
	}
	if !strings.Contains(out, "core 2.16.3") {
		t.Errorf("expected an ansible-core provenance line:\n%s", out)
	}
	if !strings.Contains(out, "ACP=") || !strings.Contains(out, filepath.Join(".deps")) {
		t.Errorf("ansible-playbook did not see the deps collection path:\n%s", out)
	}
}

func TestRunLocalToolchainVaultRefusedNoFile(t *testing.T) {
	repo, sha := initTestRepo(t)
	stateDir := t.TempDir()
	var mu sync.Mutex
	var got []string
	emit := func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() }
	m := &runnerproto.ManifestResponse{
		RunType:  "ansible",
		Checkout: &runnerproto.ManifestCheckout{Repo: repo, SHA: sha, Entry: "site.yml", UsesVault: true},
	}
	// AllowCheckout set so we pass the checkout gates and reach the vault check;
	// no VaultPasswordFile ⇒ loud refusal.
	cfg := Config{AllowCheckout: true, CheckoutRepos: []string{repo}, StateDir: stateDir}
	code := runLocalToolchain(context.Background(), m, cfg, emit)
	if code != -1 {
		t.Fatalf("exit = %d, want -1 (vault refusal)", code)
	}
	if !strings.Contains(strings.Join(got, "\n"), "no -vault-password-file") {
		t.Errorf("refusal should name the missing vault password file: %v", got)
	}
}

func TestLocalCommandVaultFlag(t *testing.T) {
	m := &runnerproto.ManifestResponse{
		RunType:  "ansible",
		Checkout: &runnerproto.ManifestCheckout{Entry: "site.yml", UsesVault: true},
	}
	argv, _, _, err := localCommand(m, Config{VaultPasswordFile: "/etc/cronomicon/vault.pw"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--vault-password-file /etc/cronomicon/vault.pw") {
		t.Errorf("expected --vault-password-file in argv, got %v", argv)
	}
	if argv[len(argv)-1] != "site.yml" {
		t.Errorf("entry should be the last arg, got %v", argv)
	}
}
