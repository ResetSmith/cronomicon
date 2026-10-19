package gitlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"gopkg.in/yaml.v3"
)

// Assignment by commit (2.5.0 C10; IN-12, questions 87 to 93).
//
// A global administrator gives a job or a workflow of one agency's repository
// to another agency as ONE commit into that agency's repository: the
// definition and what it needs that the target does not have. For a job (C10.1)
// that is its script and the reusable schedules it names; for a workflow
// (C10.2) also every job its steps run, with theirs, and every sub-workflow,
// followed through. Each copied job names a scope the caller picks among the
// target's (an agency's job must name one of its own scopes, GR-14). Each YAML
// file carries metadata.origin; the server's record (assignments,
// assignment_files) is what "an update is available" reads (C10.3).
//
// What is copied is files of a repository, at the commit the origin last
// synced (question 87: a definition built in the app is refused; 2.6.0 makes
// every definition a file). Calendars are not files before 2.6.0 (question 88):
// the answer names the ones the target lacks. A script, schedule, job or
// sub-workflow the target's repository has by name is its own and is used,
// not added (question 90); a name its agency has only in the app refuses, as a
// Git step does not resolve to it. Secrets, variables and keys are the
// agency's own (question 92): the answer lists what the copy binds and names,
// and which of them the target lacks; nothing is created.

// AssignRequest is one assignment.
type AssignRequest struct {
	Kind         string // job, workflow
	UID          string
	TargetAgency string
	// The target agency's scope each copied job names: Scopes by the job's
	// scope in the origin ("" for a job with none), else Scope.
	Scope         string
	Scopes        map[string]string
	CommitMessage string
	Actor         string
}

// AssignedFile is one file of an assignment.
type AssignedFile struct {
	Path string `json:"path"`
	Role string `json:"role"` // definition, job, workflow, script, schedule
	// Action: "added"; "usesOwn" for a dependency the target has by name,
	// which was not written (Differs: its content is not the origin's); and
	// for an update (C10.3), "replaced" for a file of the last assignment that
	// the origin changed, "unchanged" for one it did not (not written).
	Action  string `json:"action"`
	Differs bool   `json:"differs,omitempty"`
}

// AssignLack is something the copy needs that the target agency does not have.
type AssignLack struct {
	Kind string `json:"kind"` // secret, variable, key, calendar, reactionUpstream, targetHost
	Name string `json:"name"`
	// Why is what the lack does, when it does more than leave a run without
	// it: a calendar the copy names is one the target's sync refuses it for.
	Why string `json:"why,omitempty"`
}

// AssignBinding is a binding the origin's job or script has, which the copy
// does not (bindings are the agency's, in the app): the target binds it again.
type AssignBinding struct {
	Kind string `json:"kind"` // secret, var, key
	Name string `json:"name"`
	As   string `json:"as,omitempty"`
}

// AssignResult is what an assignment did.
type AssignResult struct {
	AssignmentID string          `json:"assignmentId"`
	CommitSHA    string          `json:"commitSha"`
	Branch       string          `json:"branch"`
	OriginCommit string          `json:"originCommit"`
	Files        []AssignedFile  `json:"files"`
	Lacks        []AssignLack    `json:"lacks"`
	Bindings     []AssignBinding `json:"bindings"`
	// SyncStarted: the target's sync was started. The copy appears in the
	// target agency when it finishes, not before this answer.
	SyncStarted bool `json:"syncStarted"`

	// For the caller's audit row, not the answer.
	Kind         string `json:"-"`
	Updated      bool   `json:"-"` // an update of an earlier assignment (C10.3)
	OriginAgency string `json:"-"`
	Name         string `json:"-"`
}

// AssignError is a refusal: nothing was committed.
type AssignError struct {
	Status  int
	Code    string
	Message string
	Paths   []string
}

func (e *AssignError) Error() string { return e.Message }

func refuse(status int, code, format string, a ...any) *AssignError {
	return &AssignError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func refuseTaken(paths []string, format string, a ...any) *AssignError {
	e := refuse(http.StatusUnprocessableEntity, "name_taken", format, a...)
	e.Paths = paths
	return e
}

// fileHash is the hash an assignment records of a file's bytes.
func fileHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// planned is one file an assignment will write, and what the record keeps of
// it.
type planned struct {
	write      FileWrite
	role       string
	originPath string
	originHash string
	unchanged  bool // an update's file whose origin is as it was copied: not written
	// For an unchanged file, the hash the record keeps: what was committed
	// then, which is what the agency's repository holds.
	keptHash string
}

// recordedFile is a file the last assignment wrote.
type recordedFile struct{ originHash, committedHash string }

// defRow is a job or workflow row of a repository.
type defRow struct{ uid, name, source, owner, repo, path string }

// assignState is one assignment being planned.
type assignState struct {
	ctx context.Context
	r   *Registry
	req AssignRequest

	originSvc               *Service
	originRepo, originSHA   string
	originURL, originAgency string
	targetSvc               *Service
	targetRepo, targetSHA   string
	targetName              string

	res  *AssignResult
	plan []planned

	// An update (C10.3, question 91): the last assignment of this definition
	// to this agency, and the files it wrote (target path → committed hash).
	// Those are this assignment's to replace; a file of them the agency has
	// changed since refuses it.
	prevID          string
	recorded        map[string]recordedFile
	changedByAgency []string

	jobsSeen, wfsSeen map[string]bool
	scopesChecked     map[string]bool
	calendars         map[string]bool
	bindingOwners     []string
	secrets, keys     []string
	reactions         []ReactionEntry
	hosts             [][2]string // target host, the scope it must be in
}

// Assign makes one assignment (see the top of this file).
func (r *Registry) Assign(ctx context.Context, req AssignRequest) (*AssignResult, error) {
	q := r.db
	table := map[string]string{"job": "jobs", "workflow": "workflows"}[req.Kind]
	if table == "" {
		return nil, refuse(http.StatusUnprocessableEntity, "validation_failed", `kind must be "job" or "workflow"`)
	}
	top, err := r.defRowWhere(ctx, table, `uid = ?`, req.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse(http.StatusNotFound, "not_found", "no such %s", req.Kind)
	}
	if err != nil {
		return nil, err
	}
	if top.source != "git" || top.repo == "" || top.path == "" {
		return nil, refuse(http.StatusUnprocessableEntity, "not_in_git",
			"the %s %s was built in the app: only a definition that is a file of a repository can be assigned (commit it to its agency's repository first)", req.Kind, top.name)
	}
	if top.owner == req.TargetAgency {
		return nil, refuse(http.StatusUnprocessableEntity, "same_agency", "the %s %s is already this agency's", req.Kind, top.name)
	}

	st := &assignState{ctx: ctx, r: r, req: req, originRepo: top.repo, originAgency: top.owner,
		jobsSeen: map[string]bool{}, wfsSeen: map[string]bool{}, scopesChecked: map[string]bool{}, calendars: map[string]bool{},
		res: &AssignResult{Kind: req.Kind, OriginAgency: top.owner, Name: top.name, Files: []AssignedFile{}, Lacks: []AssignLack{}, Bindings: []AssignBinding{}}}
	if err := q.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, req.TargetAgency).Scan(&st.targetName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refuse(http.StatusNotFound, "not_found", "no such agency")
		}
		return nil, err
	}
	var originName string
	_ = q.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, top.owner).Scan(&originName)
	st.targetRepo, err = settings.RepoOfAgency(ctx, q, req.TargetAgency)
	if errors.Is(err, settings.ErrRepoNotFound) {
		return nil, refuse(http.StatusUnprocessableEntity, "no_repository", "%s has no repository to commit to", st.targetName)
	}
	if err != nil {
		return nil, err
	}
	st.originSvc, st.targetSvc = r.Service(top.repo), r.Service(st.targetRepo)
	if st.originSvc == nil || st.targetSvc == nil {
		return nil, refuse(http.StatusServiceUnavailable, "unavailable", "a repository's sync service is not running; try again in a moment")
	}
	// One commit of the origin, read first: every file is read as it is there,
	// and it is what the record and the stamp name.
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(last_sha, ''), COALESCE(url, '') FROM git_repos WHERE id = ?`, top.repo).
		Scan(&st.originSHA, &st.originURL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if st.originSHA == "" {
		return nil, refuse(http.StatusConflict, "origin_unsynced", "the %s's repository has not been synced", req.Kind)
	}
	_ = q.QueryRowContext(ctx, `SELECT COALESCE(last_sha, '') FROM git_repos WHERE id = ?`, st.targetRepo).Scan(&st.targetSHA)
	st.res.OriginCommit = st.originSHA
	if err := st.loadPrevious(top); err != nil {
		return nil, err
	}

	// The definition itself: its name is the target agency's to give, or it
	// is refused. An update's own copy is not "taken".
	if _, ours := st.recorded[top.path]; !ours {
		if n, err := st.count(`SELECT COUNT(*) FROM `+table+` WHERE owner_agency = ? AND name = ? AND deleted_at IS NULL`, req.TargetAgency, top.name); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, refuseTaken([]string{top.path}, "%s already has a %s named %s", st.targetName, req.Kind, top.name)
		}
	}
	if req.Kind == "job" {
		err = st.addJob(top, "definition")
	} else {
		err = st.addWorkflow(top, "definition")
	}
	if err != nil {
		return nil, err
	}
	if len(st.changedByAgency) > 0 {
		sort.Strings(st.changedByAgency)
		e := refuse(http.StatusUnprocessableEntity, "changed_by_agency",
			"%s has changed its copy of %s since the last assignment: an update does not overwrite an agency's change (revert it, or remove the file, and assign again)",
			st.targetName, strings.Join(st.changedByAgency, ", "))
		e.Paths = st.changedByAgency
		return nil, e
	}
	if err := st.validate(); err != nil {
		return nil, err
	}
	if err := st.computeLacks(); err != nil {
		return nil, err
	}

	var writes []FileWrite
	for _, p := range st.plan {
		if !p.unchanged {
			writes = append(writes, p.write)
		}
	}
	if len(writes) == 0 {
		return nil, refuse(http.StatusConflict, "up_to_date", "%s's copy of the %s %s is as the origin is now: there is nothing to update", st.targetName, req.Kind, top.name)
	}
	msg := strings.TrimSpace(req.CommitMessage)
	if msg == "" {
		verb := "assign"
		if st.prevID != "" {
			verb = "update"
		}
		msg = fmt.Sprintf("cronomicon: %s %s %s from %s", verb, req.Kind, top.name, originName)
	}
	commit, err := st.targetSvc.CommitFiles(ctx, writes, msg)
	var takenErr *PathsTakenError
	var pre *PreconditionError
	switch {
	case errors.As(err, &takenErr):
		return nil, refuseTaken(takenErr.Paths, "%s's repository already has %s", st.targetName, strings.Join(takenErr.Paths, ", "))
	case errors.As(err, &pre) && pre.Unsynced:
		st.targetSvc.TriggerSync(ctx, "poll")
		return nil, refuse(http.StatusConflict, "target_unsynced",
			"%s's repository has commits that had not been synced here. A sync has been started: assign again when it has finished", st.targetName)
	case err != nil:
		return nil, err
	}
	st.res.CommitSHA, st.res.Branch = commit.CommitSHA, commit.Branch
	st.res.Updated = st.prevID != ""

	// The record. The commit is in: a record that cannot be written is logged,
	// and the answer still says what was committed.
	st.res.AssignmentID = db.NewID()
	if err := st.record(top); err != nil {
		r.logWarn("assignment: committed but not recorded; no update will be offered for it", "assignment", st.res.AssignmentID, "error", err)
	}
	st.targetSvc.TriggerSync(ctx, "manual")
	st.res.SyncStarted = true
	return st.res, nil
}

// defRowWhere reads one Git or in-app job or workflow row, not in the bin.
func (r *Registry) defRowWhere(ctx context.Context, table, where string, args ...any) (defRow, error) {
	var d defRow
	err := r.db.QueryRowContext(ctx, `
		SELECT uid, name, source, owner_agency, COALESCE(repo_id, ''), COALESCE(source_path, '')
		  FROM `+table+` WHERE `+where+` AND deleted_at IS NULL`, args...).
		Scan(&d.uid, &d.name, &d.source, &d.owner, &d.repo, &d.path)
	return d, err
}

func (st *assignState) count(query string, args ...any) (int, error) {
	var n int
	err := st.r.db.QueryRowContext(st.ctx, query, args...).Scan(&n)
	return n, err
}

// read reads a file or directory of the origin at its commit.
func (st *assignState) read(p string) ([]CommitFile, error) {
	f, err := ReadCommitTree(st.originSvc.cloneDir, st.originSHA, p)
	if err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%v", err)
	}
	return f, nil
}

// origin is the metadata.origin stamp of a file of the origin.
func (st *assignState) origin(p string) []string {
	o := []string{"agency", st.originAgency}
	if st.originURL != "" {
		o = append(o, "repository", st.originURL)
	}
	return append(o, "path", p, "commit", st.originSHA)
}

// add plans a file, once. A file the last assignment wrote (an update) is
// replaced when the agency has not changed it, and refused when it has.
func (st *assignState) add(f CommitFile, role string, content []byte) {
	for _, p := range st.plan {
		if p.write.Path == f.Path {
			return
		}
	}
	p := planned{write: FileWrite{Path: f.Path, Content: content, Executable: f.Executable}, role: role, originPath: f.Path, originHash: fileHash(f.Content)}
	action := "added"
	if rec, ours := st.recorded[f.Path]; ours {
		// Judged by the ORIGIN's bytes: the stamp names the origin's commit,
		// which moves with any change to its repository, so the file written
		// would differ even when the origin's file has not.
		if theirs, err := ReadCommitTree(st.targetSvc.cloneDir, st.targetSHA, f.Path); err == nil && len(theirs) == 1 {
			switch {
			case p.originHash == rec.originHash:
				p.unchanged, p.keptHash, action = true, rec.committedHash, "unchanged"
			case fileHash(theirs[0].Content) != rec.committedHash:
				st.changedByAgency = append(st.changedByAgency, f.Path)
			default:
				p.write.Replace, action = true, "replaced"
			}
		}
		// Gone from the agency's repository: written again, as new.
	}
	st.plan = append(st.plan, p)
	st.res.Files = append(st.res.Files, AssignedFile{Path: f.Path, Role: role, Action: action})
}

// loadPrevious finds the last assignment of this definition to this agency,
// which makes this one an update: by the definition's uid, or by its file (a
// file removed from the origin and added again is a new row, with a new uid).
func (st *assignState) loadPrevious(top defRow) error {
	st.recorded = map[string]recordedFile{}
	err := st.r.db.QueryRowContext(st.ctx, `
		SELECT id FROM assignments
		 WHERE target_agency = ? AND kind = ? AND (origin_uid = ? OR (origin_repo = ? AND origin_path = ?))
		 ORDER BY created_at DESC, rowid DESC LIMIT 1`, st.req.TargetAgency, st.req.Kind, top.uid, top.repo, top.path).Scan(&st.prevID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	rows, err := st.r.db.QueryContext(st.ctx, `SELECT target_path, origin_hash, committed_hash FROM assignment_files WHERE assignment_id = ?`, st.prevID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var rec recordedFile
		if err := rows.Scan(&p, &rec.originHash, &rec.committedHash); err != nil {
			return err
		}
		st.recorded[p] = rec
	}
	return rows.Err()
}

// stamped plans a YAML document with its origin stamp (and the scope).
func (st *assignState) stamped(f CommitFile, role string, scope *string) error {
	content, err := stampYAML(f.Content, st.origin(f.Path), scope)
	if err != nil {
		return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", f.Path, err)
	}
	st.add(f, role, content)
	return nil
}

// usesOwn reports a dependency the target's repository has, comparing its file
// at the target's last sync with the origin's.
func (st *assignState) usesOwn(role, targetPath string, origin []byte) {
	differs := true
	if theirs, err := ReadCommitTree(st.targetSvc.cloneDir, st.targetSHA, targetPath); err == nil && len(theirs) == 1 {
		differs = !bytes.Equal(theirs[0].Content, origin)
	}
	st.res.Files = append(st.res.Files, AssignedFile{Path: targetPath, Role: role, Action: "usesOwn", Differs: differs})
}

// scopeFor is the target's scope a copied job names, checked as the target's
// sync will check it.
func (st *assignState) scopeFor(job, originScope string) (string, error) {
	scope := strings.TrimSpace(st.req.Scopes[originScope])
	if scope == "" {
		scope = strings.TrimSpace(st.req.Scope)
	}
	if scope == "" {
		if originScope == "" {
			return "", refuse(http.StatusUnprocessableEntity, "scope_required", "name the scope of %s the job %s is to run on", st.targetName, job)
		}
		return "", refuse(http.StatusUnprocessableEntity, "scope_required",
			"name the scope of %s the job %s is to run on (it runs on %s here)", st.targetName, job, originScope)
	}
	if !st.scopesChecked[scope] {
		alone, _, err := scopeOwnedAlone(st.ctx, st.r.db, scope, st.req.TargetAgency)
		if err != nil {
			return "", err
		}
		if !alone {
			return "", refuse(http.StatusUnprocessableEntity, "unknown_scope",
				"%q is not a scope of %s alone: a job in an agency's repository must name a scope that agency owns", scope, st.targetName)
		}
		st.scopesChecked[scope] = true
	}
	return scope, nil
}

// addJob plans a job's file, its script and its schedules.
func (st *assignState) addJob(row defRow, role string) error {
	st.jobsSeen[row.name] = true
	files, err := st.read(row.path)
	if err != nil {
		return err
	}
	var spec JobYAML
	if err := yaml.Unmarshal(files[0].Content, &spec); err != nil {
		return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", row.path, err)
	}
	scope, err := st.scopeFor(row.name, strings.TrimSpace(spec.Spec.Scope))
	if err != nil {
		return err
	}
	if err := st.stamped(files[0], role, &scope); err != nil {
		return err
	}
	st.bindingOwners = append(st.bindingOwners, row.uid)
	if n := strings.TrimSpace(spec.Spec.BecomePasswordSecret); n != "" {
		st.secrets = append(st.secrets, n)
	}
	if n := strings.TrimSpace(spec.Spec.SSHCredential); n != "" {
		st.keys = append(st.keys, n)
	}
	if h := strings.TrimSpace(spec.Spec.TargetHost); h != "" {
		st.hosts = append(st.hosts, [2]string{h, scope})
	}
	st.reactions = append(st.reactions, spec.Spec.Reactions...)

	if ref := strings.TrimSpace(spec.Spec.ScriptRef); ref != "" {
		if err := st.addScript(ref); err != nil {
			return err
		}
	} else if p := strings.TrimSpace(spec.Spec.ScriptPath); p != "" {
		files, err := st.read(p)
		if err != nil {
			return err
		}
		st.add(files[0], "script", files[0].Content)
	}
	return st.addSchedules(spec.Spec.ScheduleRefs, spec.Spec.Schedules)
}

// addScript plans a script by name in the origin's repository: the target's
// own when it has one of that name.
func (st *assignState) addScript(ref string) error {
	q := st.r.db
	var uid, src, body, project, hash string
	if err := q.QueryRowContext(st.ctx, `
		SELECT uid, COALESCE(source_path, ''), COALESCE(script_path, ''), COALESCE(project_root, ''), COALESCE(content_hash, '')
		  FROM scripts WHERE repo_id = ? AND name = ?`, st.originRepo, ref).Scan(&uid, &src, &body, &project, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the script %s is not in the origin's repository", ref)
		}
		return err
	}
	var theirs, theirPath string
	err := q.QueryRowContext(st.ctx, `SELECT COALESCE(content_hash, ''), COALESCE(source_path, '') FROM scripts WHERE repo_id = ? AND name = ?`,
		st.targetRepo, ref).Scan(&theirs, &theirPath)
	if _, ours := st.recorded[theirPath]; err == nil && !ours {
		for _, f := range st.res.Files {
			if f.Path == theirPath && f.Role == "script" {
				return nil
			}
		}
		st.res.Files = append(st.res.Files, AssignedFile{Path: theirPath, Role: "script", Action: "usesOwn", Differs: theirs != hash})
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, p := range st.plan {
		if p.write.Path == src {
			return nil // another job of this assignment brings it
		}
	}
	st.bindingOwners = append(st.bindingOwners, uid)
	files, err := st.read(src)
	if err != nil {
		return err
	}
	if src != body || project != "" {
		// A Script document: it carries the stamp. A plain file is copied as it
		// is (the record holds its origin).
		if err := st.stamped(files[0], "script", nil); err != nil {
			return err
		}
	} else {
		st.add(files[0], "script", files[0].Content)
	}
	more := ""
	switch {
	case project != "":
		more = project
	case body != "" && body != src:
		more = body
	}
	if more != "" {
		files, err := st.read(more)
		if err != nil {
			return err
		}
		for _, f := range files {
			st.add(f, "script", f.Content)
		}
	}
	return nil
}

// addSchedules plans the reusable schedules a definition names, and notes the
// calendars its own entries and those schedules name.
func (st *assignState) addSchedules(refs []string, entries []ScheduleEntry) error {
	for _, e := range entries {
		for _, c := range append(append([]string{}, e.SkipCalendars...), e.OnlyCalendars...) {
			st.calendars[c] = true
		}
	}
	q := st.r.db
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		var src, hash string
		if err := q.QueryRowContext(st.ctx, `
			SELECT COALESCE(source_path, ''), COALESCE(content_hash, '') FROM schedules
			 WHERE repo_id = ? AND name = ? AND source = 'git' AND deleted_at IS NULL`, st.originRepo, ref).Scan(&src, &hash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the schedule %s is not in the origin's repository", ref)
			}
			return err
		}
		var theirs, theirPath string
		err := q.QueryRowContext(st.ctx, `
			SELECT COALESCE(content_hash, ''), COALESCE(source_path, '') FROM schedules
			 WHERE repo_id = ? AND name = ? AND source = 'git' AND deleted_at IS NULL`, st.targetRepo, ref).Scan(&theirs, &theirPath)
		if _, ours := st.recorded[theirPath]; err == nil && !ours {
			seen := false
			for _, f := range st.res.Files {
				seen = seen || f.Path == theirPath && f.Role == "schedule"
			}
			if !seen {
				st.res.Files = append(st.res.Files, AssignedFile{Path: theirPath, Role: "schedule", Action: "usesOwn", Differs: theirs != hash})
			}
			continue
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		files, err := st.read(src)
		if err != nil {
			return err
		}
		var sy ScheduleYAML
		if yaml.Unmarshal(files[0].Content, &sy) == nil {
			for _, c := range append(append([]string{}, sy.Spec.SkipCalendars...), sy.Spec.OnlyCalendars...) {
				st.calendars[c] = true
			}
		}
		if err := st.stamped(files[0], "schedule", nil); err != nil {
			return err
		}
	}
	return nil
}

// addWorkflow plans a workflow's file, its schedules, and every job and
// sub-workflow its steps run.
func (st *assignState) addWorkflow(row defRow, role string) error {
	st.wfsSeen[row.name] = true
	files, err := st.read(row.path)
	if err != nil {
		return err
	}
	var spec WorkflowYAML
	if err := yaml.Unmarshal(files[0].Content, &spec); err != nil {
		return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", row.path, err)
	}
	if err := st.stamped(files[0], role, nil); err != nil {
		return err
	}
	st.reactions = append(st.reactions, spec.Spec.Reactions...)
	if err := st.addSchedules(spec.Spec.ScheduleRefs, spec.Spec.Schedules); err != nil {
		return err
	}
	var jobs []stepRef
	var wfs []string
	walkStepRefs(spec.Spec.Steps, func(s stepRef) { jobs = append(jobs, s) }, func(w string) { wfs = append(wfs, w) })
	for _, j := range jobs {
		if err := st.depJob(row.name, j); err != nil {
			return err
		}
	}
	for _, w := range wfs {
		if err := st.depWorkflow(row.name, w); err != nil {
			return err
		}
	}
	return nil
}

// depJob plans a job a workflow's step runs: the target's own when its
// repository has one of that name.
func (st *assignState) depJob(wf string, s stepRef) error {
	if s.source == "cronomicon" {
		return refuse(http.StatusUnprocessableEntity, "not_in_git",
			"the workflow %s's step runs %s, a job built in the app: only files of a repository are assigned", wf, s.name)
	}
	if st.jobsSeen[s.name] {
		return nil
	}
	st.jobsSeen[s.name] = true
	row, err := st.r.defRowWhere(st.ctx, "jobs", `repo_id = ? AND name = ? AND source = 'git'`, st.originRepo, s.name)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the workflow %s runs the job %s, which is not in its repository", wf, s.name)
	}
	if err != nil {
		return err
	}
	return st.dependency("jobs", "job", row, func() error { return st.addJob(row, "job") })
}

// depWorkflow plans a sub-workflow, as depJob a job.
func (st *assignState) depWorkflow(wf, name string) error {
	if st.wfsSeen[name] {
		return nil // planned already, or the workflow itself (a cycle)
	}
	st.wfsSeen[name] = true
	row, err := st.r.defRowWhere(st.ctx, "workflows", `repo_id = ? AND name = ? AND source = 'git'`, st.originRepo, name)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the workflow %s runs the workflow %s, which is not in its repository", wf, name)
	}
	if err != nil {
		return err
	}
	return st.dependency("workflows", "workflow", row, func() error { return st.addWorkflow(row, "workflow") })
}

// dependency decides a job or sub-workflow a workflow needs: the target
// repository's own of that name is used; a name its agency has only in the
// app refuses (a Git step resolves in its own repository, not to it); else the
// origin's is added.
func (st *assignState) dependency(table, role string, row defRow, add func() error) error {
	own, err := st.r.defRowWhere(st.ctx, table, `repo_id = ? AND name = ? AND source = 'git'`, st.targetRepo, row.name)
	if _, ours := st.recorded[own.path]; err == nil && ours {
		return add() // the last assignment's copy: an update replaces it
	}
	if err == nil {
		files, rerr := st.read(row.path)
		if rerr != nil {
			return rerr
		}
		st.usesOwn(role, own.path, files[0].Content)
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if n, err := st.count(`SELECT COUNT(*) FROM `+table+` WHERE owner_agency = ? AND name = ? AND deleted_at IS NULL
	                         AND NOT (source = 'git' AND repo_id = ?)`, st.req.TargetAgency, row.name, st.targetRepo); err != nil {
		return err
	} else if n > 0 {
		return refuseTaken([]string{row.path}, "%s has a %s named %s that is not a file of its repository: the copy's steps would not run it", st.targetName, role, row.name)
	}
	return add()
}

// stepRef is a job a workflow step runs.
type stepRef struct{ name, source string }

// walkStepRefs visits every job and sub-workflow a raw step graph runs, in
// every container the engine walks (a parallel's jobs, a sequence's steps, a
// branch's pass and fail steps).
func walkStepRefs(steps []any, job func(stepRef), wf func(string)) {
	str := func(m map[string]any, k string) string {
		s, _ := m[k].(string)
		return strings.TrimSpace(s)
	}
	for _, raw := range steps {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch t := str(m, "type"); t {
		case "workflow":
			if w := str(m, "workflow"); w != "" {
				wf(w)
			}
		case "job", "":
			_, nests := m["jobs"]
			_, seq := m["steps"]
			if n := str(m, "name"); n != "" && (t == "job" || !nests && !seq && m["pass"] == nil && m["fail"] == nil) {
				job(stepRef{name: n, source: str(m, "jobSource")})
			}
		}
		for _, key := range []string{"jobs", "steps"} {
			if nested, ok := m[key].([]any); ok {
				walkStepRefs(nested, job, wf)
			}
		}
		for _, key := range []string{"pass", "fail"} {
			if arm, ok := m[key].(map[string]any); ok {
				if nested, ok := arm["steps"].([]any); ok {
					walkStepRefs(nested, job, wf)
				}
			}
		}
	}
}

// validate holds every YAML document the commit adds to what the target's
// sync can read.
func (st *assignState) validate() error {
	for _, p := range st.plan {
		yamlFile := strings.HasSuffix(p.write.Path, ".yaml") || strings.HasSuffix(p.write.Path, ".yml")
		if !yamlFile || p.role == "script" && !isScriptDocument(p.write.Content) {
			continue // a script's body, or a file of its project: not a document of ours
		}
		verrs, err := validateYAMLBytes(p.write.Path, p.write.Content)
		if err != nil {
			return err
		}
		if len(verrs) > 0 {
			msgs := make([]string, len(verrs))
			for i, v := range verrs {
				msgs[i] = v.Error()
			}
			return refuse(http.StatusUnprocessableEntity, "validation_failed", "%s: %s", p.write.Path, strings.Join(msgs, "; "))
		}
	}
	return nil
}

// isScriptDocument reports whether content is a `kind: Script` document (and
// not a plain YAML file synthesised as a script).
func isScriptDocument(content []byte) bool {
	var head struct {
		Kind string `yaml:"kind"`
	}
	return yaml.Unmarshal(content, &head) == nil && head.Kind == "Script"
}

// record writes the record of a committed assignment.
func (st *assignState) record(top defRow) error {
	tx, err := st.r.db.BeginTx(st.ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if st.prevID != "" {
		// An update's record replaces the last one: the files it did not write
		// again are no longer followed.
		if _, err := tx.ExecContext(st.ctx, `DELETE FROM assignments WHERE id = ?`, st.prevID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(st.ctx, `
		INSERT INTO assignments (id, kind, origin_agency, origin_repo, origin_uid, origin_name, origin_path, origin_commit,
		                         target_agency, target_repo, target_path, target_scope, commit_sha, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.res.AssignmentID, st.req.Kind, top.owner, top.repo, top.uid, top.name, top.path, st.originSHA,
		st.req.TargetAgency, st.targetRepo, top.path, strings.TrimSpace(st.req.Scope), st.res.CommitSHA, st.req.Actor,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	for _, p := range st.plan {
		committed := fileHash(p.write.Content)
		if p.unchanged {
			committed = p.keptHash
		}
		if _, err := tx.ExecContext(st.ctx, `
			INSERT INTO assignment_files (assignment_id, target_path, origin_path, role, origin_hash, committed_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			st.res.AssignmentID, p.write.Path, p.originPath, p.role, p.originHash, committed); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// computeLacks fills what the copy needs that the target agency does not have,
// and the bindings it must make again.
func (st *assignState) computeLacks() error {
	q, res, agency := st.r.db, st.res, st.req.TargetAgency
	lack := func(kind, name, query string, args ...any) error {
		var ok bool
		if err := q.QueryRowContext(st.ctx, `SELECT EXISTS (`+query+`)`, args...).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			for _, l := range res.Lacks {
				if l.Kind == kind && l.Name == name {
					return nil
				}
			}
			res.Lacks = append(res.Lacks, AssignLack{Kind: kind, Name: name})
		}
		return nil
	}
	secret := func(name string) error {
		return lack("secret", name, `SELECT 1 FROM secrets WHERE key = ? AND owner_agency = ?`, name, agency)
	}
	variable := func(name string) error {
		return lack("variable", name, `SELECT 1 FROM env_vars WHERE key = ? AND owner_agency = ?`, name, agency)
	}
	key := func(name string) error {
		return lack("key", name, `SELECT 1 FROM ssh_credentials WHERE label = ? AND owner_agency = ?`, name, agency)
	}
	for _, n := range st.secrets {
		if err := secret(n); err != nil {
			return err
		}
	}
	for _, n := range st.keys {
		if err := key(n); err != nil {
			return err
		}
	}
	for _, owner := range st.bindingOwners {
		rows, err := q.QueryContext(st.ctx, `
			SELECT ref_kind, ref_name, COALESCE(alias, '') FROM reference_bindings
			 WHERE owner_uid = ? ORDER BY ref_kind, ref_name, alias`, owner)
		if err != nil {
			return err
		}
		var bs []AssignBinding
		for rows.Next() {
			var b AssignBinding
			if err := rows.Scan(&b.Kind, &b.Name, &b.As); err != nil {
				rows.Close()
				return err
			}
			bs = append(bs, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, b := range bs {
			res.Bindings = append(res.Bindings, b)
			var err error
			switch b.Kind {
			case "secret":
				err = secret(b.Name)
			case "var":
				err = variable(b.Name)
			case "key":
				err = key(b.Name)
			}
			if err != nil {
				return err
			}
		}
	}

	names := make([]string, 0, len(st.calendars))
	for c := range st.calendars {
		if c = strings.TrimSpace(c); c != "" {
			names = append(names, c)
		}
	}
	sort.Strings(names)
	for _, c := range names {
		if err := lack("calendar", c, `SELECT 1 FROM calendars WHERE name = ? AND owner_agency = ?`, c, agency); err != nil {
			return err
		}
	}
	for i := range res.Lacks {
		if res.Lacks[i].Kind == "calendar" {
			// Calendars are not files before 2.6.0 (question 88): the copy is
			// committed, and the target's sync refuses the schedule that names
			// this calendar, and what names that schedule, until the agency has
			// one.
			res.Lacks[i].Why = "the agency's sync refuses the schedule naming it, and the definition with it, until the agency has a calendar of this name; create it there and sync"
		}
	}
	for _, re := range st.reactions {
		name := strings.TrimSpace(re.OnName)
		if name == "" {
			continue
		}
		if re.OnKind == "job" && st.jobsSeen[name] || re.OnKind == "workflow" && st.wfsSeen[name] {
			continue // part of this assignment, or the target's own of that name
		}
		table := "jobs"
		if re.OnKind == "workflow" {
			table = "workflows"
		}
		if err := lack("reactionUpstream", re.OnKind+":"+name,
			`SELECT 1 FROM `+table+` WHERE name = ? AND owner_agency = ? AND deleted_at IS NULL`, name, agency); err != nil {
			return err
		}
	}
	for _, h := range st.hosts {
		if err := lack("targetHost", h[0], `
			SELECT 1 FROM scope_hosts sh JOIN scopes sc ON sc.id = sh.scope_id
			 WHERE sc.name = ? AND sc.owner_agency = ? AND sh.host = ?`, h[1], agency, h[0]); err != nil {
			return err
		}
	}
	return nil
}

// stampYAML returns content with metadata.origin set to the key/value pairs of
// origin (in order), and, when scope is given, spec.scope set to it. Comments
// and the order of the other keys are kept; the layout is yaml.v3's.
func stampYAML(content []byte, origin []string, scope *string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("not a YAML mapping")
	}
	root := doc.Content[0]
	o := &yaml.Node{Kind: yaml.MappingNode}
	for i := 0; i+1 < len(origin); i += 2 {
		o.Content = append(o.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: origin[i]},
			&yaml.Node{Kind: yaml.ScalarNode, Value: origin[i+1], Style: yaml.DoubleQuotedStyle})
	}
	setKey(mappingAt(root, "metadata"), "origin", o)
	if scope != nil {
		setKey(mappingAt(root, "spec"), "scope", &yaml.Node{Kind: yaml.ScalarNode, Value: *scope})
	}
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// mappingAt is the mapping under key in m, added when there is none.
func mappingAt(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	n := &yaml.Node{Kind: yaml.MappingNode}
	setKey(m, key, n)
	return n
}

// setKey sets key in mapping m to v, in place when it is there.
func setKey(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}

// AssignmentDetails is the change-log text of an assignment.
func AssignmentDetails(res *AssignResult) string {
	b, _ := json.Marshal(map[string]any{"originAgency": res.OriginAgency, "originCommit": res.OriginCommit, "commit": res.CommitSHA, "files": res.Files})
	return string(b)
}
