package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ResetSmith/cronomicon/internal/agencyctx"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/gitlab"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountScripts owns the Scripts catalog (B-Git, scripts-plan Phase 2).
//
// Scripts are the reusable executable unit: parsed from scripts/*.yaml in the
// GitLab clone at sync time and cached read-only (Git wins on read, §2.4). Jobs
// reference a script via script_ref. The body and definition are authored only
// through the Git publish flow. The lone write surface is user-authored TAGS
// (scripts-tags.md) — SQLite-only metadata that never round-trips to Git.
//
// Routes implemented:
//
//	GET  /api/v1/scripts
//	GET  /api/v1/scripts/{name}           (includes the usedBy reverse index)
//	GET  /api/v1/script-content/{name}    (the resolved body, read on demand)
//	PUT  /api/v1/script-tags/{name}       (set user-authored tags; SQLite-only)
func (s *Server) mountScripts(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	mux.Handle("GET /api/v1/scripts",
		requireSession(http.HandlerFunc(s.listScripts)))
	// Trailing-wildcard {name...} so a sub-folder script whose name is a repo-
	// relative path (e.g. ops-playbooks/site.yml) resolves: Go's ServeMux matches
	// {name...} against both literal and %2F-encoded slashes (the generated client
	// percent-encodes), and PathValue("name") returns the full multi-segment name.
	mux.Handle("GET /api/v1/scripts/{name...}",
		requireSession(http.HandlerFunc(s.getScript)))
	// Body content is RequireSession only (no elevated gate) per script-upgrade.md
	// §13-Q2 — bodies are served to any authenticated session, matching the Git
	// repo they sync from. A trailing wildcard must be the final path segment, so
	// content lives at its own /script-content/{name...} route rather than under
	// /scripts/{name...}/content (which is not a valid pattern).
	mux.Handle("GET /api/v1/script-content/{name...}",
		requireSession(http.HandlerFunc(s.getScriptContent)))
	// Tags are the catalog's only write surface and the only mutable field on the
	// otherwise read-only Script. Gate is session + CSRF only (scripts-tags.md D1:
	// any logged-in user; no role check). Same sibling-prefix + {name...} shape as
	// the content route (a trailing wildcard can't take a /tags suffix).
	mux.Handle("PUT /api/v1/script-tags/{name...}",
		s.auth.RequireSession(s.auth.RequireCSRF(http.HandlerFunc(s.updateScriptTags))))
}

// scriptBodyDisplayCap bounds the body the content endpoint pulls into memory and
// the response (1 MiB). Execution and content-hashing are uncapped — only display
// is bounded — so a giant script still runs but is shown truncated.
const scriptBodyDisplayCap = 1 << 20

// scriptContent is the GET /scripts/{name}/content response: a script's resolved
// body for catalog display. Inline command/script come from the DB column; a
// scriptPath is read on demand from the synced clone (path-guarded, capped). A
// non-UTF8 body returns encoding="binary" with content null.
type scriptContent struct {
	Content    *string `json:"content"`
	Encoding   string  `json:"encoding"` // utf8 | binary
	Truncated  bool    `json:"truncated"`
	ByteLength int     `json:"byteLength"`
	RunType    string  `json:"runType"`
}

// scriptUID resolves a script's NAME to its uid, in Global's repository.
//
// The script routes address a script by name, and since migration 1290 a name
// is unique per repository, not per installation. Until a route can say which
// repository it means (2.4.0, Phase R6) it means Global's, which is the only
// one an installation has. Every statement after this lookup keys on the uid,
// so that none of them can come to mean "every repository's script of this
// name" when a second repository arrives.
func (s *Server) scriptUID(r *http.Request, name string) (string, bool) {
	return s.scriptUIDIn(r, repoid.Global, name)
}

// scriptUIDIn is scriptUID for a named repository: the two routes that READ a
// script, body and all, say which repository they mean (?repo=, GR-26), and
// the caller has decided who may read it. The routes that WRITE a script's
// tags and bindings still mean Global's.
func (s *Server) scriptUIDIn(r *http.Request, repoID, name string) (string, bool) {
	var uid string
	err := s.db.QueryRowContext(r.Context(),
		`SELECT uid FROM scripts WHERE repo_id = ? AND name = ?`, repoID, name).Scan(&uid)
	return uid, err == nil && uid != ""
}

func (s *Server) getScriptContent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Which repository's script of the name (?repo=; none is Global's), and
	// whether the caller reads it (GR-26).
	repo, ok := s.readableRepo(w, r, r.URL.Query().Get("repo"))
	if !ok {
		return
	}

	var runType string
	var command, script, scriptPath sql.NullString
	err := s.db.QueryRowContext(r.Context(),
		`SELECT run_type, command, script, script_path FROM scripts WHERE repo_id = ? AND name = ?`, repo, name).
		Scan(&runType, &command, &script, &scriptPath)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "db_error", "could not load script")
		return
	}

	var (
		raw       []byte
		truncated bool
	)
	switch {
	case command.Valid && command.String != "":
		raw = []byte(command.String)
	case script.Valid && script.String != "":
		raw = []byte(script.String)
	case scriptPath.Valid && scriptPath.String != "":
		// From the objects of the last synced commit, as dispatch reads it, and
		// not from the working tree, which a sync rewrites in place (present
		// defect 27).
		data, tr, rerr := execspec.ReadSyncedFile(r.Context(), s.db, gitlab.CloneDirFor, repo, scriptPath.String, scriptBodyDisplayCap)
		if rerr != nil {
			// A6/A3: never echo the absolute resolved path or 500 on a cold-start /
			// renamed / unsynced file — map every read failure to a clean 409.
			httpx.Fail(w, http.StatusConflict, "script_unreadable",
				"script file could not be read from the synced repository")
			return
		}
		raw, truncated = data, tr
	default:
		raw = []byte{} // malformed row with no body — show as empty
	}

	resp := scriptContent{Truncated: truncated, ByteLength: len(raw), RunType: runType}
	if utf8.Valid(raw) {
		body := string(raw)
		resp.Content = &body
		resp.Encoding = "utf8"
	} else {
		resp.Encoding = "binary" // content stays null
	}
	httpx.JSON(w, http.StatusOK, resp)
}

// scriptUsedByRef is one entry of the identity-bearing reverse index (R2F-3).
type scriptUsedByRef struct {
	Name     string   `json:"name"`
	UID      string   `json:"uid,omitempty"`
	Source   string   `json:"source,omitempty"`
	Agencies []string `json:"agencies,omitempty"`
}

// scriptRow is the JSON shape of the Script schema in openapi.yaml. usedBy is
// detail-only (omitted from list rows); usedByCount is on both.
type scriptRow struct {
	Name string `json:"name"`
	// RepoID is the repository the script is of (2.4.0): a name is a script in
	// each repository that has one. `global` for Global's.
	RepoID      string  `json:"repoId"`
	Description *string `json:"description,omitempty"`
	RunType     string  `json:"runType"`
	Command     *string `json:"command,omitempty"`
	Script      *string `json:"script,omitempty"`
	ScriptPath  *string `json:"scriptPath,omitempty"`
	// SourceKind is a server-derived one-word tag of which body a row carries —
	// command | script | file — so the Scripts list can badge rows without
	// shipping the (blob) command/script columns (CC.11). Detail rows set it too
	// for the on-expand body fetch; empty ⇒ none.
	SourceKind  string  `json:"sourceKind,omitempty"`
	ContentHash string  `json:"contentHash"`
	SourcePath  *string `json:"sourcePath,omitempty"`
	SyncedAt    string  `json:"syncedAt"`
	// createdAt/lastModifiedAt are always null in Phase A (V1.1-17): the scripts
	// table has no provenance columns yet — migration 200 + the git-history walk
	// that populates them is deferred to V2 (Phase B). The fields are on the wire
	// now so the frontend column lands once, and serialize null until then.
	CreatedAt      *string  `json:"createdAt"`
	LastModifiedAt *string  `json:"lastModifiedAt"`
	UsedByCount    int      `json:"usedByCount"`
	UsedBy         []string `json:"usedBy,omitempty"`
	// R2F-3 — the same jobs as UsedBy, in the same order, carrying the identity
	// and derived agencies a display needs to qualify two same-named jobs.
	//
	// A PARALLEL field rather than a reshape of UsedBy: that is a documented
	// array of strings, and turning it into an array of objects would break every
	// existing consumer for a display nicety. UsedBy stays the plain-name list it
	// has always been; a client that wants to disambiguate reads this instead.
	UsedByRefs []scriptUsedByRef `json:"usedByRefs,omitempty"`
	// Warnings are body-lint findings from the sync-time scan (migration 210),
	// always serialized (never omitempty) as a JSON array — [] when clean.
	Warnings []scanWarning `json:"warnings"`
	// Variables are the env vars the body references, extracted at sync (migration
	// 230). Always serialized (never omitempty) as a JSON array — [] when the body
	// references none, or for languages not yet analyzed (ansible/terraform).
	Variables []scriptVariable `json:"variables"`
	// Prompts are the run inputs the script AUTHOR DECLARED in spec.prompts (migration
	// 651, JR-Q6) — label/required/default/options an operator actually sees. Distinct
	// from Variables above, which is the heuristic body extraction used only as an
	// authoring aid (UDV5): declared beats inferred (UDV1). The Job Composer seeds a
	// new job's Run inputs from these. Always serialized ([] when none declared).
	Prompts []gitlab.PromptSpec `json:"prompts"`
	// Tags are user-authored, SQLite-only labels (migration 280, scripts-tags.md).
	// Unlike every other field they are WRITABLE (PUT /script-tags) and are NOT
	// derived from Git — they persist across syncs. Always serialized ([] when none).
	Tags []string `json:"tags"`
}

// scriptVariable is the wire shape of one referenced env variable, mirroring
// gitlab.Variable (the producer) byte-for-byte so the persisted JSON round-trips
// unchanged. A non-null default ⇒ optional (the body supplies a fallback); a null
// default ⇒ required (the operator must provide it).
type scriptVariable struct {
	Name    string  `json:"name"`
	Default *string `json:"default,omitempty"`
	Line    int     `json:"line,omitempty"`
}

// parseVariables decodes the scripts.variables JSON-array column into a non-nil
// slice (modeled on parseWarnings): empty / "[]" / malformed → []scriptVariable{}.
func parseVariables(raw string) []scriptVariable {
	if raw == "" || raw == "[]" {
		return []scriptVariable{}
	}
	var vs []scriptVariable
	if err := json.Unmarshal([]byte(raw), &vs); err != nil || vs == nil {
		return []scriptVariable{}
	}
	return vs
}

// parseScriptPrompts decodes the scripts.prompts_json column into a non-nil slice
// (modeled on parseVariables): empty / "[]" / malformed → []gitlab.PromptSpec{}.
// Advisory data — a malformed list degrades to "declares nothing", never an error.
func parseScriptPrompts(raw string) []gitlab.PromptSpec {
	if raw == "" || raw == "[]" {
		return []gitlab.PromptSpec{}
	}
	var ps []gitlab.PromptSpec
	if err := json.Unmarshal([]byte(raw), &ps); err != nil || ps == nil {
		return []gitlab.PromptSpec{}
	}
	return ps
}

// scripts.tags is decoded by the shared parseTags helper (execution_mount.go),
// the same JSON-array → []string{} decoder used for jobs.tags.

// scanWarning is the wire shape of one body-lint finding, mirroring gitlab.Warning
// (the producer) byte-for-byte so the persisted JSON round-trips unchanged.
type scanWarning struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Line     int    `json:"line,omitempty"`
}

// parseWarnings decodes the scripts.warnings JSON-array column into a non-nil
// slice (modeled on parseTags): empty / "[]" / malformed → []scanWarning{}.
func parseWarnings(raw string) []scanWarning {
	if raw == "" || raw == "[]" {
		return []scanWarning{}
	}
	var ws []scanWarning
	if err := json.Unmarshal([]byte(raw), &ws); err != nil || ws == nil {
		return []scanWarning{}
	}
	return ws
}

func (s *Server) listScripts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	typeFilter := q.Get("type")
	search := q.Get("q")
	tagFilter := tagutil.ParseQuery(q)
	page, pageSize := pageParams(q)

	// Filters are shared by the count and the page query.
	where := " WHERE 1=1"
	var args []any
	if typeFilter != "" {
		where += " AND run_type = ?"
		args = append(args, typeFilter)
	}
	if search != "" {
		where += " AND name LIKE ?"
		args = append(args, "%"+search+"%")
	}
	if frag, fargs := tagFilter.SQLFilter("tags"); frag != "" {
		where += " AND " + frag
		args = append(args, fargs...)
	}

	// GR-26: the scripts of Global's repository, and of the repositories of the
	// agencies the caller is of. The list had no filter: with a second
	// repository it showed every agency's scripts to every session.
	// By the script's repository's agency (CF-1, 2.5.0 C2).
	if frag, fargs := agencyReadFilter(r, agencyid.ScriptOwnerSQL("s")); frag != "" {
		where += frag
		args = append(args, fargs...)
	}
	// 2.4.4: the agency the request works in, on top of what may be read.
	if frag, fargs := agencyctx.Narrow(r, agencyid.ScriptOwnerSQL("s")); frag != "" {
		where += frag
		args = append(args, fargs...)
	}
	if repo := q.Get("repo"); repo != "" {
		where += " AND s.repo_id = ?"
		args = append(args, repo)
	}

	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM scripts s"+where, args...).Scan(&total)

	// used_by is a correlated subquery so the whole list is one round-trip (no
	// per-row follow-up query, no nested-iterator pool deadlock). The (blob)
	// command/script columns are projected OUT of the list query (CC.11): a
	// derived source_kind badges the row, and the body is fetched on demand from
	// /script-content when a row is expanded. getScript still returns the blobs.
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.name, s.description, s.run_type, s.script_path,
		       s.executor, s.content_hash, s.source_path, s.synced_at, s.warnings, s.variables, s.tags,
		       s.prompts_json,
		       CASE
		           WHEN s.command IS NOT NULL AND s.command <> '' THEN 'command'
		           WHEN s.script IS NOT NULL AND s.script <> '' THEN 'script'
		           WHEN s.script_path IS NOT NULL AND s.script_path <> '' THEN 'file'
		           ELSE ''
		       END AS source_kind,
		       (SELECT COUNT(*) FROM jobs j WHERE j.script_uid = s.uid) AS used_by,
		       s.repo_id
		FROM scripts s`+where+`
		ORDER BY s.name, s.repo_id LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	items := []scriptRow{}
	for rows.Next() {
		sr, err := scanScriptListRow(rows)
		if err != nil {
			continue
		}
		items = append(items, sr)
	}

	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getScript(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	// Which repository's script of the name (?repo=; none is Global's), and
	// whether the caller reads it (GR-26): this answer carries the body.
	repo, ok := s.readableRepo(w, r, r.URL.Query().Get("repo"))
	if !ok {
		return
	}
	uid, found := s.scriptUIDIn(r, repo, name)
	if !found {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	row := s.db.QueryRowContext(r.Context(), `
		SELECT name, description, run_type, command, script, script_path,
		       executor, content_hash, source_path, synced_at, warnings, variables, tags, prompts_json, 0, repo_id
		FROM scripts WHERE uid = ?`, uid)
	sr, err := scanScript(row)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	sr.SourceKind = deriveSourceKind(sr.Command, sr.Script, sr.ScriptPath)

	// usedBy reverse index: every job that references THIS script, by its uid.
	// By name it would also list the jobs of another repository's script of the
	// same name.
	//
	// CF-7 (2.5.0 C2): only the jobs the caller reads are named; usedByCount
	// counts them all, so the others are a number and not names.
	sr.UsedBy = []string{}
	sr.UsedByRefs = []scriptUsedByRef{}
	total := 0
	jrows, err := s.db.QueryContext(r.Context(),
		`SELECT name, COALESCE(uid,''), source,
		        (SELECT GROUP_CONCAT(a.name, '\x1f')
		           FROM scope_agencies sa
		           JOIN agencies a ON a.id = sa.agency_id
		           JOIN scopes   sc ON sc.id = sa.scope_id
		          WHERE sc.name = jobs.scope) AS agencies,
		        `+agencyid.JobOwnerSQL("jobs")+`
		   FROM jobs WHERE script_uid = ? ORDER BY name`, uid)
	if err == nil {
		defer jrows.Close()
		for jrows.Next() {
			var jn, uid, src, owner string
			var agencies sql.NullString
			if jrows.Scan(&jn, &uid, &src, &agencies, &owner) == nil {
				total++
				if !readsAgency(r, owner) {
					continue
				}
				sr.UsedBy = append(sr.UsedBy, jn)
				sr.UsedByRefs = append(sr.UsedByRefs, scriptUsedByRef{
					Name: jn, UID: uid, Source: src, Agencies: splitAgencies(agencies),
				})
			}
		}
	}
	sr.UsedByCount = total

	httpx.JSON(w, http.StatusOK, sr)
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows so list and detail share
// one scan/mapping routine. The SELECT column order must match exactly.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanScript(sc rowScanner) (scriptRow, error) {
	var (
		sr                                        scriptRow
		description, command, script, scriptPath  sql.NullString
		executor, sourcePath, warnings, variables sql.NullString
		tags, prompts                             sql.NullString
	)
	if err := sc.Scan(&sr.Name, &description, &sr.RunType, &command, &script, &scriptPath,
		&executor, &sr.ContentHash, &sourcePath, &sr.SyncedAt, &warnings, &variables, &tags, &prompts, &sr.UsedByCount, &sr.RepoID); err != nil {
		return scriptRow{}, err
	}
	sr.Warnings = parseWarnings(warnings.String)
	sr.Variables = parseVariables(variables.String)
	sr.Prompts = parseScriptPrompts(prompts.String)
	sr.Tags = tagutil.Parse(tags.String)
	if description.Valid && strings.TrimSpace(description.String) != "" {
		sr.Description = &description.String
	}
	if command.Valid {
		sr.Command = &command.String
	}
	if script.Valid {
		sr.Script = &script.String
	}
	if scriptPath.Valid {
		sr.ScriptPath = &scriptPath.String
	}
	_ = executor // scripts.executor is stored and no longer served or read (LR-42)
	if sourcePath.Valid {
		sr.SourcePath = &sourcePath.String
	}
	return sr, nil
}

// scanScriptListRow scans a Scripts list row. It mirrors scanScript but for the
// blob-free list projection (CC.11): the command/script columns are absent and a
// derived source_kind column takes their place.
func scanScriptListRow(sc rowScanner) (scriptRow, error) {
	var (
		sr                        scriptRow
		description, scriptPath   sql.NullString
		executor, sourcePath      sql.NullString
		warnings, variables, tags sql.NullString
		prompts                   sql.NullString
		sourceKind                string
	)
	if err := sc.Scan(&sr.Name, &description, &sr.RunType, &scriptPath,
		&executor, &sr.ContentHash, &sourcePath, &sr.SyncedAt, &warnings, &variables, &tags,
		&prompts, &sourceKind, &sr.UsedByCount, &sr.RepoID); err != nil {
		return scriptRow{}, err
	}
	sr.Warnings = parseWarnings(warnings.String)
	sr.Variables = parseVariables(variables.String)
	sr.Prompts = parseScriptPrompts(prompts.String)
	sr.Tags = tagutil.Parse(tags.String)
	if description.Valid && strings.TrimSpace(description.String) != "" {
		sr.Description = &description.String
	}
	if scriptPath.Valid {
		sr.ScriptPath = &scriptPath.String
	}
	_ = executor // scripts.executor is stored and no longer served or read (LR-42)
	if sourcePath.Valid {
		sr.SourcePath = &sourcePath.String
	}
	sr.SourceKind = sourceKind
	return sr, nil
}

// deriveSourceKind returns the one-word body tag used by the Scripts list badge,
// matching the source_kind CASE in listScripts. Detail responses compute it in
// Go from the blob columns they still carry.
func deriveSourceKind(command, script, scriptPath *string) string {
	switch {
	case command != nil && *command != "":
		return "command"
	case script != nil && *script != "":
		return "script"
	case scriptPath != nil && *scriptPath != "":
		return "file"
	default:
		return ""
	}
}

// Tag parsing/normalization + the D6 caps live in the shared tagutil package
// (CC.15), reused by every tagged entity across the api and settings packages.

// updateScriptTags sets a script's user-authored tags (scripts-tags.md). Tags are
// SQLite-only and are preserved across Git syncs (see gitlab.upsertScripts). The
// request is a full replace of the tag set. Gate is session + CSRF (D1: any
// logged-in user), wired in mountScripts, and here the script's agency: the
// answer carries the body, so a script the caller may not read is absent
// (2.5.0 C11, defect 71; GET /scripts/{name} answered 404 while this answered
// 200 with the body).
func (s *Server) updateScriptTags(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	uid, found := s.scriptUID(r, name)
	if !found || !readsAgency(r, s.ownerBy(r.Context(), `SELECT `+agencyid.ScriptOwnerSQL("sx")+` FROM scripts sx WHERE sx.uid = ?`, uid)) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}

	// Decode/normalize/UPDATE + RowsAffected==0 → 404 is the shared skeleton
	// (CC.12); the audit + re-fetch tail below stays inline (it differs per entity).
	if _, ok := s.writeTagsUpdate(w, r, "scripts", "uid = ?", "script not found", uid); !ok {
		return
	}

	// Audit trail (mirrors the compose write path).
	actor := ""
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		actor = id.Email
	}
	agency := s.ownerBy(r.Context(), `SELECT `+agencyid.ScriptOwnerSQL("sx")+` FROM scripts sx WHERE sx.uid = ?`, uid) // the script's repository's (C1)
	_ = workflow.InsertChangeLog(r.Context(), s.db, agency, actor, "Scripts", "Updated", name, "Script tags updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: agency, Kind: "config", Actor: actor, Category: "Scripts", Action: "Updated", Target: name,
	})

	// Return the updated script (same shape as GET /scripts/{name}, with an
	// accurate usedByCount; usedBy stays detail-only and is omitted here).
	row := s.db.QueryRowContext(r.Context(), `
		SELECT s.name, s.description, s.run_type, s.command, s.script, s.script_path,
		       s.executor, s.content_hash, s.source_path, s.synced_at, s.warnings, s.variables, s.tags,
		       s.prompts_json,
		       (SELECT COUNT(*) FROM jobs j WHERE j.script_uid = s.uid) AS used_by,
		       s.repo_id
		FROM scripts s WHERE s.uid = ?`, uid)
	sr, err := scanScript(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Pruned between the UPDATE and the re-read (a vanishingly small window).
		httpx.Fail(w, http.StatusNotFound, "not_found", "script not found")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, sr)
}
