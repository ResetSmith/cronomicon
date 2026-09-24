package gitlab

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs a git command in dir, failing the test on error. It is used only
// to build fixture repos — cloneOrFetch itself uses go-git, not the git binary.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// TestCloneOrFetch_PopulatesSubmodules proves a git submodule mounted under
// scripts/ (the real-world case: an Ansible playbooks repo at scripts/playbooks)
// is checked out by cloneOrFetch so discoverScripts can ingest its files. Before
// the submodule fix the directory cloned empty and the playbooks never surfaced
// on the Scripts page.
func TestCloneOrFetch_PopulatesSubmodules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	root := t.TempDir()

	// 1. Upstream "playbooks" repo with one ansible playbook.
	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all\n  tasks: []\n")
	runGit(t, pb, "init", "-b", "main")
	runGit(t, pb, "add", ".")
	runGit(t, pb, "commit", "-m", "init playbooks")

	// 2. Superproject with a loose script under scripts/ plus the playbooks repo
	//    added as a submodule at scripts/playbooks. git >=2.38 blocks file://
	//    submodule transport by default, so the fixture opts in explicitly.
	super := filepath.Join(root, "super-origin")
	mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho hi\n")
	runGit(t, super, "init", "-b", "main")
	runGit(t, super, "add", ".")
	runGit(t, super, "commit", "-m", "init super")
	runGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", pb, "scripts/playbooks")
	runGit(t, super, "commit", "-m", "add playbooks submodule")

	// 3. Sync clones the superproject; updateSubmodules must pull the submodule.
	clone := filepath.Join(root, "clone")
	svc := &Service{cloneDir: clone, repoURL: super}
	if _, err := svc.cloneOrFetch("main"); err != nil {
		t.Fatalf("cloneOrFetch: %v", err)
	}

	// The submodule's file must exist on disk after the clone...
	if _, err := os.Stat(filepath.Join(clone, "scripts", "playbooks", "lsblk.yml")); err != nil {
		t.Fatalf("submodule file not checked out: %v", err)
	}

	// ...and be discovered as a script carrying its real nested SourcePath.
	scripts, errs := svc.parseScripts()
	if len(errs) != 0 {
		t.Fatalf("parseScripts errors: %v", errs)
	}
	var foundPlaybook, foundLoose bool
	for _, sc := range scripts {
		switch filepath.ToSlash(sc.SourcePath) {
		case "scripts/playbooks/lsblk.yml":
			foundPlaybook = true
		case "scripts/loose.sh":
			foundLoose = true
		}
	}
	if !foundLoose {
		t.Errorf("loose script not discovered; got %d scripts", len(scripts))
	}
	if !foundPlaybook {
		t.Fatalf("playbook from submodule not discovered; got %d scripts: %+v", len(scripts), scripts)
	}
}

// TestCloneOrFetch_WarnsOnGitlinkWithoutGitmodules reproduces the broken state
// where scripts/playbooks is a submodule gitlink but the repo has no .gitmodules
// mapping (the result of `git add`-ing a nested clone). Without a URL nothing can
// fetch it, so the directory checks out empty — and the sync must say so loudly
// rather than silently dropping the playbooks.
func TestCloneOrFetch_WarnsOnGitlinkWithoutGitmodules(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}

	root := t.TempDir()

	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all\n  tasks: []\n")
	runGit(t, pb, "init", "-b", "main")
	runGit(t, pb, "add", ".")
	runGit(t, pb, "commit", "-m", "init playbooks")

	// Add the submodule properly, then drop .gitmodules and commit — leaving a
	// bare gitlink with no URL mapping.
	super := filepath.Join(root, "super-origin")
	mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho hi\n")
	runGit(t, super, "init", "-b", "main")
	runGit(t, super, "add", ".")
	runGit(t, super, "commit", "-m", "init super")
	runGit(t, super, "-c", "protocol.file.allow=always", "submodule", "add", pb, "scripts/playbooks")
	runGit(t, super, "rm", "-f", ".gitmodules")
	runGit(t, super, "commit", "-m", "drop .gitmodules: bare gitlink")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	clone := filepath.Join(root, "clone")
	svc := &Service{cloneDir: clone, repoURL: super, log: logger}
	if _, err := svc.cloneOrFetch("main"); err != nil {
		t.Fatalf("cloneOrFetch: %v", err)
	}

	// A bare gitlink yields no playbook files...
	if matches, _ := filepath.Glob(filepath.Join(clone, "scripts", "playbooks", "*.yml")); len(matches) != 0 {
		t.Fatalf("expected no playbook files for a bare gitlink, got %v", matches)
	}
	// ...and the sync must warn loudly about the missing mapping.
	logs := buf.String()
	if !strings.Contains(logs, "submodule gitlink has no .gitmodules entry") ||
		!strings.Contains(logs, "scripts/playbooks") {
		t.Fatalf("expected undeclared-gitlink warning naming scripts/playbooks; logs:\n%s", logs)
	}
}
