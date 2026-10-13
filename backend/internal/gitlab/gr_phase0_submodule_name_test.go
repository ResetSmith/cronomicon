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

// TestASubmoduleWhoseNameIsNotItsPathIsRefusedInASentence (present defect 43;
// this was the pin TestGR0_ASubmoduleWhoseNameIsNotItsPathStopsTheSync,
// inverted on the owner's decision of 2026-10-13). In the released code, and
// not from 2.4.0.
//
// A submodule has a NAME and a PATH, and they are the same unless somebody
// made them differ: `git submodule add --name`, or a submodule moved with
// `git mv`. go-git (5.19.2) finds a submodule by its name and, when a checkout
// changes a submodule's pin, asks for it by its path: it does not find it.
//
// At the top level that was "git reset --hard: submodule not found" on every
// sync from the pin's first move: Global's repository stopped syncing, with
// its clone left part of the way through a reset. One level down it left the
// nested submodule's parent part of the way through its checkout.
//
// It is refused before anything is moved, in a sentence that names the
// submodule and says how to put it right; the clone, or the parent submodule,
// stays whole at the commit it was at; and what the sentence says to do works.
func TestASubmoduleWhoseNameIsNotItsPathIsRefusedInASentence(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		runGit(t, dir, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	}
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(parts...))
		if err != nil {
			return "<missing>"
		}
		return string(b)
	}

	t.Run("at the top level", func(t *testing.T) {
		root := t.TempDir()
		pb := filepath.Join(root, "playbooks-origin")
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # first\n")
		git(pb, "init", "-b", "main")
		git(pb, "add", ".")
		git(pb, "commit", "-m", "init playbooks")
		super := filepath.Join(root, "super-origin")
		mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose\n")
		git(super, "init", "-b", "main")
		git(super, "add", ".")
		git(super, "commit", "-m", "init super")
		git(super, "submodule", "add", "--name", "playbooks", pb, "scripts/playbooks")
		git(super, "commit", "-m", "add playbooks, named other than its path")

		var logs bytes.Buffer
		clone := filepath.Join(root, "clone")
		svc := &Service{cloneDir: clone, repoURL: super, log: slog.New(slog.NewTextHandler(&logs, nil))}
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the first clone: %v", err)
		}
		// Nothing has moved: a sync is fine, and it says ahead of time what will
		// not be.
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("a sync with nothing new: %v", err)
		}
		if said := logs.String(); !strings.Contains(said, "name in .gitmodules is not its path") || !strings.Contains(said, "submodule=playbooks") {
			t.Errorf("a sync of a repository with such a submodule does not say so ahead of time:\n%s", said)
		}

		// The submodule's pin moves, and with it a file of the repository's own.
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # second\n")
		git(pb, "commit", "-am", "playbooks, second")
		git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
		mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose, second\n")
		git(super, "commit", "-am", "playbooks and a script, second")
		for pass := 1; pass <= 2; pass++ {
			_, err := svc.cloneOrFetch(t.Context(), "main")
			if err == nil {
				t.Fatalf("sync %d after the pin moved succeeded: the test's fixture no longer has what go-git cannot do", pass)
			}
			for _, want := range []string{"the submodule at scripts/playbooks", `named "playbooks"`, `[submodule "scripts/playbooks"]`, "its pin has moved"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("sync %d: the refusal does not say %q: %v", pass, want, err)
				}
			}
			if strings.Contains(err.Error(), "git reset --hard") {
				t.Errorf("sync %d was refused by the reset, part of the way, and not before it: %v", pass, err)
			}
			// The clone is whole, at the commit it was last synced to.
			if own, sub := read(clone, "scripts", "loose.sh"), read(clone, "scripts", "playbooks", "lsblk.yml"); own != "#!/bin/sh\necho loose\n" || sub != "- hosts: all # first\n" {
				t.Errorf("sync %d: the refused sync moved the clone: its own file is %q, the submodule's %q", pass, own, sub)
			}
		}

		// What the sentence says to do: the section is renamed to the path.
		git(super, "config", "-f", ".gitmodules", "--rename-section", "submodule.playbooks", "submodule.scripts/playbooks")
		git(super, "commit", "-am", "name the submodule by its path")
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the sync after the section was renamed: %v\n%s", err, logs.String())
		}
		if own, sub := read(clone, "scripts", "loose.sh"), read(clone, "scripts", "playbooks", "lsblk.yml"); own != "#!/bin/sh\necho loose, second\n" || sub != "- hosts: all # second\n" {
			t.Errorf("after the section was renamed the clone holds %q and %q, want both at the second commit; the sync said:\n%s", own, sub, logs.String())
		}
		// And it goes on syncing: a third pin.
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # third\n")
		git(pb, "commit", "-am", "playbooks, third")
		git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
		git(super, "commit", "-am", "playbooks, third")
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the next sync: %v", err)
		}
		if sub := read(clone, "scripts", "playbooks", "lsblk.yml"); sub != "- hosts: all # third\n" {
			t.Errorf("the submodule after its pin moved again = %q; the sync said:\n%s", sub, logs.String())
		}
		// Named by its path, it is not warned about any more.
		if said := logs.String(); strings.Contains(said, "is not its path") {
			t.Errorf("a submodule that is named by its path is still warned about:\n%s", said)
		}
	})

	t.Run("one level down", func(t *testing.T) {
		root := t.TempDir()
		roles := filepath.Join(root, "roles-origin")
		mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, first\n")
		git(roles, "init", "-b", "main")
		git(roles, "add", ".")
		git(roles, "commit", "-m", "init roles")
		pb := filepath.Join(root, "playbooks-origin")
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # first\n")
		mustWrite(t, filepath.Join(pb, "zlast.yml"), "- hosts: all # z, first\n")
		git(pb, "init", "-b", "main")
		git(pb, "add", ".")
		git(pb, "commit", "-m", "init playbooks")
		git(pb, "submodule", "add", "--name", "rolesmod", roles, "roles/common")
		git(pb, "commit", "-m", "add roles, named other than its path")
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
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the first clone: %v", err)
		}
		// Both pins move, and a file of the repository's own.
		bump := func(n string) {
			t.Helper()
			mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, "+n+"\n")
			git(roles, "commit", "-am", "roles, "+n)
			git(filepath.Join(pb, "roles", "common"), "pull", "origin", "main")
			mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # "+n+"\n")
			mustWrite(t, filepath.Join(pb, "zlast.yml"), "- hosts: all # z, "+n+"\n")
			git(pb, "commit", "-am", "playbooks, "+n)
			git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
			git(filepath.Join(super, "scripts", "playbooks"), "submodule", "update", "--init")
			mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose, "+n+"\n")
			git(super, "commit", "-am", "playbooks, "+n)
		}
		bump("second")
		for pass := 1; pass <= 2; pass++ {
			logs.Reset()
			if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
				t.Fatalf("sync %d: the repository's own sync failed: %v", pass, err)
			}
			said := logs.String()
			for _, want := range []string{"update submodule failed", "the submodule at scripts/playbooks/roles/common", `named \"rolesmod\"`,
				"the .gitmodules of the submodule at scripts/playbooks", `[submodule \"roles/common\"]`} {
				if !strings.Contains(said, want) {
					t.Errorf("sync %d: the sync does not say %s:\n%s", pass, want, said)
				}
			}
			// The repository's own file moved; the submodule is WHOLE at the
			// commit it was at, and not part of the way to the next.
			if own := read(clone, "scripts", "loose.sh"); own != "#!/bin/sh\necho loose, second\n" {
				t.Errorf("sync %d: the repository's own file = %q, want the second commit's", pass, own)
			}
			before, after, nested := read(clone, "scripts", "playbooks", "lsblk.yml"), read(clone, "scripts", "playbooks", "zlast.yml"),
				read(clone, "scripts", "playbooks", "roles", "common", "tasks.yml")
			if !strings.Contains(before, "first") || !strings.Contains(after, "first") || !strings.Contains(nested, "first") {
				t.Errorf("sync %d: the submodule holds %q, %q and %q: it is not whole at its first commit", pass, before, after, nested)
			}
		}

		// The section is renamed in the PLAYBOOKS repository, as the sentence says.
		git(pb, "config", "-f", ".gitmodules", "--rename-section", "submodule.rolesmod", "submodule.roles/common")
		git(pb, "commit", "-am", "name the roles submodule by its path")
		git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
		git(super, "commit", "-am", "playbooks, with its submodule named by its path")
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the sync after the section was renamed: %v", err)
		}
		before, after, nested := read(clone, "scripts", "playbooks", "lsblk.yml"), read(clone, "scripts", "playbooks", "zlast.yml"),
			read(clone, "scripts", "playbooks", "roles", "common", "tasks.yml")
		if !strings.Contains(before, "second") || !strings.Contains(after, "second") || !strings.Contains(nested, "second") {
			t.Errorf("after the section was renamed the submodule holds %q, %q and %q, want all three at the second commit; the sync said:\n%s",
				before, after, nested, logs.String())
		}
	})
}
