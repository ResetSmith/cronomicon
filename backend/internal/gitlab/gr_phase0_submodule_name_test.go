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

// TestGR0_ASubmoduleWhoseNameIsNotItsPathStopsTheSync pins present defect 43
// (found by the second review of 2.4.0 Phase R5 on 2026-10-12, reproduced
// here, not changed). In the released code, and not from 2.4.0.
//
// A submodule has a NAME and a PATH, and they are the same unless somebody
// made them differ: `git submodule add --name`, or a submodule moved with
// `git mv`. go-git (5.19.2) looks a submodule up by its name with what is its
// path when it resets a tree in which a submodule's pin has changed
// (worktree.go, checkoutChangeSubmodule), and does not find it.
//
// At the top level: once such a submodule's pin moves, the hard reset of the
// repository's own tree fails, "git reset --hard: submodule not found", and so
// does every sync after it. Global's repository stops syncing altogether.
//
// One level down (a submodule nested in a submodule): the nested one's parent
// is left part of the way through its checkout at every pin move, with the
// files that sort before the nested path at the new commit and the rest at
// the old one, and "update submodule failed … submodule not found" each time.
// Until a3a247a that tree froze at the first such mix; it is rewritten in part
// at each pin move now. Neither is right.
//
// To put right: see that a name differs from its path before the reset and
// say so in a sentence, or bring the submodule to its pin first so that the
// reset has no submodule change to make. The owner's to decide.
func TestGR0_ASubmoduleWhoseNameIsNotItsPathStopsTheSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	git := func(dir string, args ...string) {
		t.Helper()
		runGit(t, dir, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	}

	t.Run("at the top level the sync fails for good", func(t *testing.T) {
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
		svc := &Service{cloneDir: filepath.Join(root, "clone"), repoURL: super, log: slog.New(slog.NewTextHandler(&logs, nil))}
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("the first clone: %v", err)
		}
		// Nothing has moved: a sync is fine.
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Fatalf("a sync with nothing new: %v", err)
		}
		// The submodule's pin moves, and with it a file of the repository's own.
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # second\n")
		git(pb, "commit", "-am", "playbooks, second")
		git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
		mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose, second\n")
		git(super, "commit", "-am", "playbooks and a script, second")
		for pass := 1; pass <= 2; pass++ {
			_, err := svc.cloneOrFetch(t.Context(), "main")
			if err == nil || !strings.Contains(err.Error(), "submodule not found") {
				t.Fatalf("PRESENT DEFECT CHANGED: sync %d after the pin moved = %v. It failed with \"git reset --hard: submodule not found\"; "+
					"if a submodule whose name is not its path is now synced, or refused in a sentence, invert this test.", pass, err)
			}
		}
	})

	t.Run("one level down its parent is left part of the way through", func(t *testing.T) {
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
		// Both pins move.
		mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, second\n")
		git(roles, "commit", "-am", "roles, second")
		git(filepath.Join(pb, "roles", "common"), "pull", "origin", "main")
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # second\n")
		mustWrite(t, filepath.Join(pb, "zlast.yml"), "- hosts: all # z, second\n")
		git(pb, "commit", "-am", "playbooks, second")
		git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
		git(filepath.Join(super, "scripts", "playbooks"), "submodule", "update", "--init")
		git(super, "commit", "-am", "playbooks, second")
		for pass := 1; pass <= 2; pass++ {
			logs.Reset()
			if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
				t.Fatalf("sync %d: %v", pass, err)
			}
		}
		read := func(rel string) string {
			b, err := os.ReadFile(filepath.Join(clone, "scripts", "playbooks", rel))
			if err != nil {
				return "<missing>"
			}
			return string(b)
		}
		before, after, nested := read("lsblk.yml"), read("zlast.yml"), read("roles/common/tasks.yml")
		whole := strings.Contains(before, "second") && strings.Contains(after, "second") && strings.Contains(nested, "second")
		if whole || !strings.Contains(logs.String(), "submodule not found") {
			t.Fatalf("PRESENT DEFECT CHANGED: after both pins moved and two syncs the submodule holds %q, %q and %q, and the sync said:\n%s\n"+
				"Its update failed with \"submodule not found\" and left it part of the way; if it is now whole, or refused in a sentence, invert this test.",
				before, after, nested, logs.String())
		}
	})
}
