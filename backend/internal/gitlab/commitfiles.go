package gitlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Several files in one commit (2.5.0 C10, IN-12).
//
// An assignment gives an agency a definition and what it needs as ONE commit
// into the agency's repository. It is a publish of several new files: the same
// queue, the same fetch first, the same push without force, and the commit
// taken back off the clone when anything after the first write fails.

// FileWrite is one file CommitFiles adds.
type FileWrite struct {
	Path       string // repository-relative, slash-separated
	Content    []byte
	Executable bool // committed with the executable bit, as it was in the origin
}

// PathsTakenError is a CommitFiles refused because files are already at some
// of its paths, in the clone as it is on the branch now. Nothing was written.
type PathsTakenError struct {
	Paths []string
}

func (e *PathsTakenError) Error() string {
	return "already in the repository: " + strings.Join(e.Paths, ", ")
}

// validateCommitPath holds a path CommitFiles writes to the repository: a
// relative path that stays inside it and is not in .git. Unlike a publish's,
// it may be in any directory: a script's body file and its project directory
// are wherever the origin repository keeps them.
func validateCommitPath(p string) error {
	if p == "" || strings.ContainsRune(p, '\\') || path.IsAbs(p) || filepath.IsAbs(p) {
		return fmt.Errorf("%q is not a repository-relative path", p)
	}
	clean := path.Clean(p)
	if clean != p || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%q is not a clean path inside the repository", p)
	}
	if first, _, _ := strings.Cut(clean, "/"); first == ".git" {
		return fmt.Errorf("%q is inside .git", p)
	}
	return nil
}

// CommitFiles writes files as one commit on the branch this repository syncs
// and pushes it. Every path must be free: a file already there refuses the
// whole commit (*PathsTakenError), with nothing written. The clone must be at
// the branch's head on the remote (*PreconditionError with Unsynced, and the
// caller starts a sync, as a publish does).
func (s *Service) CommitFiles(ctx context.Context, files []FileWrite, msg string) (*PublishResult, error) {
	if len(files) == 0 {
		return nil, errors.New("nothing to commit")
	}
	seen := map[string]bool{}
	for _, f := range files {
		if err := validateCommitPath(f.Path); err != nil {
			return nil, &ValidationFailedError{Errors: []ValidationError{{Field: "path", Message: err.Error()}}, Message: err.Error()}
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("%s is written twice", f.Path)
		}
		seen[f.Path] = true
	}

	ctx, release, err := s.enter(ctx)
	if err != nil {
		return nil, fmt.Errorf("commit not made: %w", err)
	}
	defer release()
	s.installGuardedGitTransport()

	branch := s.writeBranch(ctx)
	if err := s.checkURL(ctx); err != nil {
		return nil, err
	}
	repo, err := gogit.PlainOpen(s.cloneDir)
	if err != nil {
		return nil, fmt.Errorf("open clone (run a sync first): %w", err)
	}
	remoteSHA, err := s.fetchBranchHead(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("git fetch: %w", err)
	}
	cloneRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	if cloneSHA := cloneRef.Hash().String(); cloneSHA != remoteSHA {
		return nil, &PreconditionError{BaseSHA: cloneSHA, CurrentSHA: remoteSHA, Unsynced: true}
	}

	// Inside the queue, so no sync changes the clone between this and the
	// write. Never through a link (publishPath).
	var taken []string
	abs := make([]string, len(files))
	for i, f := range files {
		a, exists, perr := publishPath(s.cloneDir, f.Path)
		if perr != nil || exists {
			taken = append(taken, f.Path)
			continue
		}
		abs[i] = a
	}
	if len(taken) > 0 {
		sort.Strings(taken)
		return nil, &PathsTakenError{Paths: taken}
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	written := 0
	undo := func(why error) error {
		for _, a := range abs[:written] {
			_ = os.Remove(a)
		}
		if uerr := takeBackCommit(repo, wt, cloneRef); uerr != nil {
			s.logError("commit files: could not put the clone back; the next sync resets it", "repo_id", s.repo(), "err", uerr)
		}
		return why
	}
	for i, f := range files {
		if err := os.MkdirAll(filepath.Dir(abs[i]), 0o750); err != nil {
			return nil, undo(fmt.Errorf("mkdir: %w", err))
		}
		mode := os.FileMode(0o640)
		if f.Executable {
			mode = 0o750
		}
		if err := os.WriteFile(abs[i], f.Content, mode); err != nil {
			return nil, undo(fmt.Errorf("write %s: %w", f.Path, err))
		}
		written++
		if _, err := wt.Add(f.Path); err != nil {
			return nil, undo(fmt.Errorf("git add %s: %w", f.Path, err))
		}
	}
	authorName, authorEmail := s.publishAuthor(ctx)
	commit, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: authorName, Email: authorEmail, When: time.Now().UTC()},
	})
	if err != nil {
		return nil, undo(fmt.Errorf("git commit: %w", err))
	}

	pushOpts := &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))},
	}
	if s.token != "" {
		pushOpts.Auth = &gogithttp.BasicAuth{Username: "oauth2", Password: s.token}
	}
	if s.beforePush != nil {
		s.beforePush()
	}
	var remoteSaid strings.Builder
	perr := s.transfer(ctx, func(tctx context.Context) error { return pushHearingRemote(tctx, repo, pushOpts, &remoteSaid) })
	perr = protectedBranchRefusal(perr, remoteSaid.String(), branch)
	if errors.Is(perr, gogit.NoErrAlreadyUpToDate) {
		perr = errors.New("the remote had nothing to receive, so the commit was not delivered")
	}
	if perr != nil {
		return nil, undo(fmt.Errorf("git push: %w", perr))
	}
	return &PublishResult{CommitSHA: commit.String(), Branch: branch}, nil
}

// CommitFile is one regular file of a commit (ReadCommitTree).
type CommitFile struct {
	Path       string
	Content    []byte
	Executable bool
}

// ReadCommitTree reads rel as commit sha of the repository cloned at cloneDir
// holds it: the file, or every file under the directory, in path order. What
// an assignment copies is files: a symbolic link or a submodule on the way or
// inside is refused, naming it, rather than followed or left out.
func ReadCommitTree(cloneDir, sha, rel string) ([]CommitFile, error) {
	if err := validateCommitPath(rel); err != nil {
		return nil, err
	}
	if !plumbing.IsHash(sha) {
		return nil, fmt.Errorf("%q is not a commit", sha)
	}
	repo, err := gogit.PlainOpen(cloneDir)
	if err != nil {
		return nil, errors.New("the repository has no clone on this server to read from")
	}
	commit, err := repo.CommitObject(plumbing.NewHash(sha))
	if err != nil {
		return nil, fmt.Errorf("commit %s is not in the clone", sha)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("commit %s has no readable tree", sha)
	}
	entry, err := tree.FindEntry(rel)
	if err != nil {
		return nil, fmt.Errorf("%s is not in commit %.12s", rel, sha)
	}
	switch entry.Mode {
	case filemode.Regular, filemode.Executable, filemode.Deprecated:
		f, err := tree.File(rel)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		data, err := f.Contents()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		return []CommitFile{{Path: rel, Content: []byte(data), Executable: entry.Mode == filemode.Executable}}, nil
	case filemode.Dir:
	default:
		return nil, fmt.Errorf("%s is a symbolic link or a submodule in the repository; an assignment copies files only", rel)
	}
	sub, err := tree.Tree(rel)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	var out []CommitFile
	walker := object.NewTreeWalker(sub, true, nil)
	defer walker.Close()
	for {
		name, e, err := walker.Next()
		if err != nil {
			break
		}
		p := rel + "/" + name
		switch e.Mode {
		case filemode.Dir:
			continue
		case filemode.Regular, filemode.Executable, filemode.Deprecated:
			f, err := sub.File(name)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			data, err := f.Contents()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			out = append(out, CommitFile{Path: p, Content: []byte(data), Executable: e.Mode == filemode.Executable})
		default:
			return nil, fmt.Errorf("%s is a symbolic link or a submodule in the repository; an assignment copies files only", p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
