package api

import (
	"encoding/json"
	"net/http"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
)

// Tags on Env Vars, Secrets, and SSH key credentials (migration 470) mirror the
// operator-authored tag pattern of the Scripts/Jobs/Schedules/Workflows catalogs
// (scripts-tags.md / tags-support.md): a full-replace PUT that normalizes the set
// server-side (reusing tagutil.Normalize + the shared caps) and stores a JSON
// array in a SQLite-only `tags` column.
//
// The one deliberate DIFFERENCE from the catalog tag endpoints: those gate on
// session + CSRF only (any logged-in user) because they annotate a read-only Git
// catalog. These three entities are amadeus-owned and already permission-gated on
// every mutation, so the tag write carries the SAME permission as the entity's
// other writes — ManageEnvVars for variables + secrets, ConfigureApp for SSH key
// credentials. The routes are registered in mountSettings (settings_mount.go).
//
// Audit follows each entity's OWN value-write path, not one uniform rule: the
// env-var value write emits change_log + activity (settings.audit), so its tag
// write does too (settings.Audit); the secret and SSH-credential value writes are
// change_log-only (mountWriteChangeLog), so their tag writes match that. The full
// audit trail is always written — only the user-visible activity feed differs.
//
// Each handler runs a single standalone `UPDATE <table> SET tags=? WHERE id=?`
// (the table is a compile-time constant, never user input) and maps
// RowsAffected==0 to 404 — no pre-UPDATE existence COUNT, so a row pruned between
// request and write yields a clean 404 with no phantom audit row, and the lone
// statement keeps the SQLite pool deadlock-safe. Tags never touch the encrypted
// envelope of a secret/credential, so the crypto/reveal/rotate paths are untouched.
//
// RUNNERS ARE THE ONE EXCEPTION to that last paragraph, since RT-1: their tags
// are read by the runner claim query, so the write also maintains the
// `runner_tags` projection and therefore needs a transaction. See
// writeRunnerTagsUpdate for why the two must move together.

// decodeTagsBody decodes + normalizes a {tags:[...]} full-replace body. On any
// problem it writes the 422 and returns ok=false. Tags is a pointer so an
// absent/null field is a 422 (the contract marks it required) — not a silent,
// destructive clear; an explicit [] is the documented clear.
func decodeTagsBody(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var in struct {
		Tags *[]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "invalid request body")
		return nil, false
	}
	if in.Tags == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", "tags is required")
		return nil, false
	}
	tags, err := tagutil.Normalize(*in.Tags)
	if err != nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return nil, false
	}
	return tags, true
}

func tagsActor(r *http.Request) string {
	if id, ok := auth.IdentityFrom(r.Context()); ok {
		return id.Email
	}
	return ""
}

// writeTagsUpdate is the shared decode→normalize→UPDATE→404 prefix of every
// "update tags" handler (CC.12), across all seven tagged entities. It writes the
// normalized set to <table> under <whereSQL>/<whereArgs> and maps
// RowsAffected==0 to a 404 with notFound. On any error/validation/404 it writes
// the response itself and returns ok=false; on success it returns the normalized
// tags so each caller can run its own — deliberately divergent — re-fetch,
// audit, and respond tail (the per-entity permission/audit differences documented
// in this file's header live in those tails, not here). table and whereSQL are
// always compile-time constants, never user input, and the lone standalone
// UPDATE keeps the SQLite pool deadlock-safe.
func (s *Server) writeTagsUpdate(w http.ResponseWriter, r *http.Request, table, whereSQL, notFound string, whereArgs ...any) ([]string, bool) {
	tags, ok := decodeTagsBody(w, r)
	if !ok {
		return nil, false
	}
	tagsJSON, _ := json.Marshal(tags)
	args := append([]any{string(tagsJSON)}, whereArgs...)
	res, err := s.db.ExecContext(r.Context(), "UPDATE "+table+" SET tags = ? WHERE "+whereSQL, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", notFound)
		return nil, false
	}
	return tags, true
}

// updateEnvVarTags sets an env var's operator-authored tags (full replace).
func (s *Server) updateEnvVarTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("envVarId")
	// M4: enforce WRITE-scope before labeling (404 out-of-scope; 403 global) — mirror
	// the secret-tags gate so a restricted manager can't label an out-of-reach var.
	actor, _ := auth.IdentityFrom(r.Context())
	if _, ok := s.loadEnvVarWritable(w, r, id, actor); !ok {
		return
	}
	// RB-32 (RF-5): tags are a write on the row; departmental gate, RB-Q14 for
	// unmembered rows.
	if !s.requireEntityAgency(w, r, actor, auth.PermManageEnvVars, "env_var_agencies", "env_var_id", id, "variable") {
		return
	}
	if _, ok := s.writeTagsUpdate(w, r, "env_vars", "id = ?", "env var not found", id); !ok {
		return
	}
	ev, err := settings.GetEnvVar(r.Context(), s.db, id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if ev == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "env var not found")
		return
	}
	// settings.Audit (change_log + activity), NOT mountWriteChangeLog: the env-var
	// VALUE write (settings.UpdateEnvVar) emits an activity row, so a tag edit must
	// surface in the activity feed the same way a value edit does. The secret/SSH
	// tag handlers below deliberately stay change_log-only because THEIR value
	// writes are change_log-only too — each tag write matches its own entity.
	settings.Audit(r.Context(), s.db, tagsActor(r), "Env Vars", "updated", ev.Key, "")
	httpx.JSON(w, http.StatusOK, ev)
}

// updateSecretTags sets a secret's operator-authored tags (full replace). Tags are
// plaintext metadata and never touch the encrypted envelope.
func (s *Server) updateSecretTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("secretId")
	actor, _ := auth.IdentityFrom(r.Context())
	sec := secrets.New(s.db, s.cfg, s.log)
	// D3/H3: enforce WRITE-scope before writing the tags (404 out-of-scope; 403 on a
	// global row) — a scope-restricted manager must not label a secret outside their
	// reach, nor a global secret it cannot otherwise write.
	if _, ok := s.loadSecretWritable(w, r, sec, id, actor); !ok {
		return
	}
	// RB-32 (RF-5): same departmental gate as the secret value write.
	if !s.requireEntityAgency(w, r, actor, auth.PermManageEnvVars, "secret_agencies", "secret_id", id, "secret") {
		return
	}
	if _, ok := s.writeTagsUpdate(w, r, "secrets", "id = ?", "secret not found", id); !ok {
		return
	}
	sc, err := sec.Get(r.Context(), id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if sc == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "secret not found")
		return
	}
	s.mountWriteChangeLog(r, "Secrets", "updated", sc.Key, tagsActor(r))
	httpx.JSON(w, http.StatusOK, sc)
}

// updateCredentialTags sets an SSH key credential's operator-authored tags (full
// replace). Tags are plaintext metadata and never touch the sealed key material.
func (s *Server) updateCredentialTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("credentialId")
	// RB-16 (RF-5): this handler previously had NO ownership guard at all —
	// key-material writes are agency-gated, so the metadata beside them is too.
	actor, _ := auth.IdentityFrom(r.Context())
	if !s.requireEntityAgency(w, r, actor, auth.PermConfigureApp, "ssh_credential_agencies", "credential_id", id, "SSH key") {
		return
	}
	if _, ok := s.writeTagsUpdate(w, r, "ssh_credentials", "id = ?", "ssh credential not found", id); !ok {
		return
	}
	c, err := sshkeys.New(s.db, s.cfg, s.log).Get(r.Context(), id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if c == nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "ssh credential not found")
		return
	}
	s.mountWriteChangeLog(r, "SSH Keys", "updated", c.Label, tagsActor(r))
	httpx.JSON(w, http.StatusOK, c)
}

// updateRunnerTags sets a runner's operator-authored tags (full replace,
// migration 580). Runners are self-registered agents (not a Git catalog), so
// tags are plain SQLite metadata with no sync-preservation concern. Addressed by
// UUID id. Gated ConfigureApp + CSRF at the route (mountRunners), matching the
// sibling runner↔agency membership write. Change-log-only audit — a runner is
// infrastructure, and its tag edit needn't flood the activity feed.
// writeRunnerTagsUpdate is the runner-only transactional twin of
// writeTagsUpdate. Runner tags are the ONE tagged entity whose tags are read by
// dispatch (RT-1, mig. 1070): claimRun probes the `runner_tags` projection
// rather than json_each(runners.tags), because a correlated JSON parse in the
// hottest query in the system is the shape that measured +350% for the agency
// predicate (mig. 690 header).
//
// That projection is why this handler cannot use the shared helper. The other
// six tagged entities write one standalone UPDATE — deliberately, for the SQLite
// pool's sake (see this file's header). Here the column and its projection must
// move together or the fleet silently dispatches on stale tags: a half-applied
// write would leave a runner claiming work for a tag it no longer carries, or
// refusing work for one it does. One transaction, column first (it stays
// authoritative), projection rebuilt from the same normalized set.
//
// Full replace, matching the endpoint's contract: DELETE then INSERT rather than
// a diff. The row counts here are single digits.
func (s *Server) writeRunnerTagsUpdate(w http.ResponseWriter, r *http.Request, runnerID string) ([]string, bool) {
	tags, ok := decodeTagsBody(w, r)
	if !ok {
		return nil, false
	}
	tagsJSON, _ := json.Marshal(tags)

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(r.Context(),
		`UPDATE runners SET tags = ? WHERE id = ?`, string(tagsJSON), runnerID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	// No pre-UPDATE existence check, same as the shared helper: a runner
	// deregistered between request and write is a clean 404 with no phantom audit
	// row and — because we roll back — no orphaned projection rows either.
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return nil, false
	}
	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM runner_tags WHERE runner_id = ?`, runnerID); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	for _, t := range tags {
		// tagutil.Normalize cannot emit "", but the claim predicate treats an empty
		// runner_tag as "unpinned", so an empty row here would be a tag that matches
		// nothing and confuses the RT-G5 fleet count. Cheap to exclude, so exclude it.
		if t == "" {
			continue
		}
		if _, err := tx.ExecContext(r.Context(),
			`INSERT OR IGNORE INTO runner_tags (runner_id, tag) VALUES (?, ?)`, runnerID, t); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return nil, false
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return nil, false
	}
	return tags, true
}

func (s *Server) updateRunnerTags(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runnerId")
	tags, ok := s.writeRunnerTagsUpdate(w, r, id)
	if !ok {
		return
	}
	// Re-read the name for the audit target and response. RowsAffected>0 proved the
	// row existed a moment ago; a vanished row here is a 404, not a 500.
	var name string
	if err := s.db.QueryRowContext(r.Context(), `SELECT name FROM runners WHERE id = ?`, id).Scan(&name); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	s.mountWriteChangeLog(r, "Runners", "updated", name, tagsActor(r))
	httpx.JSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "tags": tags})
}
