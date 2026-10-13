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

// A submodule whose name is not its path is refused when its pin moves, because
// go-git's checkout cannot do it (present defect 43). That is true only of a
// submodule that has a commit CHECKED OUT in the clone. One that has not is
// passed over by the checkout, and a repository that holds one synced before
// the refusal was there: every submodule of an agency's repository (they are
// never fetched), one of Global's that the server cannot fetch, and the same
// one level down. The first form of the refusal stopped all three (found by the
// review of 2.4.0 Phase R5's last part). They sync, at every pin move.

// One level down: Global's repository has a submodule named by its path
// (scripts/playbooks), which has a NESTED submodule whose name is not its path
// and which the server cannot fetch, so it is never checked out. Both pins
// move, and scripts/playbooks follows its own.
func TestANestedMisnamedSubmoduleThatIsNotCheckedOutDoesNotFreezeItsParent(t *testing.T) {
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
	root := t.TempDir()
	roles := filepath.Join(root, "roles-origin")
	mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, first\n")
	git(roles, "init", "-b", "main")
	git(roles, "add", ".")
	git(roles, "commit", "-m", "init roles")
	pb := filepath.Join(root, "playbooks-origin")
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # first\n")
	git(pb, "init", "-b", "main")
	git(pb, "add", ".")
	git(pb, "commit", "-m", "init playbooks")
	git(pb, "submodule", "add", "--name", "rolesmod", roles, "roles/common")
	git(pb, "config", "-f", ".gitmodules", "submodule.rolesmod.url", filepath.Join(root, "no-such-repo.git"))
	git(pb, "add", ".gitmodules")
	git(pb, "commit", "-m", "add roles, named other than its path, at a URL the server cannot fetch")
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
	if got := read(clone, "scripts", "playbooks", "roles", "common", "tasks.yml"); got != "<missing>" {
		t.Fatalf("the fixture's nested submodule was fetched (%q): this test is of one that cannot be", got)
	}
	for n, word := range []string{"second", "third"} {
		mustWrite(t, filepath.Join(roles, "tasks.yml"), "- debug: msg=the role, "+word+"\n")
		git(roles, "commit", "-am", "roles, "+word)
		git(filepath.Join(pb, "roles", "common"), "pull", roles, "main")
		mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # "+word+"\n")
		git(pb, "commit", "-am", "playbooks, "+word)
		git(filepath.Join(super, "scripts", "playbooks"), "pull", pb, "main")
		git(super, "commit", "-am", "playbooks, "+word)
		logs.Reset()
		if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
			t.Errorf("pin move %d: the repository's own sync failed: %v", n+1, err)
			continue
		}
		got := read(clone, "scripts", "playbooks", "lsblk.yml")
		if got != "- hosts: all # "+word+"\n" {
			t.Errorf("pin move %d: scripts/playbooks did not follow its pin: its file is %q, want the %s commit's. The sync said:\n%s",
				n+1, got, word, logs.String())
		}
	}
}

// At the top level: an agency's repository (its submodules are never fetched),
// and Global's with a submodule the server cannot fetch. The pin moves, twice,
// and the repository's own files follow each time. Nothing is said ahead of
// time either: the warning that the sync WILL be refused would not be true.
func TestAMisnamedSubmoduleThatIsNotCheckedOutDoesNotStopTheSync(t *testing.T) {
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
	for _, c := range []struct {
		name       string
		repoID     string
		unfetchURL bool
	}{
		{"an agency's repository", "repo-b", false},
		{"Global's, the submodule's URL cannot be fetched", "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
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
			if c.unfetchURL {
				git(super, "config", "-f", ".gitmodules", "submodule.playbooks.url", filepath.Join(root, "no-such-repo.git"))
				git(super, "add", ".gitmodules")
			}
			git(super, "commit", "-m", "add playbooks, named other than its path")

			var logs bytes.Buffer
			clone := filepath.Join(root, "clone")
			svc := &Service{cloneDir: clone, repoURL: super, repoID: c.repoID, log: slog.New(slog.NewTextHandler(&logs, nil))}
			if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
				t.Fatalf("the first clone: %v", err)
			}
			if _, err := svc.cloneOrFetch(t.Context(), "main"); err != nil {
				t.Fatalf("a sync with nothing new: %v", err)
			}
			if got := read(clone, "scripts", "playbooks", "lsblk.yml"); got != "<missing>" {
				t.Fatalf("the fixture's submodule was checked out (%q): this test is of one that is not", got)
			}
			if strings.Contains(logs.String(), "will be refused when its pin moves") {
				t.Errorf("a submodule that is not checked out was warned about ahead of time:\n%s", logs.String())
			}

			for n, word := range []string{"second", "third"} {
				mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # "+word+"\n")
				git(pb, "commit", "-am", "playbooks, "+word)
				git(filepath.Join(super, "scripts", "playbooks"), "pull", pb, "main")
				mustWrite(t, filepath.Join(super, "scripts", "loose.sh"), "#!/bin/sh\necho loose, "+word+"\n")
				git(super, "commit", "-am", "playbooks and a script, "+word)
				logs.Reset()
				_, err := svc.cloneOrFetch(t.Context(), "main")
				own := read(clone, "scripts", "loose.sh")
				if err != nil {
					t.Errorf("pin move %d: the sync failed: %v\n  the repository's own file in the clone: %q", n+1, err, own)
					continue
				}
				if want := "#!/bin/sh\necho loose, " + word + "\n"; own != want {
					t.Errorf("pin move %d: the repository's own file is %q, want %q", n+1, own, want)
				}
				if strings.Contains(logs.String(), "will be refused when its pin moves") {
					t.Errorf("pin move %d: a submodule that is not checked out was warned about:\n%s", n+1, logs.String())
				}
			}
		})
	}
}
