package gitlab

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CommitFiles and ReadCommitTree (2.5.0 C10, IN-12): an assignment is one
// commit carrying every file, or nothing.

func TestCommitFilesIsOneCommitCarryingEveryFile(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	if r := svc.SyncBlocking(ctx, "manual"); r.SHA == "" {
		t.Fatalf("first sync: %+v", r)
	}
	before := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)

	res, err := svc.CommitFiles(ctx, []FileWrite{
		{Path: "jobs/nightly.yaml", Content: []byte(jobYAML("nightly"))},
		{Path: "scripts/nightly.sh", Content: []byte("#!/bin/sh\necho hi\n"), Executable: true},
		{Path: "ansible/site/roles/a/tasks/main.yml", Content: []byte("- debug: {}\n")},
	}, "assign nightly")
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	after := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)
	if res.CommitSHA != after {
		t.Fatalf("pushed %s, the branch is at %s", res.CommitSHA, after)
	}
	if parent := sysGit(t, bare, "rev-parse", after+"^"); parent != before {
		t.Errorf("not one commit on top of the branch: parent %s, want %s", parent, before)
	}
	files := sysGit(t, bare, "diff-tree", "--no-commit-id", "--name-only", "-r", after)
	for _, p := range []string{"jobs/nightly.yaml", "scripts/nightly.sh", "ansible/site/roles/a/tasks/main.yml"} {
		if !strings.Contains(files, p) {
			t.Errorf("%s is not in the commit (%q)", p, files)
		}
	}
	if mode := sysGit(t, bare, "ls-tree", after, "scripts/nightly.sh"); !strings.HasPrefix(mode, "100755") {
		t.Errorf("the executable bit was not kept: %q", mode)
	}
	if mode := sysGit(t, bare, "ls-tree", after, "jobs/nightly.yaml"); !strings.HasPrefix(mode, "100644") {
		t.Errorf("a plain file was committed as %q", mode)
	}
}

func TestCommitFilesRefusesATakenPathWithNothingPushed(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	if err := os.MkdirAll(filepath.Join(work, "jobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "jobs", "theirs.yaml"), []byte(jobYAML("theirs")), 0o644); err != nil {
		t.Fatal(err)
	}
	sysGit(t, work, "add", "-A")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "theirs")
	sysGit(t, work, "push", "-q", "origin", branch)
	if r := svc.SyncBlocking(ctx, "manual"); r.SHA == "" {
		t.Fatalf("sync: %+v", r)
	}
	before := sysGit(t, bare, "rev-parse", "refs/heads/"+branch)

	_, err := svc.CommitFiles(ctx, []FileWrite{
		{Path: "scripts/new.sh", Content: []byte("echo\n")},
		{Path: "jobs/theirs.yaml", Content: []byte(jobYAML("theirs"))},
	}, "assign")
	var taken *PathsTakenError
	if !errors.As(err, &taken) || len(taken.Paths) != 1 || taken.Paths[0] != "jobs/theirs.yaml" {
		t.Fatalf("want a refusal naming jobs/theirs.yaml, got %v", err)
	}
	if after := sysGit(t, bare, "rev-parse", "refs/heads/"+branch); after != before {
		t.Errorf("a refused commit moved the branch")
	}
	if _, err := os.Stat(filepath.Join(svc.cloneDir, "scripts", "new.sh")); !os.IsNotExist(err) {
		t.Errorf("a refused commit left a file in the clone")
	}
}

func TestCommitFilesRefusesAnUnsyncedClone(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	ctx := context.Background()
	if r := svc.SyncBlocking(ctx, "manual"); r.SHA == "" {
		t.Fatalf("sync: %+v", r)
	}
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sysGit(t, work, "add", "-A")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "later")
	sysGit(t, work, "push", "-q", "origin", branch)

	_, err := svc.CommitFiles(ctx, []FileWrite{{Path: "jobs/a.yaml", Content: []byte(jobYAML("a"))}}, "assign")
	var pe *PreconditionError
	if !errors.As(err, &pe) || !pe.Unsynced {
		t.Fatalf("want an unsynced refusal, got %v", err)
	}
}

func TestCommitFilesRefusesAPathOutOfTheRepository(t *testing.T) {
	svc, _, _ := bareFixture(t)
	for _, p := range []string{"../x", "/etc/x", ".git/config", "a/../b", "a\\b", ""} {
		if _, err := svc.CommitFiles(context.Background(), []FileWrite{{Path: p, Content: []byte("x")}}, "m"); err == nil {
			t.Errorf("%q was accepted", p)
		}
	}
}

func TestReadCommitTreeReadsFilesAndRefusesLinks(t *testing.T) {
	svc, bare, branch := bareFixture(t)
	work := t.TempDir()
	sysGit(t, work, "clone", "-q", "--branch", branch, bare, ".")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(work, "proj", "roles"), 0o755))
	must(os.WriteFile(filepath.Join(work, "proj", "site.yml"), []byte("- hosts: all\n"), 0o644))
	must(os.WriteFile(filepath.Join(work, "proj", "roles", "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(os.MkdirAll(filepath.Join(work, "linked"), 0o755))
	must(os.WriteFile(filepath.Join(work, "linked", "a"), []byte("a"), 0o644))
	must(os.Symlink("a", filepath.Join(work, "linked", "b")))
	sysGit(t, work, "add", "-A")
	sysGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "tree")
	sysGit(t, work, "push", "-q", "origin", branch)
	sha := svc.SyncBlocking(context.Background(), "manual").SHA

	got, err := ReadCommitTree(svc.cloneDir, sha, "proj")
	if err != nil {
		t.Fatalf("ReadCommitTree: %v", err)
	}
	if len(got) != 2 || got[0].Path != "proj/roles/run.sh" || !got[0].Executable || got[1].Path != "proj/site.yml" || got[1].Executable {
		t.Errorf("read %+v", got)
	}
	one, err := ReadCommitTree(svc.cloneDir, sha, "proj/site.yml")
	if err != nil || len(one) != 1 || string(one[0].Content) != "- hosts: all\n" {
		t.Errorf("one file: %+v, %v", one, err)
	}
	if _, err := ReadCommitTree(svc.cloneDir, sha, "linked"); err == nil || !strings.Contains(err.Error(), "linked/b") {
		t.Errorf("a directory holding a link: %v", err)
	}
	if _, err := ReadCommitTree(svc.cloneDir, sha, "nowhere"); err == nil {
		t.Error("a path not in the commit was read")
	}
}
