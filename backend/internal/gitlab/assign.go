package gitlab

import (
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

// Assignment by commit (2.5.0 C10.1; IN-12, questions 87 to 93).
//
// A global administrator gives a job of one agency's repository to another
// agency as ONE commit into that agency's repository: the job's file, its
// script and the reusable schedules it names, when the target does not have
// them. The job's scope is the one the caller picks among the target's (an
// agency's job must name one of its own scopes, GR-14). Each YAML file carries
// metadata.origin; the server's record (assignments, assignment_files) is what
// "an update is available" reads (C10.3).
//
// What is copied is files of a repository, at the commit the origin last
// synced (question 87: a job built in the app is refused; 2.6.0 makes every
// definition a file). Calendars are not files before 2.6.0 (question 88): the
// answer names the ones the target lacks. A script or schedule the target has
// by name is its own and is used, not added (question 90). Secrets, variables
// and keys are the agency's own (question 92): the answer lists what the job
// binds and names, and which of them the target lacks; nothing is created.

// AssignJobRequest is one assignment of a job.
type AssignJobRequest struct {
	JobUID        string
	TargetAgency  string
	Scope         string // the target agency's scope the job's copy names
	CommitMessage string
	Actor         string
}

// AssignedFile is one file of an assignment.
type AssignedFile struct {
	Path string `json:"path"`
	Role string `json:"role"` // definition, script, schedule
	// Action: "added", or "usesOwn" for a script or schedule the target has by
	// name, which was not written (Differs: its content is not the origin's).
	Action  string `json:"action"`
	Differs bool   `json:"differs,omitempty"`
}

// AssignLack is something the job needs that the target agency does not have.
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
	// SyncStarted: the target's sync was started. The job appears in the
	// target agency when it finishes, not before this answer.
	SyncStarted bool `json:"syncStarted"`

	// For the caller's audit row, not the answer.
	OriginAgency string `json:"-"`
	JobName      string `json:"-"`
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

// fileHash is the hash an assignment records of a file's bytes.
func fileHash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// AssignJob makes one assignment of a job (see the top of this file).
func (r *Registry) AssignJob(ctx context.Context, req AssignJobRequest) (*AssignResult, error) {
	q := r.db
	var job struct{ uid, name, source, owner, repo, path string }
	err := q.QueryRowContext(ctx, `
		SELECT uid, name, source, owner_agency, COALESCE(repo_id, ''), COALESCE(source_path, '')
		  FROM jobs WHERE uid = ? AND deleted_at IS NULL`, req.JobUID).
		Scan(&job.uid, &job.name, &job.source, &job.owner, &job.repo, &job.path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, refuse(http.StatusNotFound, "not_found", "no such job")
	}
	if err != nil {
		return nil, err
	}
	if job.source != "git" || job.repo == "" || job.path == "" {
		return nil, refuse(http.StatusUnprocessableEntity, "not_in_git",
			"the job %s was built in the app: only a job that is a file of a repository can be assigned (commit it to its agency's repository first)", job.name)
	}
	if job.owner == req.TargetAgency {
		return nil, refuse(http.StatusUnprocessableEntity, "same_agency", "the job %s is already this agency's", job.name)
	}
	var targetName, originName string
	if err := q.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, req.TargetAgency).Scan(&targetName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, refuse(http.StatusNotFound, "not_found", "no such agency")
		}
		return nil, err
	}
	_ = q.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, job.owner).Scan(&originName)
	targetRepo, err := settings.RepoOfAgency(ctx, q, req.TargetAgency)
	if errors.Is(err, settings.ErrRepoNotFound) {
		return nil, refuse(http.StatusUnprocessableEntity, "no_repository", "%s has no repository to commit to", targetName)
	}
	if err != nil {
		return nil, err
	}
	originSvc, targetSvc := r.Service(job.repo), r.Service(targetRepo)
	if originSvc == nil || targetSvc == nil {
		return nil, refuse(http.StatusServiceUnavailable, "unavailable", "a repository's sync service is not running; try again in a moment")
	}

	// One commit of the origin, read first: every file is read as it is there,
	// and it is what the record and the stamp name.
	var sha, originURL string
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(last_sha, ''), COALESCE(url, '') FROM git_repos WHERE id = ?`, job.repo).
		Scan(&sha, &originURL); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if sha == "" {
		return nil, refuse(http.StatusConflict, "origin_unsynced", "the job's repository has not been synced")
	}
	read := func(p string) ([]CommitFile, error) {
		f, err := ReadCommitTree(originSvc.cloneDir, sha, p)
		if err != nil {
			return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%v", err)
		}
		return f, nil
	}

	scope := strings.TrimSpace(req.Scope)
	if scope == "" {
		return nil, refuse(http.StatusUnprocessableEntity, "scope_required", "name the scope of %s the job is to run on", targetName)
	}
	alone, _, err := scopeOwnedAlone(ctx, q, scope, req.TargetAgency)
	if err != nil {
		return nil, err
	}
	if !alone {
		return nil, refuse(http.StatusUnprocessableEntity, "unknown_scope",
			"%q is not a scope of %s alone: a job in an agency's repository must name a scope that agency owns", scope, targetName)
	}
	var taken int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE owner_agency = ? AND name = ? AND deleted_at IS NULL`,
		req.TargetAgency, job.name).Scan(&taken); err != nil {
		return nil, err
	}
	if taken > 0 {
		e := refuse(http.StatusUnprocessableEntity, "name_taken", "%s already has a job named %s", targetName, job.name)
		e.Paths = []string{job.path}
		return nil, e
	}

	defFiles, err := read(job.path)
	if err != nil {
		return nil, err
	}
	var spec JobYAML
	if err := yaml.Unmarshal(defFiles[0].Content, &spec); err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", job.path, err)
	}
	origin := func(p string) []string {
		o := []string{"agency", job.owner}
		if originURL != "" {
			o = append(o, "repository", originURL)
		}
		return append(o, "path", p, "commit", sha)
	}

	res := &AssignResult{OriginCommit: sha, OriginAgency: job.owner, JobName: job.name, Files: []AssignedFile{}, Lacks: []AssignLack{}, Bindings: []AssignBinding{}}
	var plan []planned
	add := func(f CommitFile, role string, content []byte) {
		for _, p := range plan {
			if p.write.Path == f.Path {
				return
			}
		}
		plan = append(plan, planned{FileWrite{Path: f.Path, Content: content, Executable: f.Executable}, role, f.Path, fileHash(f.Content)})
		res.Files = append(res.Files, AssignedFile{Path: f.Path, Role: role, Action: "added"})
	}

	def, err := stampYAML(defFiles[0].Content, origin(job.path), &scope)
	if err != nil {
		return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", job.path, err)
	}
	add(defFiles[0], "definition", def)

	// The script: by name in the origin's repository; the target's own when it
	// has one of that name.
	var scriptUID string
	if ref := strings.TrimSpace(spec.Spec.ScriptRef); ref != "" {
		var uid, src, body, project, hash string
		if err := q.QueryRowContext(ctx, `
			SELECT uid, COALESCE(source_path, ''), COALESCE(script_path, ''), COALESCE(project_root, ''), COALESCE(content_hash, '')
			  FROM scripts WHERE repo_id = ? AND name = ?`, job.repo, ref).Scan(&uid, &src, &body, &project, &hash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the job's script %s is not in its repository", ref)
			}
			return nil, err
		}
		var theirs sql.NullString
		err := q.QueryRowContext(ctx, `SELECT COALESCE(content_hash, '') FROM scripts WHERE repo_id = ? AND name = ?`, targetRepo, ref).Scan(&theirs)
		switch {
		case err == nil:
			res.Files = append(res.Files, AssignedFile{Path: src, Role: "script", Action: "usesOwn", Differs: theirs.String != hash})
		case errors.Is(err, sql.ErrNoRows):
			scriptUID = uid
			files, err := read(src)
			if err != nil {
				return nil, err
			}
			content := files[0].Content
			if src != body || project != "" {
				// A Script document: it carries the stamp. A plain file is copied as
				// it is (the record holds its origin).
				if content, err = stampYAML(content, origin(src), nil); err != nil {
					return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", src, err)
				}
			}
			add(files[0], "script", content)
			more := ""
			switch {
			case project != "":
				more = project
			case body != "" && body != src:
				more = body
			}
			if more != "" {
				files, err := read(more)
				if err != nil {
					return nil, err
				}
				for _, f := range files {
					add(f, "script", f.Content)
				}
			}
		default:
			return nil, err
		}
	} else if p := strings.TrimSpace(spec.Spec.ScriptPath); p != "" {
		files, err := read(p)
		if err != nil {
			return nil, err
		}
		add(files[0], "script", files[0].Content)
	}

	// Reusable schedules, by name in the origin's repository.
	calendars := map[string]bool{}
	for _, e := range spec.Spec.Schedules {
		for _, c := range append(append([]string{}, e.SkipCalendars...), e.OnlyCalendars...) {
			calendars[c] = true
		}
	}
	for _, ref := range spec.Spec.ScheduleRefs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		var src, hash string
		if err := q.QueryRowContext(ctx, `
			SELECT COALESCE(source_path, ''), COALESCE(content_hash, '') FROM schedules
			 WHERE repo_id = ? AND name = ? AND source = 'git' AND deleted_at IS NULL`, job.repo, ref).Scan(&src, &hash); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "the job's schedule %s is not in its repository", ref)
			}
			return nil, err
		}
		var theirs string
		err := q.QueryRowContext(ctx, `
			SELECT COALESCE(content_hash, '') FROM schedules
			 WHERE repo_id = ? AND name = ? AND source = 'git' AND deleted_at IS NULL`, targetRepo, ref).Scan(&theirs)
		if err == nil {
			res.Files = append(res.Files, AssignedFile{Path: src, Role: "schedule", Action: "usesOwn", Differs: theirs != hash})
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		files, err := read(src)
		if err != nil {
			return nil, err
		}
		var sy ScheduleYAML
		if yaml.Unmarshal(files[0].Content, &sy) == nil {
			for _, c := range append(append([]string{}, sy.Spec.SkipCalendars...), sy.Spec.OnlyCalendars...) {
				calendars[c] = true
			}
		}
		content, err := stampYAML(files[0].Content, origin(src), nil)
		if err != nil {
			return nil, refuse(http.StatusUnprocessableEntity, "origin_unreadable", "%s: %v", src, err)
		}
		add(files[0], "schedule", content)
	}

	// Every YAML document the commit adds is one the target's sync can read.
	for _, p := range plan {
		yamlFile := strings.HasSuffix(p.write.Path, ".yaml") || strings.HasSuffix(p.write.Path, ".yml")
		if !yamlFile || p.role == "script" && !isScriptDocument(p.write.Content) {
			continue // a script's body, or a file of its project: not a document of ours
		}
		verrs, err := validateYAMLBytes(p.write.Path, p.write.Content)
		if err != nil {
			return nil, err
		}
		if len(verrs) > 0 {
			msgs := make([]string, len(verrs))
			for i, v := range verrs {
				msgs[i] = v.Error()
			}
			return nil, refuse(http.StatusUnprocessableEntity, "validation_failed", "%s: %s", p.write.Path, strings.Join(msgs, "; "))
		}
	}

	if err := r.assignLacks(ctx, res, req.TargetAgency, scope, &spec, calendars, job.uid, scriptUID); err != nil {
		return nil, err
	}

	msg := strings.TrimSpace(req.CommitMessage)
	if msg == "" {
		msg = fmt.Sprintf("cronomicon: assign %s from %s", job.name, originName)
	}
	writes := make([]FileWrite, len(plan))
	for i, p := range plan {
		writes[i] = p.write
	}
	commit, err := targetSvc.CommitFiles(ctx, writes, msg)
	var takenErr *PathsTakenError
	var pre *PreconditionError
	switch {
	case errors.As(err, &takenErr):
		e := refuse(http.StatusUnprocessableEntity, "name_taken", "%s's repository already has %s", targetName, strings.Join(takenErr.Paths, ", "))
		e.Paths = takenErr.Paths
		return nil, e
	case errors.As(err, &pre) && pre.Unsynced:
		targetSvc.TriggerSync(ctx, "poll")
		return nil, refuse(http.StatusConflict, "target_unsynced",
			"%s's repository has commits that had not been synced here. A sync has been started: assign again when it has finished", targetName)
	case err != nil:
		return nil, err
	}
	res.CommitSHA, res.Branch = commit.CommitSHA, commit.Branch

	// The record. The commit is in: a record that cannot be written is logged,
	// and the answer still says what was committed.
	res.AssignmentID = db.NewID()
	if err := r.recordAssignment(ctx, res, req, job.owner, job.repo, job.uid, job.name, job.path, targetRepo, scope, plan); err != nil {
		r.logWarn("assignment: committed but not recorded; no update will be offered for it", "assignment", res.AssignmentID, "error", err)
	}
	targetSvc.TriggerSync(ctx, "manual")
	res.SyncStarted = true
	return res, nil
}

// isScriptDocument reports whether content is a `kind: Script` document (and
// not a plain YAML file synthesised as a script).
func isScriptDocument(content []byte) bool {
	var head struct {
		Kind string `yaml:"kind"`
	}
	return yaml.Unmarshal(content, &head) == nil && head.Kind == "Script"
}

// planned is one file an assignment will write, and what the record keeps of
// it.
type planned struct {
	write      FileWrite
	role       string
	originPath string
	originHash string
}

// recordAssignment writes the record of a committed assignment.
func (r *Registry) recordAssignment(ctx context.Context, res *AssignResult, req AssignJobRequest, originAgency, originRepo, uid, name, path, targetRepo, scope string, plan []planned) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assignments (id, kind, origin_agency, origin_repo, origin_uid, origin_name, origin_path, origin_commit,
		                         target_agency, target_repo, target_path, target_scope, commit_sha, created_by, created_at)
		VALUES (?, 'job', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		res.AssignmentID, originAgency, originRepo, uid, name, path, res.OriginCommit,
		req.TargetAgency, targetRepo, path, scope, res.CommitSHA, req.Actor, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	for _, p := range plan {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assignment_files (assignment_id, target_path, origin_path, role, origin_hash, committed_hash)
			VALUES (?, ?, ?, ?, ?, ?)`,
			res.AssignmentID, p.write.Path, p.originPath, p.role, p.originHash, fileHash(p.write.Content)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// assignLacks fills what the job needs that the target agency does not have,
// and the bindings it must make again.
func (r *Registry) assignLacks(ctx context.Context, res *AssignResult, agency, scope string, spec *JobYAML, calendars map[string]bool, jobUID, scriptUID string) error {
	q := r.db
	has := func(query string, args ...any) (bool, error) {
		var ok bool
		err := q.QueryRowContext(ctx, `SELECT EXISTS (`+query+`)`, args...).Scan(&ok)
		return ok, err
	}
	lack := func(kind, name, query string, args ...any) error {
		ok, err := has(query, args...)
		if err != nil {
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

	if n := strings.TrimSpace(spec.Spec.BecomePasswordSecret); n != "" {
		if err := secret(n); err != nil {
			return err
		}
	}
	if n := strings.TrimSpace(spec.Spec.SSHCredential); n != "" {
		if err := key(n); err != nil {
			return err
		}
	}
	owners := []string{jobUID}
	if scriptUID != "" {
		owners = append(owners, scriptUID)
	}
	for _, owner := range owners {
		rows, err := q.QueryContext(ctx, `
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

	names := make([]string, 0, len(calendars))
	for c := range calendars {
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
			// this calendar, and the job with it, until the agency has one.
			res.Lacks[i].Why = "the agency's sync refuses the schedule naming it, and the job with it, until the agency has a calendar of this name; create it there and sync"
		}
	}
	for _, re := range spec.Spec.Reactions {
		name := strings.TrimSpace(re.OnName)
		if name == "" {
			continue
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
	if h := strings.TrimSpace(spec.Spec.TargetHost); h != "" {
		if err := lack("targetHost", h, `
			SELECT 1 FROM scope_hosts sh JOIN scopes sc ON sc.id = sh.scope_id
			 WHERE sc.name = ? AND sc.owner_agency = ? AND sh.host = ?`, scope, agency, h); err != nil {
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
