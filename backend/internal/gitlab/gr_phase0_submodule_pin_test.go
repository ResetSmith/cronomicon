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

// TestGR0_ASubmoduleLeftHalfWrittenIsNeverUpdatedAgain pins what the released
// code does (seen on 2026-10-12 in a test of present defect 27, reproduced
// here, not changed).
//
// A sync resets the repository's OWN working tree hard, so whatever state it
// was left in is put right by the next sync. A submodule is updated by a
// checkout that refuses to overwrite a tree that differs from its commit. One
// file of a submodule left cut short (a server stopped while it was checking
// the submodule out, a full disk, an edit by hand) and that submodule is never
// updated again: every sync after it warns "worktree contains unstaged
// changes" and goes on, whether or not the repository has moved the
// submodule's pin. The file stays as it was left, and it is what sync hashes
// and catalogues.
//
// Since 2.4.0 a run reads a body from the submodule's objects and not from
// that tree (present defect 27): with the pin unmoved it is handed the whole
// committed file all the same, and with the pin moved it is refused with a
// reason. Before, it ran the cut-off file for good.
//
// To put right: reset the submodule's tree to its commit before, or when, its
// update is refused. The owner's to decide where.
func TestGR0_ASubmoduleLeftHalfWrittenIsNeverUpdatedAgain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	root := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		runGit(t, dir, append([]string{"-c", "protocol.file.allow=always"}, args...)...)
	}
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
	sync()

	// One file of each tree is left cut short.
	own := filepath.Join(clone, "scripts", "loose.sh")
	sub := filepath.Join(clone, "scripts", "playbooks", "lsblk.yml")
	for _, f := range []string{own, sub} {
		if err := os.WriteFile(f, []byte("- ho"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(f string) string {
		t.Helper()
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// The next sync, with nothing new to fetch. The repository's own file is
	// put right. The submodule's is not, and its update is refused.
	said := sync()
	if got := read(own); got != "#!/bin/sh\necho loose\n" {
		t.Fatalf("the repository's own file after the next sync = %q, want it put right by the reset", got)
	}
	const refusal = "worktree contains unstaged changes"
	if got := read(sub); got != "- ho" || !strings.Contains(said, refusal) {
		t.Fatalf("PRESENT DEFECT CHANGED: after the next sync the submodule's cut-off file is %q and the sync said:\n%s\n"+
			"It was left as it was, with the update refused (%q). If a sync now puts a submodule's tree right, invert this test.",
			got, said, refusal)
	}

	// The repository moves the submodule's pin. The submodule is still not
	// updated, on this sync or the one after.
	mustWrite(t, filepath.Join(pb, "lsblk.yml"), "- hosts: all # second\n")
	git(pb, "commit", "-am", "playbooks, second")
	git(filepath.Join(super, "scripts", "playbooks"), "pull", "origin", "main")
	git(super, "commit", "-am", "playbooks, second")
	for pass := 1; pass <= 2; pass++ {
		said = sync()
		if got := read(sub); got != "- ho" || !strings.Contains(said, refusal) {
			t.Fatalf("PRESENT DEFECT CHANGED: sync %d after the pin moved left the submodule's file %q and said:\n%s", pass, got, said)
		}
	}
}
