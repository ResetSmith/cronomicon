package gitlab

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/ResetSmith/cronomicon/internal/execspec"
)

// syncedHead syncs the clone and says which commit it is then at.
func syncedHead(t *testing.T, svc *Service) string {
	t.Helper()
	repo, err := svc.cloneOrFetch(t.Context(), "main")
	if err != nil {
		t.Fatalf("cloneOrFetch: %v", err)
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", "main"), true)
	if err != nil {
		t.Fatalf("resolve origin/main: %v", err)
	}
	return ref.Hash().String()
}

// TestAScriptInASubmoduleIsReadFromTheSubmodulesObjects (present defect 27,
// and the owner's answer of 2026-10-12 to what it means for submodules).
//
// A script's body is read from the commit's objects. Sync finds a playbook of
// Global's submodule by walking the working tree and records it as
// scripts/playbooks/x.yml; the repository's own commit holds a gitlink at
// scripts/playbooks and none of the submodule's files. Read as first worded,
// every submodule playbook would have stopped running.
//
// A path through a submodule is read from the SUBMODULE's own objects, at the
// commit the repository pins for it, one hop for each nested submodule. This
// reads what a real sync leaves on disk. Where that cannot be opened the read
// is refused with a reason, and the working tree is not read in its place.
func TestAScriptInASubmoduleIsReadFromTheSubmodulesObjects(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	allow := []string{"-c", "protocol.file.allow=always"}
	git := func(dir string, args ...string) {
		t.Helper()
		runGit(t, dir, append(append([]string{}, allow...), args...)...)
	}

	// roles ← playbooks (roles is its submodule at roles/common) ← the
	// repository (playbooks is its submodule at scripts/playbooks).
	roles := filepath.Join(root, "roles-origin")
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, first\n")
	git(roles, "init", "-b", "main")
	git(roles, "add", ".")
	git(roles, "commit", "-m", "init roles")

	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # the playbook, first\n")
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
	first := syncedHead(t, svc)

	read := func(sha, path string) (string, error) {
		t.Helper()
		data, _, err := execspec.ReadCommitFile(clone, sha, path, 0)
		return string(data), err
	}
	refused := func(what, sha, path string, wants ...string) {
		t.Helper()
		got, err := read(sha, path)
		var unread *execspec.CommitReadError
		if err == nil || !errors.As(err, &unread) {
			t.Errorf("%s: read %q, err %v; want a refusal with a reason", what, got, err)
			return
		}
		for _, w := range wants {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: the reason %q does not say %q", what, err, w)
			}
		}
		if strings.Contains(err.Error(), root) {
			t.Errorf("%s: the reason names a path on the server: %q", what, err)
		}
	}

	// The three kinds of path, with the working tree's copy of each made
	// unusable first: what is read is not the working tree.
	for path, want := range map[string]string{
		"scripts/loose.sh":                         "#!/bin/sh\necho loose\n",
		"scripts/playbooks/lsblk.yml":              "- hosts: all # the playbook, first\n",
		"scripts/playbooks/roles/common/tasks.yml": "- debug: msg=the role, first\n",
	} {
		if err := os.WriteFile(filepath.Join(clone, path), []byte("half wr"), 0o644); err != nil {
			t.Fatalf("the sync did not check out %s: %v", path, err)
		}
		if got, err := read(first, path); err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q from the objects", path, got, err, want)
		}
		// Put back, so that the syncs below have nothing to repair: a submodule
		// whose working tree differs from its commit is reset by the next sync
		// (TestASubmoduleLeftHalfWrittenIsPutRightByTheNextSync), with a warning
		// this test does not expect.
		if err := os.WriteFile(filepath.Join(clone, path), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	refused("the submodule itself", first, "scripts/playbooks", "a submodule")
	refused("a file the submodule's commit does not hold", first, "scripts/playbooks/nothing.yml", "not in commit")

	// The repository moves its submodule to a new commit, and so does the
	// playbooks repository its own. After a sync both are read at the new pins.
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, second\n")
	git(roles, "commit", "-am", "roles, second")
	git(filepath.Join(pb, "roles", "common"), "pull", "origin", "main")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # the playbook, second\n")
	git(pb, "commit", "-am", "playbooks, second")
	git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
	git(filepath.Join(super, "scripts", "playbooks"), "submodule", "update", "--init")
	git(super, "commit", "-am", "playbooks, second")
	second := syncedHead(t, svc)
	if second == first {
		t.Fatal("the repository did not move")
	}
	if got, err := read(second, "scripts/playbooks/lsblk.yml"); err != nil || got != "- hosts: all # the playbook, second\n" {
		t.Errorf("after the sync the playbook = %q (%v), want the second", got, err)
	}
	if got, err := read(second, "scripts/playbooks/roles/common/tasks.yml"); err != nil || got != "- debug: msg=the role, second\n" {
		t.Errorf("after the sync the nested submodule's file = %q (%v), want the second", got, err)
	}
	// And the commit synced before is still read as it was: a run pinned to it
	// is handed what that commit pinned, in the submodules too.
	if got, err := read(first, "scripts/playbooks/lsblk.yml"); err != nil || got != "- hosts: all # the playbook, first\n" {
		t.Errorf("the playbook at the commit synced before = %q (%v), want the first", got, err)
	}
	if got, err := read(first, "scripts/playbooks/roles/common/tasks.yml"); err != nil || got != "- debug: msg=the role, first\n" {
		t.Errorf("the nested submodule's file at the commit synced before = %q (%v), want the first", got, err)
	}

	// The repository pins a third commit of the submodule, and the submodule's
	// update fails (its remote has gone). The sync goes on, as it always has:
	// the submodule's working tree is still the second commit's. The read is
	// refused, and does not hand out the working tree's older file.
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # the playbook, third\n")
	git(pb, "commit", "-am", "playbooks, third")
	git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
	git(super, "commit", "-am", "playbooks, third")
	if err := os.Rename(pb, pb+".gone"); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	third := syncedHead(t, svc)
	if !strings.Contains(logs.String(), "update submodule failed") {
		t.Fatalf("the submodule's update did not fail, and the test proves nothing; logs:\n%s", logs.String())
	}
	if onDisk, err := os.ReadFile(filepath.Join(clone, "scripts", "playbooks", "lsblk.yml")); err != nil || !strings.Contains(string(onDisk), "second") {
		t.Fatalf("the working tree's playbook = %q (%v), want the second commit's left in place", onDisk, err)
	}
	refused("a submodule that was not updated to its pin", third, "scripts/playbooks/lsblk.yml",
		"playbooks", "does not hold commit", "not updated by the last sync")
	// The repository's own files at that commit are read.
	if got, err := read(third, "scripts/loose.sh"); err != nil || got != "#!/bin/sh\necho loose\n" {
		t.Errorf("the repository's own script at the third commit = %q (%v)", got, err)
	}

	// An agency's repository's submodules are not fetched at all (GR-10): a
	// path through one is refused, and says the submodule was not fetched.
	agencyClone := filepath.Join(root, "clone-agency")
	agency := &Service{cloneDir: agencyClone, repoURL: super, repoID: "repo-b", log: slog.New(slog.NewTextHandler(&logs, nil))}
	at := syncedHead(t, agency)
	data, _, err := execspec.ReadCommitFile(agencyClone, at, "scripts/playbooks/lsblk.yml", 0)
	var unread *execspec.CommitReadError
	if err == nil || !errors.As(err, &unread) || !strings.Contains(err.Error(), "has not been fetched") {
		t.Errorf("a submodule of an agency's repository: read %q, err %v; want it refused as not fetched", data, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(agencyClone, ".git", "modules")); len(entries) != 0 {
		t.Errorf("the read left %d entries under the clone's .git/modules: it must not create a store for a submodule that was not fetched", len(entries))
	}
}
