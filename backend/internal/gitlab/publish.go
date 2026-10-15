package gitlab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"gopkg.in/yaml.v3"
)

// PublishRequest carries the fields from POST /api/v1/schedules/publish.
type PublishRequest struct {
	FilePath      string `json:"filePath"`
	Content       string `json:"content"`
	CommitMessage string `json:"commitMessage"`
	New           bool   `json:"new"`
	// RepoID is the repository to publish to (2.4.0, GR-24). Empty, as every
	// request was before a repository was a row, is Global's.
	RepoID string `json:"repoId,omitempty"`
}

// PublishResult is the outcome of a successful publish.
type PublishResult struct {
	CommitSHA string
	Branch    string
}

// PreconditionError is returned when a publish is not based on what the
// repository's branch is now: the If-Match base_sha, the clone's HEAD (the last
// sync) and the branch's head on the remote must all be one commit.
type PreconditionError struct {
	BaseSHA    string // the SHA the client sent
	CurrentSHA string // the branch's head on the remote, fetched for this publish
	Diff       string // unified diff for the UI
	// Unsynced: the remote has moved since the last sync, so the clone is
	// behind it and nothing can be published until a sync has brought the
	// clone up. The handler starts one.
	Unsynced bool
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("precondition failed: base_sha %s != current %s", e.BaseSHA, e.CurrentSHA)
}

// Publish validates, commits, and pushes the YAML to GitLab.
// baseSHA is from the If-Match header (required). actor is the authenticated user's email.
//
// Thread-safety: a publish waits in the one queue every sync and publish of
// every repository waits in (Service.enter, GR-11), so it never works in a
// clone while a sync is fetching into it or resetting it. Until 2.4.0 it held a
// mutex that a running sync did not hold.
//
// What a publish is based on (2.4.0; present defects 30 and 32, the owner's
// decisions of 2026-10-10 and 2026-10-12). It FETCHES the branch first, and
// goes ahead only when the caller's base, the clone's HEAD and the branch's
// head on the remote are one commit. It pushes WITHOUT force. Until 2.4.0 it
// compared the base with the clone alone, which is the last sync, and pushed
// with force: a commit somebody had pushed since the last sync was discarded
// from the branch, in silence. And a push that delivers nothing is a failure,
// which leaves no commit on the clone: the remote reporting "already up to
// date" after a commit was made means the commit went nowhere, and it was
// answered as a success.
//
// The fetch and the push are each bounded as a sync's transfers are
// (Service.transfer, CRONOMICON_GIT_FETCH_TIMEOUT).
// PublishTarget describes what a publish would write, for the caller to
// authorize (GC-9). The gitlab package knows how to read a definition file; it
// does not know who the caller is, so it reports facts and the route decides.
type PublishTarget struct {
	// Repo is the repository the publish names, and Agency the agency it
	// belongs to (GR-24). NoSuchRepo: there is no such repository, and nothing
	// else here is set; the route decides who may be told so.
	Repo       string
	Agency     string
	NoSuchRepo bool
	// Kind is "job", "schedule" or "workflow", from the directory the path is in.
	Kind string
	// Name and NewScope come from the INCOMING content. NewParsed is false when
	// the content is not a readable job document — the caller must then treat
	// the scope as unknown, never as empty.
	Name      string
	NewScope  string
	NewParsed bool
	// OldExists reports a file already at that path in the clone; OldScope and
	// OldParsed describe it the same way.
	OldExists bool
	OldScope  string
	OldParsed bool
}

// PublishAuthorizer returns "" to allow a publish, or the refusal to send as a
// 403. It is responsible for its own audit row.
type PublishAuthorizer func(r *http.Request, t PublishTarget) string

// publishTarget reads the facts a PublishAuthorizer needs. The path must already
// have passed validatePublishPath.
func (s *Service) publishTarget(req PublishRequest) PublishTarget {
	cleaned := filepath.Clean(req.FilePath)
	t := PublishTarget{Kind: "job"}
	if validatePublishPath(req.FilePath) != nil {
		// Never join an unvalidated path into the clone, whoever the caller is:
		// report a file whose scope is unknown, which no departmental caller
		// can be authorized to replace.
		t.OldExists = true
		return t
	}
	switch {
	case strings.HasPrefix(cleaned, "schedules/"):
		t.Kind = "schedule"
	case strings.HasPrefix(cleaned, "workflows/"):
		t.Kind = "workflow"
	}
	if t.Kind != "job" {
		return t
	}
	t.Name, t.NewScope, t.NewParsed = jobNameAndScope([]byte(req.Content))
	// Never through a symbolic link (publishPath): a path that is one, or
	// passes through one, is reported as a file that is there and whose scope
	// is unknown, which no departmental caller can be authorized to replace,
	// and nothing of what it points to is read.
	abs, exists, perr := publishPath(s.cloneDir, cleaned)
	if perr != nil {
		t.OldExists = true
		return t
	}
	if !exists {
		return t
	}
	if old, err := os.ReadFile(abs); err == nil { //nolint:gosec // abs is inside the clone and no link (publishPath)
		t.OldExists = true
		_, t.OldScope, t.OldParsed = jobNameAndScope(old)
	} else if !os.IsNotExist(err) {
		// Unreadable is not "absent": report a file whose scope is unknown, so
		// the caller fails closed instead of treating the path as free.
		t.OldExists = true
	}
	return t
}

// jobNameAndScope reads metadata.name and spec.scope from a job document.
func jobNameAndScope(content []byte) (name, scope string, ok bool) {
	var doc JobYAML
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return "", "", false
	}
	return strings.TrimSpace(doc.Metadata.Name), strings.TrimSpace(doc.Spec.Scope), true
}

func (s *Service) Publish(ctx context.Context, req PublishRequest, baseSHA, actor string) (*PublishResult, error) {
	ctx, release, err := s.enter(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish not run: %w", err)
	}
	defer release()
	s.installGuardedGitTransport() // SU-7/SU-8: guard go-git http(s) egress (once)

	// PP-B3: never join an untrusted path into the clone — re-validate here so the
	// guard holds even for a caller that bypasses the handler. Maps to 422.
	if err := validatePublishPath(req.FilePath); err != nil {
		return nil, &ValidationFailedError{
			Errors:  []ValidationError{{Field: "filePath", Message: err.Error()}},
			Message: err.Error(),
		}
	}

	// And never through a symbolic link: the path is read (for the diff of a
	// refusal) and written below, in the clone, and a link committed to the
	// repository would take either out of it (publishPath). Asked here, inside
	// the queue, so that no sync changes the clone between the question and
	// the write.
	if _, _, err := publishPath(s.cloneDir, req.FilePath); err != nil {
		return nil, &ValidationFailedError{
			Errors:  []ValidationError{{Field: "filePath", Message: err.Error()}},
			Message: err.Error(),
		}
	}

	// Resolve the GitOps branch fresh (env → DB → "main"); we push to the same
	// branch sync reads from (V1.1-10).
	branch := s.writeBranch(ctx)

	// 1. Validate the YAML content (T10).
	valErrs, err := validateYAMLBytes(req.FilePath, []byte(req.Content))
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	if len(valErrs) > 0 {
		// Return as a structured error — caller wraps in 422.
		msgs := make([]string, len(valErrs))
		for i, e := range valErrs {
			msgs[i] = e.Error()
		}
		return nil, &ValidationFailedError{Errors: valErrs, Message: strings.Join(msgs, "; ")}
	}

	// GR-10, as a sync asks it: an agency's repository on a host that is no
	// longer allowed is not pushed to.
	if err := s.checkURL(ctx); err != nil {
		return nil, err
	}

	// 2. Open the clone (must exist — sync must have run first or clone is empty).
	repo, err := gogit.PlainOpen(s.cloneDir)
	if err != nil {
		return nil, fmt.Errorf("open clone (run a sync first): %w", err)
	}

	// 3. What the branch is on the remote NOW, and the If-Match precondition
	// (A2/D2/D13) against it.
	remoteSHA, err := s.fetchBranchHead(ctx, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("git fetch: %w", err)
	}
	cloneRef, err := repo.Head()
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	cloneSHA := cloneRef.Hash().String()
	if baseSHA != remoteSHA || cloneSHA != remoteSHA {
		diff, _ := s.buildDiff(repo, req.FilePath, req.Content)
		return nil, &PreconditionError{
			BaseSHA:    baseSHA,
			CurrentSHA: remoteSHA,
			Diff:       diff,
			Unsynced:   cloneSHA != remoteSHA,
		}
	}

	// 4. Write the file into the working tree.
	absPath := filepath.Join(s.cloneDir, req.FilePath)
	if err := os.MkdirAll(filepath.Dir(absPath), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(absPath, []byte(req.Content), 0o640); err != nil {
		return nil, fmt.Errorf("write file: %w", err)
	}

	// 5. Stage and commit.
	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("worktree: %w", err)
	}
	if _, err := wt.Add(req.FilePath); err != nil {
		return nil, fmt.Errorf("git add: %w", err)
	}

	msg := req.CommitMessage
	if msg == "" {
		msg = fmt.Sprintf("cronomicon: update %s", req.FilePath)
	}

	authorName, authorEmail := s.publishAuthor(ctx)
	commit, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  time.Now().UTC(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}

	// 6. Push to GitLab, explicitly targeting the configured branch. Not
	// forced: the branch was fetched a moment ago and this commit is on top of
	// it, so the push is a fast-forward unless somebody pushed in between, and
	// then it is refused by the remote and nothing of theirs is lost.
	pushOpts := &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch)),
		},
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
		// After a commit, this is not a success: the remote took nothing.
		perr = errors.New("the remote had nothing to receive, so the commit was not delivered")
	}
	if perr != nil {
		// The commit is not left on the clone: the next sync would reset it
		// away in any case, and until then the clone would be ahead of the
		// branch and every publish refused.
		if uerr := takeBackCommit(repo, wt, cloneRef); uerr != nil {
			s.logError("publish: the push failed and the commit could not be taken back off the clone; the next sync resets it",
				"repo_id", s.repo(), "err", uerr)
		}
		return nil, fmt.Errorf("git push: %w", perr)
	}

	return &PublishResult{
		CommitSHA: commit.String(),
		Branch:    branch,
	}, nil
}

// ProtectedBranchError is a publish's push refused because the branch it
// writes to is protected and the connection's token may not push to it
// (2.4.0; the connection test's push handshake cannot see that: it shows at
// the first push). GitLab protects a project's default branch by default, and
// only Maintainers may push to it.
type ProtectedBranchError struct {
	Branch string
	Remote string // the remote's own words
}

func (e *ProtectedBranchError) Error() string {
	return "the repository refused the push to its branch " + e.Branch + ": the branch is protected, and the connection's token may not push to it" +
		" (the remote said: " + e.Remote + ")"
}

// pushHearingRemote pushes, keeping what the remote says while it receives
// (the "remote:" lines GitLab, GitHub and Gitea send) in said: the reason a
// push is refused is there, and not in the error.
func pushHearingRemote(ctx context.Context, repo *gogit.Repository, opts *gogit.PushOptions, said *strings.Builder) error {
	o := *opts
	o.Progress = said
	return repo.PushContext(ctx, &o)
}

// protectedBranchRefusal is err as a *ProtectedBranchError when the remote's
// words (or the error's) say the branch is protected; err otherwise.
func protectedBranchRefusal(err error, remoteSaid, branch string) error {
	if err == nil || errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return err
	}
	for _, text := range []string{remoteSaid, err.Error()} {
		if strings.Contains(strings.ToLower(text), "protected branch") {
			return &ProtectedBranchError{Branch: branch, Remote: lastRemoteLine(text, "protected branch")}
		}
	}
	return err
}

// lastRemoteLine is the line of text that says needle, trimmed.
func lastRemoteLine(text, needle string) string {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' })
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(lines[i]), needle) {
			return strings.TrimSpace(lines[i])
		}
	}
	return strings.TrimSpace(text)
}

// The author of a publish commit when the repository's connection names none:
// what the connection's columns default to (migration 1310).
const (
	defaultBotName  = "cronomicon-bot"
	defaultBotEmail = "cronomicon-bot@cronomicon.io"
)

// publishAuthor is the author of this repository's publish commits: the bot
// name and email of its connection (git_repos.bot_name, bot_email), which the
// connection's form has always shown and stored. Until 2.4.0 nothing read
// them, and every publish commit was written as "Cronomicon Scheduler
// <cronomicon-bot@cronomicon.internal>" (present defect 20; the owner's
// decision of 2026-10-09). A connection that has no row yet, or an empty
// value, gives the columns' defaults.
func (s *Service) publishAuthor(ctx context.Context) (name, email string) {
	var n, e sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT bot_name, bot_email FROM git_repos WHERE id = ?`, s.repo()).Scan(&n, &e)
	name, email = strings.TrimSpace(n.String), strings.TrimSpace(e.String)
	if name == "" {
		name = defaultBotName
	}
	if email == "" {
		email = defaultBotEmail
	}
	return name, email
}

// errPublishThroughLink is publishPath's refusal.
var errPublishThroughLink = errors.New("filePath is, or passes through, a symbolic link in the repository: " +
	"a publish does not read or write through a link. Publish the file the link points to, or replace the link in Git")

// publishPath is where a publish reads and writes a file of the repository:
// the path inside the clone, and whether a file is there. It refuses a path
// any part of which is a SYMBOLIC LINK, the file itself or a directory on the
// way to it (2.4.0; found by the review of Phase R6).
//
// The path a publish names is held to the repository's directories as a string
// (validatePublishPath). A file of the repository can be a link, committed by
// whoever commits to the repository, and reading or writing it by its path
// follows the link: out of the clone, to any file the server can read or
// write. The diff of a refused publish then carried that file's contents, and
// a publish on the right base overwrote it (the clone's own .git/config, in
// the test that found it). A sync refuses a link that leads out of the
// repository (containedReader) and follows one that stays inside; a publish
// follows none: writing through a link changes a file other than the one that
// was named.
//
// relPath must have passed validatePublishPath.
func publishPath(cloneDir, relPath string) (abs string, exists bool, err error) {
	abs = cloneDir
	parts := strings.Split(filepath.ToSlash(filepath.Clean(relPath)), "/")
	for i, part := range parts {
		abs = filepath.Join(abs, part)
		fi, lerr := os.Lstat(abs)
		if os.IsNotExist(lerr) {
			// Nothing here, so nothing below it either: a new file, and new
			// directories on the way to it.
			return filepath.Join(cloneDir, filepath.Clean(relPath)), false, nil
		}
		if lerr != nil {
			return "", false, lerr
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", false, errPublishThroughLink
		}
		last := i == len(parts)-1
		if !last && !fi.IsDir() {
			return "", false, fmt.Errorf("filePath passes through %s, which is a file", filepath.ToSlash(filepath.Join(parts[:i+1]...)))
		}
		if last && !fi.Mode().IsRegular() {
			return "", false, fmt.Errorf("filePath names something in the repository that is not a file")
		}
	}
	return abs, true, nil
}

// takeBackCommit puts a clone back where a publish found it, after a push that
// failed: the branch HEAD was on, at the commit it was at, with HEAD on it, and
// the index and the working tree reset hard to that commit (which removes the
// file the publish added). The branch and HEAD are set as a sync sets them,
// and not left to the reset, so that this holds whatever state the failed push
// left the clone's refs in.
func takeBackCommit(repo *gogit.Repository, wt *gogit.Worktree, was *plumbing.Reference) error {
	if was.Name().IsBranch() {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(was.Name(), was.Hash())); err != nil {
			return err
		}
		if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, was.Name())); err != nil {
			return err
		}
	}
	return wt.Reset(&gogit.ResetOptions{Commit: was.Hash(), Mode: gogit.HardReset})
}

// fetchBranchHead fetches the connection's branch from the remote, bounded as
// a sync's fetch is, and returns its head there. The clone's working tree, its
// HEAD and its local branch are not touched: only the remote-tracking ref
// moves, as a sync's fetch moves it.
func (s *Service) fetchBranchHead(ctx context.Context, repo *gogit.Repository, branch string) (string, error) {
	refSpec := gogitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	if err := s.pointOriginAt(repo, refSpec); err != nil {
		return "", err
	}
	opts := &gogit.FetchOptions{RemoteName: "origin", Force: true, RefSpecs: []gogitconfig.RefSpec{refSpec}}
	if auth := s.gitAuth(); auth != nil {
		opts.Auth = auth
	}
	err := s.transfer(ctx, func(tctx context.Context) error {
		if ferr := repo.FetchContext(tctx, opts); ferr != gogit.NoErrAlreadyUpToDate {
			return ferr
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true)
	if err != nil {
		return "", fmt.Errorf("the branch %q is not on the remote: %w", branch, err)
	}
	return ref.Hash().String(), nil
}

// ValidationFailedError wraps validation errors for 422 responses.
type ValidationFailedError struct {
	Errors  []ValidationError
	Message string
}

func (e *ValidationFailedError) Error() string { return e.Message }

// buildDiff produces a simple unified diff between the current file on disk and
// the incoming content, for the 412 error body (A2/D13).
func (s *Service) buildDiff(repo *gogit.Repository, relPath, newContent string) (string, error) {
	// PP-B3: defense-in-depth — refuse to read an out-of-tree path for the diff.
	if err := validatePublishPath(relPath); err != nil {
		return "", err
	}
	absPath, exists, err := publishPath(s.cloneDir, relPath)
	if err != nil {
		return "", err
	}
	var existingBytes []byte
	if exists {
		existingBytes, err = os.ReadFile(absPath) //nolint:gosec // absPath is inside the clone and no link (publishPath)
	} else {
		err = os.ErrNotExist
	}
	if err != nil {
		if os.IsNotExist(err) {
			// New file — diff is the entire content.
			var sb strings.Builder
			sb.WriteString("--- /dev/null\n")
			sb.WriteString("+++ b/" + relPath + "\n")
			for line := range strings.SplitSeq(newContent, "\n") {
				sb.WriteString("+" + line + "\n")
			}
			return sb.String(), nil
		}
		return "", err
	}

	// Simple line-by-line unified diff.
	existing := strings.Split(string(existingBytes), "\n")
	incoming := strings.Split(newContent, "\n")
	return simpleDiff(relPath, existing, incoming), nil
}

// simpleDiff produces a minimal unified diff header suitable for the 412 error body.
func simpleDiff(path string, existing, incoming []string) string {
	var sb strings.Builder
	sb.WriteString("--- a/" + path + "\n")
	sb.WriteString("+++ b/" + path + "\n")

	// Very simple line diff: show removed/added context.
	maxLen := max(len(incoming), len(existing))
	for i := range maxLen {
		e := ""
		if i < len(existing) {
			e = existing[i]
		}
		n := ""
		if i < len(incoming) {
			n = incoming[i]
		}
		if e != n {
			if i < len(existing) {
				sb.WriteString(fmt.Sprintf("-%s\n", e))
			}
			if i < len(incoming) {
				sb.WriteString(fmt.Sprintf("+%s\n", n))
			}
		}
	}
	return sb.String()
}

// RecordPush inserts a schedule_pushes audit row and an activity entry.
// status is "success" or "failed". actor is the authenticated user's email.
func (s *Service) RecordPush(ctx context.Context, actor, filePath, baseSHA, newSHA, status, errMsg string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO schedule_pushes(repo_id, at, actor, schedule_file, base_sha, new_sha, status, trace_id, details, created_at)
		VALUES(?,?,?,?,?,?,?,NULL,?,?)`,
		s.repo(), now, actor, filePath,
		nullStr(baseSHA), nullStr(newSHA),
		status,
		nullStr(errMsg),
		now)
	if err != nil {
		return fmt.Errorf("insert schedule_pushes: %w", err)
	}

	if status == "success" {
		// Shares `now` with the schedule_pushes row above so the audit pair reads
		// as one event rather than two timestamps apart.
		_ = auditlog.WriteActivity(ctx, s.db, auditlog.ActivityParams{
			At:           now,
			Kind:         "push",
			Outcome:      "success",
			Actor:        actor,
			ScheduleFile: filePath,
			Summary:      fmt.Sprintf("Published %s — commit %s", filePath, newSHA),
		})
	}
	return nil
}
