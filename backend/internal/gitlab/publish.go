package gitlab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// PublishRequest carries the fields from POST /api/v1/schedules/publish.
type PublishRequest struct {
	FilePath      string `json:"filePath"`
	Content       string `json:"content"`
	CommitMessage string `json:"commitMessage"`
	New           bool   `json:"new"`
}

// PublishResult is the outcome of a successful publish.
type PublishResult struct {
	CommitSHA string
	Branch    string
}

// PreconditionError is returned when the If-Match base_sha doesn't match the current HEAD.
type PreconditionError struct {
	BaseSHA    string // stale SHA the client sent
	CurrentSHA string // current HEAD
	Diff       string // unified diff for the UI
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("precondition failed: base_sha %s != current %s", e.BaseSHA, e.CurrentSHA)
}

// Publish validates, commits, and pushes the YAML to GitLab.
// baseSHA is from the If-Match header (required). actor is the authenticated user's email.
//
// Thread-safety: callers must serialize publishes using a DB row lock or external mutex;
// the in-process single-instance invariant (§1) means a sync.Mutex in the service is enough.
func (s *Service) Publish(ctx context.Context, req PublishRequest, baseSHA, actor string) (*PublishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.installGuardedGitTransport() // SU-7/SU-8: guard go-git http(s) egress (once)

	// PP-B3: never join an untrusted path into the clone — re-validate here so the
	// guard holds even for a caller that bypasses the handler. Maps to 422.
	if err := validatePublishPath(req.FilePath); err != nil {
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

	// 2. Open the clone (must exist — sync must have run first or clone is empty).
	repo, err := gogit.PlainOpen(s.cloneDir)
	if err != nil {
		return nil, fmt.Errorf("open clone (run a sync first): %w", err)
	}

	// 3. Check If-Match precondition (A2/D2/D13).
	currentSHA, err := s.headSHA(repo)
	if err != nil {
		return nil, fmt.Errorf("read HEAD: %w", err)
	}
	if baseSHA != currentSHA {
		diff, _ := s.buildDiff(repo, req.FilePath, req.Content)
		return nil, &PreconditionError{
			BaseSHA:    baseSHA,
			CurrentSHA: currentSHA,
			Diff:       diff,
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
		msg = fmt.Sprintf("amadeus: update %s", req.FilePath)
	}

	commit, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{
			Name:  "Cronomicon Scheduler",
			Email: "amadeus-bot@amadeus.internal",
			When:  time.Now().UTC(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("git commit: %w", err)
	}

	// 6. Push to GitLab, explicitly targeting the configured branch.
	pushOpts := &gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs: []gogitconfig.RefSpec{
			gogitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:refs/heads/%s", branch, branch)),
		},
	}
	if s.token != "" {
		pushOpts.Auth = &gogithttp.BasicAuth{Username: "oauth2", Password: s.token}
	}
	if err := repo.PushContext(ctx, pushOpts); err != nil && err != gogit.NoErrAlreadyUpToDate {
		return nil, fmt.Errorf("git push: %w", err)
	}

	return &PublishResult{
		CommitSHA: commit.String(),
		Branch:    branch,
	}, nil
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
	absPath := filepath.Join(s.cloneDir, relPath)
	existingBytes, err := os.ReadFile(absPath)
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
		INSERT INTO schedule_pushes(at, actor, schedule_file, base_sha, new_sha, status, trace_id, details, created_at)
		VALUES(?,?,?,?,?,?,NULL,?,?)`,
		now, actor, filePath,
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
