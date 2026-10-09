package gitlab

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// Sync problems are rows, per repository (2.4.0, GR-30).
//
// A sync has always said what was wrong with the files it read: a file that
// does not validate, a reference that names nothing, a job that declares no
// scope, a key that is no longer read. The errors went into one text column of
// the sync's history row, joined by semicolons; the warnings went to the
// server's log and nowhere else. The administrators of the one repository were
// the people who could read both. An agency that keeps its own repository can
// read neither, and has to be told which file failed and why.
//
// Every error and every warning a sync produces is now also a row of
// git_sync_problems, for its repository, written in the sync's transaction.
// The rows are the repository's problems AS OF ITS LAST SYNC: what that sync
// did not report again is gone.
//
// The inbox carries one notice per repository whose last sync reported
// ERRORS. Not warnings: a warning is advice about a file that WAS synced, most
// repositories have some that stand for good (a job that declares no scope, a
// key that is no longer read), and a notice that never clears is one that is
// dismissed once and then hides the day a file stops syncing. The notice
// counts the warnings and quotes the errors.

const (
	problemError   = "error"
	problemWarning = "warning"
)

// syncProblem is one thing a sync has to say about one file (or, with no
// path, about the repository).
type syncProblem struct {
	path     string // repository-relative; "" for the repository as a whole
	kind     string // job | workflow | schedule | script | scope | repository
	severity string
	message  string
}

// problemSet collects the problems of the sync that is under way.
type problemSet struct {
	mu    sync.Mutex
	items []syncProblem
	seen  map[syncProblem]bool
}

func (p *problemSet) add(pr syncProblem) {
	if p == nil || pr.message == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen == nil {
		p.seen = map[syncProblem]bool{}
	}
	if p.seen[pr] {
		return
	}
	p.seen[pr] = true
	p.items = append(p.items, pr)
}

func (p *problemSet) all() []syncProblem {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]syncProblem(nil), p.items...)
}

// setProblems installs (or, with nil, removes) the collector of the sync that
// is under way.
func (s *Service) setProblems(p *problemSet) {
	s.problemsMu.Lock()
	s.problems = p
	s.problemsMu.Unlock()
}

// currentProblems is the collector of the sync that is under way, or nil.
func (s *Service) currentProblems() *problemSet {
	s.problemsMu.Lock()
	defer s.problemsMu.Unlock()
	return s.problems
}

// definitionDirs are the directories of a repository that hold definitions.
var definitionDirs = map[string]string{
	"jobs": "job", "workflows": "workflow", "schedules": "schedule", "scripts": "script", "inventory": "scope",
}

// kindOfPath says what kind of definition a file of a repository holds, from
// the directory it is in (or is: a problem can be about `inventory` itself).
func kindOfPath(path string) string {
	dir, _, _ := strings.Cut(path, "/")
	if kind, ok := definitionDirs[dir]; ok {
		return kind
	}
	return "repository"
}

// relToClone makes a path the repository's own: a path under the clone loses
// the clone's directory, anything else is kept as it is. The clone's directory
// is the server's business and is in no problem's text.
func (s *Service) relToClone(path string) string {
	if path == "" || s.cloneDir == "" {
		return filepath.ToSlash(path)
	}
	if rel, err := filepath.Rel(s.cloneDir, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

// scrub takes the server's directories out of a message: the clone's, and the
// directory the clones are kept in (git names it when a clone cannot be made).
func (s *Service) scrub(msg string) string {
	if s.cloneDir == "" {
		return msg
	}
	clone := filepath.Clean(s.cloneDir)
	msg = strings.ReplaceAll(msg, clone+string(filepath.Separator), "")
	msg = strings.ReplaceAll(msg, clone, "the repository")
	if cache := filepath.Dir(clone); cache != "." && cache != string(filepath.Separator) {
		msg = strings.ReplaceAll(msg, cache, "the clone cache")
	}
	return msg
}

// warningDetail is which of a warning's logged attributes go into its row.
// The log line carries more (an error's text, a repository's id): those are
// for whoever reads the server's log, and a row is read by the repository's
// agency.
var warningDetail = map[string]bool{
	"job": true, "script": true, "workflow": true, "schedule": true, "scope": true, "name": true,
	"host": true, "line": true, "field": true, "runner_tag": true, "executor": true,
	"submodule": true, "path": true, "problem": true, "spec": true, "project": true,
	"detail": true, "scopes": true, "value": true, "step": true, "subsystems": true,
}

// noteWarning records a warning the sync has just logged, when a sync is under
// way. It is called by logWarn, so that nothing a sync warns of can be left
// out of the rows by forgetting to add it in a second place.
func (s *Service) noteWarning(msg string, args []any) {
	set := s.currentProblems()
	if set == nil {
		return
	}
	var path, kind string
	var details []string
	for i := 0; i+1 < len(args); i += 2 {
		key, ok := args[i].(string)
		if !ok {
			continue
		}
		val := fmt.Sprint(args[i+1])
		switch key {
		case "source_path", "file":
			if path == "" {
				path = s.relToClone(val)
			}
			continue
		}
		if kind == "" {
			switch key {
			case "job", "script", "workflow", "schedule", "scope":
				kind = key
			}
		}
		if warningDetail[key] && val != "" {
			details = append(details, key+" "+val)
		}
	}
	if path != "" {
		if k := kindOfPath(path); k != "repository" || kind == "" {
			kind = k
		}
	}
	if kind == "" {
		kind = "repository"
	}
	text := strings.TrimPrefix(msg, "git sync: ")
	if len(details) > 0 {
		text += " (" + strings.Join(details, ", ") + ")"
	}
	set.add(syncProblem{path: path, kind: kind, severity: problemWarning, message: s.scrub(text)})
}

// pathInMessage finds the file an error is about when the error did not say
// so in a field of its own: several of the sync's errors are plain errors
// whose text begins with the file ("read jobs/x.yaml: …", "parse jobs/x.yaml:
// …", "inventory/x.ini:2: …"). It returns the file, repository-relative, and
// the text that follows it; ok is false when the text does not begin with a
// file of one of the definition directories.
func (s *Service) pathInMessage(msg string) (path, rest string, ok bool) {
	body := msg
	for _, verb := range []string{"read ", "parse "} {
		if strings.HasPrefix(body, verb) {
			body = strings.TrimPrefix(body, verb)
			break
		}
	}
	head, tail, found := strings.Cut(body, ": ")
	if !found {
		return "", "", false
	}
	head = s.relToClone(head)
	// "inventory/x.ini:2" carries a line.
	file, line := head, ""
	if i := strings.LastIndex(head, ":"); i > 0 {
		if _, err := strconv.Atoi(head[i+1:]); err == nil {
			file, line = head[:i], head[i+1:]
		}
	}
	if kindOfPath(file) == "repository" || strings.ContainsAny(file, " \t") {
		return "", "", false
	}
	if line != "" {
		tail = "line " + line + ": " + tail
	}
	return file, tail, true
}

// noteErrors records the errors of the sync that is under way: one row per
// file error the sync reports.
func (s *Service) noteErrors(errs []ValidationError) {
	set := s.currentProblems()
	if set == nil {
		return
	}
	for _, e := range errs {
		path := s.relToClone(e.File)
		msg := e.Message
		if path == "" {
			if p, rest, ok := s.pathInMessage(msg); ok {
				path, msg = p, rest
			}
		}
		if e.Line > 0 {
			msg = fmt.Sprintf("line %d: %s", e.Line, msg)
		}
		if e.Field != "" && !strings.Contains(msg, e.Field) {
			msg = e.Field + ": " + msg
		}
		set.add(syncProblem{path: path, kind: kindOfPath(path), severity: problemError, message: s.scrub(msg)})
	}
}

// execQuerier is *sql.DB or *sql.Tx.
type execQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// knownErrors reads the errors a repository's rows hold now, as a set.
func (s *Service) knownErrors(ctx context.Context, x execQuerier) (map[syncProblem]bool, error) {
	rows, err := x.QueryContext(ctx,
		`SELECT path, kind, message FROM git_sync_problems WHERE repo_id = ? AND severity = 'error'`, s.repo())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[syncProblem]bool{}
	for rows.Next() {
		p := syncProblem{severity: problemError}
		if err := rows.Scan(&p.path, &p.kind, &p.message); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// writeProblems makes the repository's rows equal to what this sync found, in
// the sync's transaction, and opens or resolves the repository's notice. A
// problem that was there at the last sync and is there again keeps the moment
// it was first seen.
func (s *Service) writeProblems(ctx context.Context, tx *sql.Tx, now string, set *problemSet) error {
	found := set.all()
	before, err := s.knownErrors(ctx, tx)
	if err != nil {
		return err
	}
	newError := false
	pass := db.NewID()
	for _, p := range found {
		if p.severity == problemError && !before[p] {
			newError = true
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO git_sync_problems (repo_id, path, kind, severity, message, first_seen, last_seen, pass)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (repo_id, path, kind, severity, message) DO UPDATE SET
				last_seen = excluded.last_seen, pass = excluded.pass`,
			s.repo(), p.path, p.kind, p.severity, p.message, now, now, pass); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM git_sync_problems WHERE repo_id = ? AND pass <> ?`, s.repo(), pass); err != nil {
		return err
	}
	return s.problemsNotice(ctx, tx, found, newError)
}

// noteFetchFailure records that the repository could not be fetched at all.
// There is no sync to write it in and nothing was read, so the rows of the
// last sync that did read the files are left as they are: they are still what
// is known of the files. The row goes when a sync next succeeds in fetching.
func (s *Service) noteFetchFailure(ctx context.Context, fetchErr error) {
	if s.db == nil {
		return
	}
	msg := fetchErr.Error()
	if s.repoURL != "" {
		// The URL is the connection's, and may carry a credential.
		msg = strings.ReplaceAll(msg, s.repoURL, "the repository")
	}
	msg = "the repository could not be fetched: " + s.scrub(msg)
	now := syncStamp()
	ctx = context.WithoutCancel(ctx)
	before, err := s.knownErrors(ctx, s.db)
	if err != nil {
		return // no table yet, or the database is going away: nothing to tell
	}
	this := syncProblem{kind: "repository", severity: problemError, message: msg}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM git_sync_problems WHERE repo_id = ? AND kind = 'repository' AND path = '' AND severity = 'error'
		   AND message LIKE 'the repository could not be fetched:%' AND message <> ?`, s.repo(), msg); err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO git_sync_problems (repo_id, path, kind, severity, message, first_seen, last_seen, pass)
		VALUES (?, '', 'repository', 'error', ?, ?, ?, 'fetch')
		ON CONFLICT (repo_id, path, kind, severity, message) DO UPDATE SET last_seen = excluded.last_seen`,
		s.repo(), msg, now, now); err != nil {
		return
	}
	all, err := s.storedProblems(ctx, s.db)
	if err != nil {
		return
	}
	_ = s.problemsNotice(ctx, s.db, all, !before[this])
}

// storedProblems reads every row a repository has.
func (s *Service) storedProblems(ctx context.Context, x execQuerier) ([]syncProblem, error) {
	rows, err := x.QueryContext(ctx,
		`SELECT path, kind, severity, message FROM git_sync_problems WHERE repo_id = ?`, s.repo())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []syncProblem
	for rows.Next() {
		var p syncProblem
		if err := rows.Scan(&p.path, &p.kind, &p.severity, &p.message); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	return all, rows.Err()
}

// clearProblems forgets a repository's problems and resolves its notice: its
// connection has no URL any more, so nothing will sync it again and what its
// last sync said describes files nobody is reading.
func (s *Service) clearProblems(ctx context.Context) {
	if s.db == nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM git_sync_problems WHERE repo_id = ?`, s.repo()); err != nil {
		return
	}
	_ = notices.Resolve(ctx, s.db, notices.KindGitSyncProblems, s.repo())
}

// problemsNotice opens the repository's notice when its last sync reported
// errors and resolves it when it reported none. One notice per repository,
// filed under the repository's agency; its subject is the repository's id.
//
// newError: an error is there that was not there before. The notice is then
// opened AFRESH, whoever dismissed it: a dismissal says "I have seen this",
// and nobody has seen an error that has only just appeared.
func (s *Service) problemsNotice(ctx context.Context, x execQuerier, found []syncProblem, newError bool) error {
	var errs []syncProblem
	warnings := 0
	for _, p := range found {
		if p.severity == problemError {
			errs = append(errs, p)
		} else {
			warnings++
		}
	}
	if len(errs) == 0 {
		return notices.Resolve(ctx, x, notices.KindGitSyncProblems, s.repo())
	}
	sort.SliceStable(errs, func(i, j int) bool {
		if errs[i].path != errs[j].path {
			return errs[i].path < errs[j].path
		}
		return errs[i].message < errs[j].message
	})
	plural := func(n int, one string) string {
		if n == 1 {
			return fmt.Sprintf("1 %s", one)
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The last sync of this repository reported %s and %s.", plural(len(errs), "error"), plural(warnings, "warning"))
	b.WriteString(" A file with an error was not synced: what it defines is as it was at the last sync that could read it, or is not there.")
	const shown = 5
	for i, p := range errs {
		if i == shown {
			fmt.Fprintf(&b, " And %d more.", len(errs)-shown)
			break
		}
		where := p.path
		if where == "" {
			where = "the repository"
		}
		fmt.Fprintf(&b, " %s: %s.", where, strings.TrimRight(p.message, "."))
	}
	b.WriteString(" The notice clears at the first sync that reports no error.")
	if newError {
		if err := notices.Resolve(ctx, x, notices.KindGitSyncProblems, s.repo()); err != nil {
			return err
		}
	}
	return notices.Upsert(ctx, x, notices.KindGitSyncProblems, notices.Finding{
		AgencyID: s.agency(),
		Subject:  s.repo(),
		Detail:   b.String(),
	})
}

// syncStamp is the time a sync stamps its rows with.
func syncStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// duplicateNames reports every definition of a kind whose name another file
// of the SAME repository has already taken. One definition of a name is used
// (the one read last) and the others are ignored: until 2.4.0 in silence, the
// sync a clean success.
func duplicateNames[T any](kind string, items []T, nameOf, pathOf func(T) string) []ValidationError {
	first := map[string]string{}
	var out []ValidationError
	for _, it := range items {
		name := strings.TrimSpace(nameOf(it))
		if name == "" {
			continue
		}
		path := pathOf(it)
		if prev, taken := first[name]; taken {
			out = append(out, ValidationError{
				File:  path,
				Field: "metadata.name",
				Message: fmt.Sprintf("the %s %q is defined in more than one file of this repository (also in %s): "+
					"one of them is used and the others are ignored; give each its own name", kind, name, prev),
			})
			continue
		}
		first[name] = path
	}
	return out
}
