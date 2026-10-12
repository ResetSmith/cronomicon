// Package gittest is TEST SUPPORT: it makes a directory a Git repository with
// a commit in it, the way a synced clone is one.
//
// Since 2.4.0 a script kept as a file is read from a commit's objects and not
// from the clone's working tree (present defect 27; execspec/commitread.go). A
// test that used to write a file into a directory and call it the clone has to
// commit it, and tell the database which commit was synced.
//
// Only test files import this package. It is a normal package rather than a
// _test.go file because several packages (execspec, runner, sshexec, api) need
// it and Go has no cross-package test helper mechanism.
package gittest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Commit writes files (repository-relative path → content) into dir, makes dir
// a repository if it is not one yet, commits them and returns the commit.
func Commit(t testing.TB, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("gittest: %v", err)
	}
	repo, err := gogit.PlainOpen(dir)
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		repo, err = gogit.PlainInit(dir, false)
	}
	if err != nil {
		t.Fatalf("gittest: open %s: %v", dir, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("gittest: %v", err)
	}
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("gittest: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("gittest: %v", err)
		}
		if _, err := wt.Add(rel); err != nil {
			t.Fatalf("gittest: add %s: %v", rel, err)
		}
	}
	return CommitStaged(t, repo)
}

// CommitStaged commits what is staged in repo and returns the commit. For a
// test that stages something Commit cannot write: a symbolic link, a removal.
func CommitStaged(t testing.TB, repo *gogit.Repository) string {
	t.Helper()
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("gittest: %v", err)
	}
	h, err := wt.Commit("test", &gogit.CommitOptions{
		AllowEmptyCommits: true,
		Author:            &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("gittest: commit: %v", err)
	}
	return h.String()
}

// CommitObjects writes a commit straight into the object store of the
// repository at dir (made if it is not one) and returns it. files are regular
// files; links are symbolic links, path → target, stored with the target
// exactly as given, which is what the git program stores. (A link added
// through a go-git working tree is not: an absolute target is rewritten
// relative to the repository.) The commit has no parent and the working tree
// is not touched.
func CommitObjects(t testing.TB, dir string, files, links map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("gittest: %v", err)
	}
	repo, err := gogit.PlainOpen(dir)
	if errors.Is(err, gogit.ErrRepositoryNotExists) {
		repo, err = gogit.PlainInit(dir, false)
	}
	if err != nil {
		t.Fatalf("gittest: open %s: %v", dir, err)
	}
	store := func(typ plumbing.ObjectType, write func(plumbing.EncodedObject) error) plumbing.Hash {
		t.Helper()
		obj := repo.Storer.NewEncodedObject()
		obj.SetType(typ)
		if err := write(obj); err != nil {
			t.Fatalf("gittest: encode: %v", err)
		}
		h, err := repo.Storer.SetEncodedObject(obj)
		if err != nil {
			t.Fatalf("gittest: store: %v", err)
		}
		return h
	}
	blob := func(content string) plumbing.Hash {
		return store(plumbing.BlobObject, func(o plumbing.EncodedObject) error {
			w, err := o.Writer()
			if err != nil {
				return err
			}
			if _, err := w.Write([]byte(content)); err != nil {
				return err
			}
			return w.Close()
		})
	}

	// A directory is a map of names to either a blob's entry or a directory.
	type node struct {
		entry    *object.TreeEntry
		children map[string]*node
	}
	root := &node{children: map[string]*node{}}
	put := func(p string, mode filemode.FileMode, content string) {
		parts := strings.Split(p, "/")
		at := root
		for _, dirName := range parts[:len(parts)-1] {
			if at.children[dirName] == nil {
				at.children[dirName] = &node{children: map[string]*node{}}
			}
			at = at.children[dirName]
		}
		name := parts[len(parts)-1]
		at.children[name] = &node{entry: &object.TreeEntry{Name: name, Mode: mode, Hash: blob(content)}}
	}
	for p, content := range files {
		put(p, filemode.Regular, content)
	}
	for p, target := range links {
		put(p, filemode.Symlink, target)
	}
	var write func(n *node) plumbing.Hash
	write = func(n *node) plumbing.Hash {
		tree := &object.Tree{}
		for name, child := range n.children {
			if child.entry != nil {
				tree.Entries = append(tree.Entries, *child.entry)
				continue
			}
			tree.Entries = append(tree.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: write(child)})
		}
		sort.Sort(object.TreeEntrySorter(tree.Entries))
		return store(plumbing.TreeObject, tree.Encode)
	}
	who := object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}
	commit := &object.Commit{Author: who, Committer: who, Message: "test", TreeHash: write(root)}
	return store(plumbing.CommitObject, commit.Encode).String()
}

// Synced records sha as the last synced commit of repository repoID, as the
// sync's own transaction does (git_repos.last_sha).
func Synced(t testing.TB, db *sql.DB, repoID, sha string) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `UPDATE git_repos SET last_sha = ? WHERE id = ?`, sha, repoID)
	if err != nil {
		t.Fatalf("gittest: record the synced commit of %s: %v", repoID, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("gittest: repository %s has no row to record a synced commit on", repoID)
	}
}

// CommitSynced is Commit followed by Synced: the files are in the clone at
// dir, and the database says that commit is the one last synced.
func CommitSynced(t testing.TB, db *sql.DB, repoID, dir string, files map[string]string) string {
	t.Helper()
	sha := Commit(t, dir, files)
	Synced(t, db, repoID, sha)
	return sha
}
