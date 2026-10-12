package execspec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// A script kept as a file is read from the commit's objects, not from the
// clone's working tree (present defect 27, 2.4.0).
//
// Until 2.4.0 dispatch and the two routes that show a body read the working
// tree. A sync resets that tree in place: go-git truncates each changed file
// and writes it again, and a dispatch is not an operation on the clone and
// does not wait behind a sync. A run that started in that moment was handed an
// empty or cut-off script and ran it. A commit's objects never change, so
// nothing here can be half written; and the commit read at is the one the
// definitions in the database were synced from (git_repos.last_sha, written in
// the sync's own transaction), where the working tree is ahead of them from
// the reset until that transaction commits.
//
// Nothing falls back to the working tree. A file that cannot be read from the
// objects is an error with a reason, and the run fails with it.

// maxLinkHops bounds the symbolic links followed for one path, as the kernel
// bounds them for a working-tree read.
const maxLinkHops = 40

// CommitReadError is a file that could not be read from a commit's objects.
// Its text names the repository-relative path and the commit and never a path
// on the server, so a caller may show it.
type CommitReadError struct {
	Path   string
	Reason string
}

func (e *CommitReadError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

func notRead(relPath, format string, a ...any) error {
	return &CommitReadError{Path: relPath, Reason: fmt.Sprintf(format, a...)}
}

// ReadCommitFile reads relPath as commit sha of the repository cloned at
// cloneDir holds it. It keeps what the working-tree reader (SafeReadRepoFile)
// allowed and refused: a path is relative and stays inside the repository; a
// symbolic link committed in the repository is followed, inside the commit's
// tree, and one that points outside it is refused.
//
// A path that goes through a submodule is read from the SUBMODULE's own
// objects, at the commit the repository pins for it (the gitlink), one hop for
// each nested submodule. A repository's commit holds only that gitlink, not
// the submodule's files. A submodule that has not been fetched, or that does
// not hold the pinned commit (its last update failed), is an error.
//
// limit is as for SafeReadRepoFile: at most limit bytes when it is above zero,
// with truncated saying whether there were more; the whole file otherwise.
func ReadCommitFile(cloneDir, sha, relPath string, limit int64) (data []byte, truncated bool, err error) {
	clean := filepath.ToSlash(filepath.Clean(relPath))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) || filepath.IsAbs(relPath) {
		return nil, false, notRead(relPath, "unsafe scriptPath")
	}
	if !plumbing.IsHash(sha) {
		return nil, false, notRead(relPath, "there is no commit to read it at (%q is not a commit)", sha)
	}
	repo, err := gogit.PlainOpen(cloneDir)
	if err != nil {
		// Not the directory: it is a path on the server.
		return nil, false, notRead(relPath, "the repository has no clone on this server to read it from")
	}
	hops := 0
	return readInCommit(repo, plumbing.NewHash(sha), clean, relPath, limit, &hops)
}

// readInCommit reads rel (clean, slash-separated) at commit h of repo. asked is
// the path as the caller gave it, for errors.
func readInCommit(repo *gogit.Repository, h plumbing.Hash, rel, asked string, limit int64, hops *int) ([]byte, bool, error) {
	commit, err := repo.CommitObject(h)
	if err != nil {
		return nil, false, notRead(asked, "commit %s is not in the clone", short(h))
	}
	root, err := commit.Tree()
	if err != nil {
		return nil, false, notRead(asked, "commit %s has no readable tree", short(h))
	}

	parts := strings.Split(rel, "/")
	tree := root
	for i := 0; i < len(parts); i++ {
		entry, ok := treeEntry(tree, parts[i])
		if !ok {
			return nil, false, notRead(asked, "not in commit %s", short(h))
		}
		last := i == len(parts)-1
		here := strings.Join(parts[:i+1], "/")
		switch entry.Mode {
		case filemode.Dir:
			if last {
				return nil, false, notRead(asked, "a directory in commit %s, not a file", short(h))
			}
			if tree, err = object.GetTree(repo.Storer, entry.Hash); err != nil {
				return nil, false, notRead(asked, "commit %s has no readable tree at %s", short(h), here)
			}

		case filemode.Submodule:
			if last {
				return nil, false, notRead(asked, "a submodule in commit %s, not a file", short(h))
			}
			sub, name, err := openSubmodule(repo, root, here)
			if err != nil {
				return nil, false, notRead(asked, "%s", err)
			}
			if _, err := sub.CommitObject(entry.Hash); err != nil {
				return nil, false, notRead(asked,
					"submodule %q does not hold commit %s, which the repository pins for it: it was not updated by the last sync",
					name, short(entry.Hash))
			}
			// A link inside the submodule is followed inside the submodule: its
			// tree is the root from here on.
			return readInCommit(sub, entry.Hash, strings.Join(parts[i+1:], "/"), asked, limit, hops)

		case filemode.Symlink:
			if *hops++; *hops > maxLinkHops {
				return nil, false, notRead(asked, "too many symbolic links")
			}
			target, _, err := readBlob(repo, entry.Hash, 4096)
			if err != nil {
				return nil, false, notRead(asked, "the symbolic link at %s could not be read", here)
			}
			to := strings.TrimSpace(string(target))
			if to == "" || path.IsAbs(to) || filepath.IsAbs(to) {
				return nil, false, notRead(asked, "escapes repository root: the symbolic link at %s points outside it", here)
			}
			next := path.Clean(path.Join(append([]string{path.Dir(here), to}, parts[i+1:]...)...))
			if next == "." || next == ".." || strings.HasPrefix(next, "../") {
				return nil, false, notRead(asked, "escapes repository root: the symbolic link at %s points outside it", here)
			}
			parts, tree, i = strings.Split(next, "/"), root, -1

		default: // a file: regular, executable, or the deprecated group-writable mode
			if !last {
				return nil, false, notRead(asked, "not in commit %s (%s is a file)", short(h), here)
			}
			data, truncated, err := readBlob(repo, entry.Hash, limit)
			if err != nil {
				return nil, false, notRead(asked, "its content could not be read from commit %s", short(h))
			}
			return data, truncated, nil
		}
	}
	return nil, false, notRead(asked, "not in commit %s", short(h))
}

func treeEntry(t *object.Tree, name string) (object.TreeEntry, bool) {
	for _, e := range t.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return object.TreeEntry{}, false
}

func readBlob(repo *gogit.Repository, h plumbing.Hash, limit int64) ([]byte, bool, error) {
	blob, err := repo.BlobObject(h)
	if err != nil {
		return nil, false, err
	}
	r, err := blob.Reader()
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	if limit <= 0 {
		data, err := io.ReadAll(r)
		return data, false, err
	}
	// One byte past the cap, so a file exactly at it is not called truncated.
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

// openSubmodule opens the object store of the submodule at subPath of repo.
// The name it is stored under is the one .gitmodules gives that path in the
// SAME commit (root is that commit's tree), not the one in the working tree.
func openSubmodule(repo *gogit.Repository, root *object.Tree, subPath string) (*gogit.Repository, string, error) {
	f, err := root.File(".gitmodules")
	if err != nil {
		return nil, "", fmt.Errorf("%s is a submodule that .gitmodules does not declare", subPath)
	}
	content, err := f.Contents()
	if err != nil {
		return nil, "", fmt.Errorf(".gitmodules could not be read")
	}
	modules := gogitconfig.NewModules()
	if err := modules.Unmarshal([]byte(content)); err != nil {
		return nil, "", fmt.Errorf(".gitmodules could not be parsed")
	}
	name := ""
	for _, m := range modules.Submodules {
		if path.Clean(m.Path) == subPath {
			name = m.Name
			break
		}
	}
	if name == "" {
		return nil, "", fmt.Errorf("%s is a submodule that .gitmodules does not declare", subPath)
	}
	st, err := repo.Storer.Module(name)
	if err != nil {
		return nil, "", fmt.Errorf("submodule %q has not been fetched", name)
	}
	sub, err := gogit.Open(st, nil)
	if err != nil {
		return nil, "", fmt.Errorf("submodule %q has not been fetched", name)
	}
	return sub, name, nil
}

func short(h plumbing.Hash) string { return h.String()[:12] }

// ReadSyncedFile reads relPath of repository repoID as its last synced commit
// holds it: the commit the definitions in the database came from
// (git_repos.last_sha). dirs says where the repository is cloned.
func ReadSyncedFile(ctx context.Context, db *sql.DB, dirs CloneDirs, repoID, relPath string, limit int64) ([]byte, bool, error) {
	var sha sql.NullString
	err := db.QueryRowContext(ctx, `SELECT last_sha FROM git_repos WHERE id = ?`, repoID).Scan(&sha)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read the last synced commit of repository %s: %w", repoID, err)
	}
	if sha.String == "" {
		return nil, false, notRead(relPath, "repository %s has not been synced: there is no commit to read it at", repoID)
	}
	return ReadCommitFile(dirs(repoID), sha.String, relPath, limit)
}
