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

// TestASubmoduleLeftHalfWrittenIsPutRightByTheNextSync (present defect 42;
// this was the pin TestGR0_ASubmoduleLeftHalfWrittenIsNeverUpdatedAgain,
// inverted on the owner's decision of 2026-10-12).
//
// A sync resets the repository's OWN working tree hard, so whatever state it
// was left in is put right by the next sync. A submodule is updated by a
// checkout that refuses to overwrite a tree that differs from its commit. One
// file of a submodule left cut short (a server stopped while it was checking
// the submodule out, a full disk, an edit by hand) and that submodule was
// never updated again: every sync after it warned "worktree contains unstaged
// changes" and went on, whether or not the repository had moved the
// submodule's pin.
//
// A submodule whose update is refused for that is reset to its commit, with
// the submodules nested in it, and updated once more. The sync says that it
// did.
func TestASubmoduleLeftHalfWrittenIsPutRightByTheNextSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		runGit(t, dir, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	}
	// roles ← playbooks ← the repository, as in the read's own test.
	roles := filepath.Join(root, "roles-origin")
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, first\n")
	git(roles, "init", "-b", "main")
	git(roles, "add", ".")
	git(roles, "commit", "-m", "init roles")
	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # first\n")
	mustWrite(t, filepath.Join(pb, "other.yml"), "- hosts: all # other\n")
	git(pb, "init", "-b", "main")
	git(pb, "add", ".")
	git(pb, "commit", "-m", "init playbooks")
	git(pb, "submodule", "add", roles, "roles/common")
	git(pb, "commit", "-m", "add roles")
	super := filepath.Join(root, "super-origin")
	mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose\n")
	git(super, "init", "-b", "main")
	git(super, "add", ".")
	git(super, "commit", "-m", "init super")
	git(super, "submodule", "add", pb, "scripts/playbooks")
	git(super, "commit", "-m", "add playbooks")

	var logs bytes.Buffer
	clone := filepath.Join(root, "clone")
	svc := &Service{cloneDir: clone, repoURL: super, log: slog.New(slog.NewTextHandler(&logs, nil))}
	sync := func() string {
		t.Helper()
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("cloneOrFetch: %v", err)
		}
		return logs.String()
	}
	if said := sync(); strings.Contains(said, "was reset to it") {
		t.Fatalf("the first clone said a submodule was reset:\n%s", said)
	}
	own := filepath.Join(clone, "scripts", "loose.sh")
	sub := filepath.Join(clone, "scripts", "playbooks", "lsblk.yml")
	other := filepath.Join(clone, "scripts", "playbooks", "other.yml")
	nested := filepath.Join(clone, "scripts", "playbooks", "roles", "common", "tasks.yml")
	read := func(f string) string {
		t.Helper()
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		return string(b)
	}
	const reset, refusal = "was reset to it", "worktree contains unstaged changes"

	// A sync with nothing wrong and nothing new resets nothing.
	if said := sync(); strings.Contains(said, reset) || strings.Contains(said, refusal) {
		t.Fatalf("a sync of a clean clone said:\n%s", said)
	}

	// One file of each tree is left cut short, and one of the submodule's is
	// gone.
	for _, f := range []string{own, sub} {
		if err := os.WriteFile(f, []byte("- ho"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}

	// The next sync, with nothing new to fetch, puts all of it right, and says
	// that the submodule was reset.
	said := sync()
	if got := read(own); got != "#!/bin/sh\necho loose\n" {
		t.Errorf("the repository's own file after the next sync = %q", got)
	}
	if got := read(sub); got != "- hosts: all # first\n" {
		t.Errorf("the submodule's cut-off file after the next sync = %q, want it put right; the sync said:\n%s", got, said)
	}
	if got := read(other); got != "- hosts: all # other\n" {
		t.Errorf("the submodule's removed file after the next sync = %q, want it back", got)
	}
	if !strings.Contains(said, reset) || !strings.Contains(said, "scripts/playbooks") || strings.Contains(said, "update submodule failed") {
		t.Errorf("the sync should say the submodule was reset, and not that its update failed:\n%s", said)
	}
	// And the one after has nothing to reset.
	if said := sync(); strings.Contains(said, reset) || strings.Contains(said, refusal) {
		t.Errorf("the sync after the repair said:\n%s", said)
	}

	// The same with the pin moved while the tree is in that state, and with a
	// file of the NESTED submodule cut short as well: the submodule is updated
	// to its new pin.
	for _, f := range []string{sub, nested} {
		if err := os.WriteFile(f, []byte("- ho"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The nested submodule's pin moves too. The reset alone would leave it at
	// the commit it was at: the update made after the reset is what goes on
	// into it.
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, second\n")
	git(roles, "commit", "-am", "roles, second")
	git(filepath.Join(pb, "roles", "common"), "pull", "origin", "main")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # second\n")
	git(pb, "commit", "-am", "playbooks, second")
	git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
	git(filepath.Join(super, "scripts", "playbooks"), "submodule", "update", "--init")
	git(super, "commit", "-am", "playbooks, second")
	said = sync()
	if got := read(sub); got != "- hosts: all # second\n" {
		t.Errorf("after the pin moved the submodule's file = %q, want the second commit's; the sync said:\n%s", got, said)
	}
	if got := read(nested); got != "- debug: msg=the role, second\n" {
		t.Errorf("the nested submodule's cut-off file = %q, want its new pin's; the sync said:\n%s", got, said)
	}
	if strings.Contains(said, "update submodule failed") {
		t.Errorf("the submodule's update failed:\n%s", said)
	}
	// Both were cut short, and each reset is said by its own path.
	for _, path := range []string{"path=scripts/playbooks\n", "path=scripts/playbooks/roles/common\n"} {
		if !strings.Contains(said, reset+" (files there that its commit does not hold were removed)\" submodule=scripts/playbooks "+path) {
			t.Errorf("the sync does not say that the submodule at %q was reset:\n%s", strings.TrimSpace(path), said)
		}
	}

	// The other way into the same state, with no file cut short at all: a
	// nested submodule's update fails ONCE. Both pins move again, and the
	// nested submodule's remote is away for one sync. Its parent is checked
	// out at its new commit, which pins a commit the nested one is not at, and
	// that difference is one the parent's next update is refused for. It was
	// refused on every sync after; the sync after the remote is back puts both
	// right.
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, third\n")
	git(roles, "commit", "-am", "roles, third")
	git(filepath.Join(pb, "roles", "common"), "pull", "origin", "main")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # third\n")
	git(pb, "commit", "-am", "playbooks, third")
	git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
	git(filepath.Join(super, "scripts", "playbooks"), "submodule", "update", "--init")
	git(super, "commit", "-am", "playbooks, third")
	if err := os.Rename(roles, roles+".away"); err != nil {
		t.Fatal(err)
	}
	said = sync()
	if got := read(sub); got != "- hosts: all # third\n" {
		t.Fatalf("with the nested submodule's remote away, its parent's file = %q, want the third commit's; the sync said:\n%s", got, said)
	}
	if got := read(nested); got != "- debug: msg=the role, second\n" || !strings.Contains(said, "update submodule failed") {
		t.Fatalf("the nested submodule's update did not fail (its file is %q), and the test proves nothing; the sync said:\n%s", got, said)
	}
	if err := os.Rename(roles+".away", roles); err != nil {
		t.Fatal(err)
	}
	said = sync()
	if got := read(nested); got != "- debug: msg=the role, third\n" {
		t.Errorf("the sync after its remote came back left the nested submodule's file %q, want the third commit's; the sync said:\n%s", got, said)
	}
	if strings.Contains(said, "update submodule failed") {
		t.Errorf("the submodule's update is still refused after the nested one's remote came back:\n%s", said)
	}
	// And the one after that has nothing to put right.
	if said := sync(); strings.Contains(said, reset) || strings.Contains(said, refusal) {
		t.Errorf("the sync after the recovery said:\n%s", said)
	}
}
