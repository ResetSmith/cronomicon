package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/entitycode"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runner"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// Revision history + recycle bin for cronomicon-source definitions
// (RH, the prod-features plan §4).
//
// Git-source definitions get history and undelete from Git. In-app ones had
// neither: edited in place, deleted hard, with an audit log that records that a
// change happened but not what it was.
//
// # Two mechanisms, deliberately separate
//
//   - A REVISION is an append-only snapshot of the compose input, written
//     inside the same transaction as the write it describes. Restoring one
//     re-submits it through the ordinary write path.
//   - The RECYCLE BIN is `deleted_at` on the definition row. Restoring from it
//     is a single UPDATE that clears the stamp — the row was never taken apart,
//     so nothing has to be rebuilt and nothing can be lost in the rebuilding.
//
// The second is why the soft delete does NOT remove definition_schedules or
// paused_jobs, even though the AFTER DELETE triggers that normally cascade them
// do not fire for an UPDATE. Destroying bindings on the way in would make an
// undelete lossy. Instead every execution path filters `deleted_at IS NULL`;
// see deleted_filter_sites_test.go for the enumeration that keeps that honest —
// it exercises every route that ACTS on a definition against a binned one, and
// carries the reasoned exemption list for the routes that legitimately do not
// refuse (kill, the bin's own routes, tags, the list routes' filter-not-refuse).
//
// That enumeration was promised here from the start and written only in FX-A5,
// which is why the gap it covers was found twice by review instead: v0.57.32
// caught the workflow trigger/pause/name routes, FX-A caught GET /workflows/{id},
// the reaction writes, and a workflow STEP running a binned job.
//
//	GET  /api/v1/definitions/{kind}/{name}/revisions        the history
//	POST /api/v1/definitions/{kind}/{name}/revisions/{no}/restore
//	GET  /api/v1/recycle-bin                                what is binned
//	POST /api/v1/recycle-bin/{kind}/{name}/restore          un-bin it
//	DELETE /api/v1/recycle-bin/{kind}/{name}                purge it now
//
// SO-2 (2.5.0 C3): each is compose on the DEFINITION's agency, as writing the
// definition is. They were a global administrator's (requireComposeAdmin) while
// a definition could be shared; since §0a a definition is one agency's, and a
// name is one agency's (I2), so every route here resolves a name among the
// definitions the caller may compose on (composableByName): another agency's
// definition of the name is neither found, counted nor touched (question 72).
func (s *Server) mountRevisions(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/definitions/{kind}/{name}/revisions",
		s.requireCompose(http.HandlerFunc(s.listRevisions)))
	mux.Handle("POST /api/v1/definitions/{kind}/{name}/revisions/{no}/restore",
		s.requireCompose(http.HandlerFunc(s.restoreRevision)))
	mux.Handle("GET /api/v1/recycle-bin",
		s.requireCompose(http.HandlerFunc(s.listRecycleBin)))
	mux.Handle("POST /api/v1/recycle-bin/{kind}/{name}/restore",
		s.requireCompose(http.HandlerFunc(s.restoreFromRecycleBin)))
	mux.Handle("DELETE /api/v1/recycle-bin/{kind}/{name}",
		s.requireCompose(http.HandlerFunc(s.purgeFromRecycleBin)))
}

// composedDef is one app-made definition of a name: its identity, its agency,
// and whether it is in the recycle bin.
type composedDef struct {
	uid, owner string
	binned     bool
}

// composableByName is the app-made definitions of kind called name that the
// caller may compose on (compose on the definition's agency, SO-2), and how
// many definitions hold the name in all (an old revision with no uid is read
// only when the name is one definition's).
func (s *Server) composableByName(r *http.Request, kind, name string) (mine []composedDef, all int, err error) {
	table, alias, expr := definitionOwner(kind)
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT COALESCE(`+alias+`.uid, ''), `+expr+`, `+alias+`.deleted_at IS NOT NULL FROM `+table+` `+alias+`
		  WHERE `+alias+`.source = 'cronomicon' AND `+alias+`.name = ?`, name)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	id, _ := auth.IdentityFrom(r.Context())
	for rows.Next() {
		var d composedDef
		if err := rows.Scan(&d.uid, &d.owner, &d.binned); err != nil {
			return nil, 0, err
		}
		all++
		if id.CanAgency(auth.PermCompose, d.owner) {
			mine = append(mine, d)
		}
	}
	return mine, all, rows.Err()
}

// Definition kinds that carry revisions. The strings match the migration's CHECK.
const (
	revKindJob      = "job"
	revKindWorkflow = "workflow"
	revKindSchedule = "schedule"
)

// Revision actions, matching the migration's CHECK.
const (
	revActionCreated  = "created"
	revActionUpdated  = "updated"
	revActionDeleted  = "deleted"
	revActionRestored = "restored"
)

// revKindTable maps a kind to its definition table. Also the allowlist: a kind
// absent here never reaches SQL.
var revKindTable = map[string]string{
	revKindJob:      "jobs",
	revKindWorkflow: "workflows",
	revKindSchedule: "schedules",
}

// revKindCategory maps a kind to its audit category, matching what the compose
// mounts already write.
var revKindCategory = map[string]string{
	revKindJob:      "Jobs",
	revKindWorkflow: "Workflows",
	revKindSchedule: "Schedules",
}

// snapshotRevision appends one revision inside the caller's transaction.
//
// ⚠️ It takes a *sql.Tx, not the pool, on purpose. The compose mounts' audit
// epilogue runs POST-commit against s.db; a snapshot written there could fail
// and leave a committed definition with no record of what it replaced — which
// is the failure this feature exists to prevent. This is the same argument
// entitycode.Allocate's placement comment makes in job_compose_mount.go, and it
// is why the call sites sit next to that one rather than next to the changelog.
//
// A snapshot identical to the definition's latest is skipped, so opening an
// editor and pressing Save with no edits does not manufacture history.
func snapshotRevision(ctx context.Context, tx *sql.Tx, kind, source, name, uid, actor, action string, payload any) error {
	if _, ok := revKindTable[kind]; !ok {
		return fmt.Errorf("snapshotRevision: unknown kind %q", kind)
	}
	blob, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("snapshotRevision: marshal: %w", err)
	}
	sum := sha256.Sum256(blob)
	digest := hex.EncodeToString(sum[:])

	var lastNo int64
	var lastDigest string
	// R2-5: the revision chain is per-IDENTITY. Two same-named siblings each
	// keep their own monotonic history rather than interleaving one.
	err = tx.QueryRowContext(ctx, `
		SELECT revision_no, snapshot_digest FROM definition_revisions
		 WHERE kind = ? AND uid = ?
		 ORDER BY revision_no DESC LIMIT 1`, kind, uid).Scan(&lastNo, &lastDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("snapshotRevision: read latest: %w", err)
	}
	// Only an unchanged UPDATE collapses. A delete or a restore is an event in
	// its own right even when the payload is byte-identical to the revision
	// before it — that is precisely the pair a reader needs to see.
	if action == revActionUpdated && lastDigest == digest {
		return nil
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO definition_revisions
			(id, kind, source, name, revision_no, action, actor, created_at, snapshot_json, snapshot_digest,
			 uid)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		db.NewID(), kind, source, name, lastNo+1, action, actor,
		time.Now().UTC().Format(time.RFC3339), string(blob), digest, uid)
	if err != nil {
		return fmt.Errorf("snapshotRevision: insert: %w", err)
	}
	return nil
}

// detachedBindingsFromTombstone reads the bindings deleteScheduleDef captured
// into the most recent tombstone for this schedule. A schedule binned before
// this was recorded (or one no definition referenced) simply yields none.
//
// THIS schedule: the one with this uid. A name outlives the schedule that had
// it: a schedule that was purged leaves its revisions, and a new schedule of
// the same name starts a chain of its own. Read by name alone, the restore of
// the new one could replay the OLD one's bindings (and now ties what it
// replays to the restored schedule's uid). A tombstone with no uid, from
// before revisions carried one, is still found by its name.
func detachedBindingsFromTombstone(ctx context.Context, tx *sql.Tx, name, uid string) ([]scheduleBinding, error) {
	var blob string
	err := tx.QueryRowContext(ctx, `
		SELECT snapshot_json FROM definition_revisions
		 WHERE kind = ? AND source = 'cronomicon' AND name = ? AND action = ?
		   AND (uid = ? OR uid IS NULL OR uid = '')
		 ORDER BY (uid = ?) DESC, revision_no DESC LIMIT 1`, revKindSchedule, name, revActionDeleted, uid, uid).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var payload struct {
		DetachedBindings []scheduleBinding `json:"detachedBindings"`
	}
	if err := json.Unmarshal([]byte(blob), &payload); err != nil {
		// A tombstone we cannot parse must not block the un-bin; the definition
		// still comes back, just without its bindings replayed.
		return nil, nil
	}
	return payload.DetachedBindings, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Revision history
// ─────────────────────────────────────────────────────────────────────────────

type revisionRow struct {
	RevisionNo int64           `json:"revisionNo"`
	Action     string          `json:"action"`
	Actor      string          `json:"actor"`
	CreatedAt  string          `json:"createdAt"`
	Digest     string          `json:"digest"`
	Snapshot   json.RawMessage `json:"snapshot"`
}

func (s *Server) listRevisions(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	if _, ok := revKindTable[kind]; !ok {
		httpx.Fail(w, http.StatusBadRequest, "validation", "kind must be job, workflow or schedule")
		return
	}
	// The history of the definitions of the name the caller composes on (SO-2),
	// by their identities: revision numbers are per definition (R2-5), so two
	// agencies' definitions of one name each have a revision 3. A revision from
	// before revisions carried a uid is read when the name is one definition's.
	// A global administrator for compose reads the name's whole history, a
	// purged definition's included, as before.
	q := `SELECT revision_no, action, actor, created_at, snapshot_digest, snapshot_json
		  FROM definition_revisions
		 WHERE kind = ? AND source = 'cronomicon' AND name = ?`
	args := []any{kind, name}
	if id, _ := auth.IdentityFrom(r.Context()); !id.GlobalAdmin(auth.PermCompose) {
		mine, all, derr := s.composableByName(r, kind, name)
		if derr != nil {
			httpx.Fail500(w, s.log, "db_error", derr)
			return
		}
		if len(mine) == 0 {
			httpx.Fail(w, http.StatusNotFound, "not_found", "no definition by that name")
			return
		}
		cond := " AND (uid IN (" + strings.TrimSuffix(strings.Repeat("?, ", len(mine)), ", ") + ")"
		for _, d := range mine {
			args = append(args, d.uid)
		}
		if all == 1 {
			cond += " OR COALESCE(uid, '') = ''"
		}
		q += cond + ")"
	}
	rows, err := s.db.QueryContext(r.Context(), q+`
		 ORDER BY revision_no DESC
		 LIMIT 100`, args...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	out := []revisionRow{}
	for rows.Next() {
		var it revisionRow
		var snap string
		if err := rows.Scan(&it.RevisionNo, &it.Action, &it.Actor, &it.CreatedAt, &it.Digest, &snap); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		it.Snapshot = json.RawMessage(snap)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": out})
}

// restoreRevision re-submits an older snapshot through the ordinary write path.
//
// It does NOT write the old row back directly. Going through writeComposed*
// means the restored definition is validated, audited, RBAC-checked and
// entity-code-allocated exactly as a hand-typed edit would be — and it means
// this handler cannot drift from the write path, because it IS the write path.
func (s *Server) restoreRevision(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	no, err := strconv.ParseInt(r.PathValue("no"), 10, 64)
	if _, ok := revKindTable[kind]; !ok || err != nil {
		httpx.Fail(w, http.StatusBadRequest, "validation", "bad kind or revision number")
		return
	}

	// R2-5 — the definition this revision belongs to, by identity, among those of
	// the name the caller composes on (SO-2, 2.5.0 C3; another agency's is not
	// counted). Multiplicity refuses generically.
	mine, all, derr := s.composableByName(r, kind, name)
	if derr != nil {
		httpx.Fail500(w, s.log, "db_error", derr)
		return
	}
	if len(mine) == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "no live definition by that name")
		return
	}
	if len(mine) > 1 {
		httpx.Fail(w, http.StatusConflict, "conflict", "more than one definition holds this name")
		return
	}
	liveUID := mine[0].uid
	// A binned definition must be restored from the recycle bin first: rewriting
	// its body while it is dead would leave a definition that is simultaneously
	// current and deleted.
	if mine[0].binned {
		httpx.Fail(w, http.StatusConflict, "conflict",
			"this definition is in the recycle bin — restore it from there before restoring a revision")
		return
	}
	// ITS revision of that number (they are numbered per definition), or one
	// from before revisions carried a uid when the name is one definition's.
	var snap string
	err = s.db.QueryRowContext(r.Context(), `
		SELECT snapshot_json FROM definition_revisions
		 WHERE kind = ? AND source = 'cronomicon' AND name = ? AND revision_no = ?
		   AND (uid = ? OR (? = 1 AND COALESCE(uid, '') = ''))
		 ORDER BY (uid = ?) DESC LIMIT 1`,
		kind, name, no, liveUID, all, liveUID).Scan(&snap)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such revision")
		return
	}
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}

	switch kind {
	case revKindJob:
		var in jobComposeInput
		if err := json.Unmarshal([]byte(snap), &in); err != nil {
			httpx.Fail500(w, s.log, "bad_snapshot", err)
			return
		}
		in.Name = name
		// RH/PF-Q8 — tags are operator-owned and SQLite-only: they deliberately
		// bypass Git-read-only and are edited through their own endpoint. The
		// compose upsert DOES write them, so a restore carrying the snapshot's
		// tags would silently revert tagging work done since. Splice the LIVE
		// values over the snapshot's instead.
		in.Tags = s.liveTags(r.Context(), "jobs", liveUID)
		s.writeComposedJob(w, r, in, id.Email, false, liveUID, s.jobAgencyFor(r.Context(), liveUID, "cronomicon", name))
	case revKindWorkflow:
		var in workflowComposeInput
		if err := json.Unmarshal([]byte(snap), &in); err != nil {
			httpx.Fail500(w, s.log, "bad_snapshot", err)
			return
		}
		in.Name = name
		// No tag splice here: unlike jobComposeInput, workflowComposeInput carries
		// no Tags field at all — workflow tags are written only through their own
		// endpoint, so the compose upsert cannot clobber them.
		//
		// A revision whose jobs are another agency's than the workflow runs now
		// makes it that agency's: a repair (question 46), whose two fields the
		// restore takes in an optional body.
		var rep repairInput
		if r.ContentLength != 0 {
			_ = json.NewDecoder(r.Body).Decode(&rep)
		}
		s.writeComposedWorkflow(w, r, in, id.Email, false, liveUID, rep)
	case revKindSchedule:
		var in scheduleInput
		if err := json.Unmarshal([]byte(snap), &in); err != nil {
			httpx.Fail500(w, s.log, "bad_snapshot", err)
			return
		}
		in.Name = name
		// Whose: the live schedule's owner, which the write keys on (I2). A
		// snapshot taken by an edit records none.
		_ = s.db.QueryRowContext(r.Context(), `SELECT owner_agency FROM schedules WHERE uid = ?`, liveUID).Scan(&in.OwnerAgency)
		s.writeComposedSchedule(w, r, in, id.Email, false)
	}
}

// liveTags reads a definition's current operator-owned tags, by its uid (a name
// may be two agencies' since I2; 2.5.0 C3), so a restore can
// preserve them rather than reverting them (PF-Q8).
func (s *Server) liveTags(ctx context.Context, table, uid string) []string {
	var raw string
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(tags,'[]') FROM `+table+` WHERE uid = ?`, uid).Scan(&raw); err != nil {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// Recycle bin
// ─────────────────────────────────────────────────────────────────────────────

type recycleBinRow struct {
	Kind      string  `json:"kind"`
	Name      string  `json:"name"`
	DeletedAt string  `json:"deletedAt"`
	DeletedBy string  `json:"deletedBy"`
	PurgeAt   *string `json:"purgeAt,omitempty"`
}

func (s *Server) listRecycleBin(w http.ResponseWriter, r *http.Request) {
	caller, _ := auth.IdentityFrom(r.Context())
	retentionDays := s.recycleBinRetentionDays(r.Context())
	out := []recycleBinRow{}
	for _, kind := range []string{revKindJob, revKindWorkflow, revKindSchedule} {
		// The definitions the caller composes on (SO-2, 2.5.0 C3): every
		// agency's binned rows were listed, to a global administrator only.
		table, alias, expr := definitionOwner(kind)
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT `+alias+`.name, `+alias+`.deleted_at, COALESCE(`+alias+`.deleted_by,''), `+expr+`
			  FROM `+table+` `+alias+`
			 WHERE `+alias+`.source='cronomicon' AND `+alias+`.deleted_at IS NOT NULL
			 ORDER BY `+alias+`.deleted_at DESC`)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		for rows.Next() {
			it := recycleBinRow{Kind: kind}
			var owner string
			if err := rows.Scan(&it.Name, &it.DeletedAt, &it.DeletedBy, &owner); err != nil {
				rows.Close()
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
			if !caller.CanAgency(auth.PermCompose, owner) {
				continue
			}
			// A purge date the operator can read is the difference between "it is
			// in the bin" and "you have four days".
			if retentionDays > 0 {
				if t, err := time.Parse(time.RFC3339, it.DeletedAt); err == nil {
					p := t.AddDate(0, 0, retentionDays).Format(time.RFC3339)
					it.PurgeAt = &p
				}
			}
			out = append(out, it)
		}
		rows.Close()
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"items":         out,
		"retentionDays": retentionDays,
	})
}

// restoreFromRecycleBin clears the tombstone.
//
// For jobs and workflows this is one UPDATE, because the soft delete never took
// the definition apart — its schedules, pauses, tags, reactions and entity code
// all survived untouched, and that is the payoff for filtering reads instead of
// deleting bindings.
//
// Schedules are the exception the header's rule has to admit: their runtime
// expansions are keyed by source_ref rather than cascaded by a trigger, so
// deleteScheduleDef removes them (a binned schedule must stop firing its
// referrers) — and those rows are the only place a referrer's schedule ref is
// stored. The delete captures them into its tombstone revision; this replays
// them, inside the same transaction as the un-bin so a restore cannot half
// happen.
func (s *Server) restoreFromRecycleBin(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	table, ok := revKindTable[kind]
	if !ok {
		httpx.Fail(w, http.StatusBadRequest, "validation", "kind must be job, workflow or schedule")
		return
	}

	// R2-5 — resolve WHICH binned row this name means, among the caller's
	// (SO-2, 2.5.0 C3: compose on its agency; another agency's binned row of the
	// name is neither restored nor counted). Two of the caller's own may both be
	// binned; restoring "whichever the engine finds first" would be a silent
	// pick, so multiplicity refuses with the generic wording. Each owner is the
	// definition's (a job's since 1400, a workflow's since 1360, a schedule's
	// since 1300), which is what its name collides within.
	mine, _, derr := s.composableByName(r, kind, name)
	if derr != nil {
		httpx.Fail500(w, s.log, "db_error", derr)
		return
	}
	var binnedUIDs, binnedOwners []string
	for _, d := range mine {
		if d.binned {
			binnedUIDs, binnedOwners = append(binnedUIDs, d.uid), append(binnedOwners, d.owner)
		}
	}

	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer tx.Rollback()
	if len(binnedUIDs) == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "nothing by that name is in the recycle bin")
		return
	}
	if len(binnedUIDs) > 1 {
		httpx.Fail(w, http.StatusConflict, "conflict", "more than one binned definition holds this name")
		return
	}
	restoredUID := binnedUIDs[0]
	// The name may have been legitimately reused in an overlapping pool while
	// this one sat in the bin — a restore is a create for uniqueness purposes.
	if kind == revKindJob {
		conflict, cerr := execspec.NamePoolConflict(r.Context(), s.db, "jobs", "cronomicon", name, binnedOwners[0], restoredUID)
		if cerr != nil {
			httpx.Fail500(w, s.log, "db_error", cerr)
			return
		}
		if conflict {
			httpx.Fail(w, http.StatusConflict, "conflict", execspec.NamePoolRefusal)
			return
		}
	}
	if kind == revKindWorkflow {
		conflict, cerr := execspec.WorkflowNamePoolConflict(r.Context(), s.db, "cronomicon", name, binnedOwners[0], restoredUID)
		if cerr != nil {
			httpx.Fail500(w, s.log, "db_error", cerr)
			return
		}
		if conflict {
			httpx.Fail(w, http.StatusConflict, "conflict", execspec.NamePoolRefusal)
			return
		}
	}
	res, err := tx.ExecContext(r.Context(),
		`UPDATE `+table+` SET deleted_at = NULL, deleted_by = NULL WHERE uid = ?`, restoredUID)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "nothing by that name is in the recycle bin")
		return
	}

	if kind == revKindSchedule {
		bindings, err := detachedBindingsFromTombstone(r.Context(), tx, name, restoredUID)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		owners, err := restoreScheduleBindings(r.Context(), tx, bindings, restoredUID)
		if err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		// The legacy display column mirrors the lowest-position entry, so it has
		// to be recomputed for every owner that just got its binding back —
		// exactly as the delete recomputed it on the way out.
		for _, o := range owners {
			if err := recomputeLegacyMirror(r.Context(), tx, o.source, o.kind, o.name); err != nil {
				httpx.Fail500(w, s.log, "db_error", err)
				return
			}
		}
	}

	if err := snapshotRevision(r.Context(), tx, kind, "cronomicon", name, restoredUID, id.Email, revActionRestored,
		map[string]any{"restoredFrom": "recycle-bin"}); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	cat := revKindCategory[kind]
	agency := s.definitionAgency(r.Context(), kind, restoredUID) // the restored definition's (C1)
	_ = workflow.InsertChangeLog(r.Context(), s.db, agency, id.Email, cat, "Restored", name, "Restored from the recycle bin")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: agency, Kind: "config", Outcome: "success", Actor: id.Email, Category: cat,
		Action: "Restored", Target: name, Summary: "Restored " + name + " from the recycle bin",
	})
	// The definition is live again and the scheduler's fingerprint does not
	// notice an UPDATE that changes no counted column — force the reload.
	s.forceScheduleReload(r.Context())
	httpx.JSON(w, http.StatusOK, map[string]any{"kind": kind, "name": name, "restored": true})
}

// purgeFromRecycleBin performs the hard delete the soft delete deferred.
//
// This is where the AFTER DELETE triggers finally fire and take the bindings
// with them, and it is where the entity code retires so a later definition of
// the same name mints a fresh log folder (LU-6/LU-Q6(b)) — deferred to here
// rather than done at soft-delete time precisely because a restorable
// definition has not gone anywhere yet.
func (s *Server) purgeFromRecycleBin(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.IdentityFrom(r.Context())
	kind := r.PathValue("kind")
	name := r.PathValue("name")
	if _, ok := revKindTable[kind]; !ok {
		httpx.Fail(w, http.StatusBadRequest, "validation", "kind must be job, workflow or schedule")
		return
	}
	// The caller's binned definition of the name (SO-2, 2.5.0 C3), purged by its
	// uid: another agency's binned row of the name is neither purged nor counted.
	mine, _, derr := s.composableByName(r, kind, name)
	if derr != nil {
		httpx.Fail500(w, s.log, "db_error", derr)
		return
	}
	var binned []composedDef
	for _, d := range mine {
		if d.binned {
			binned = append(binned, d)
		}
	}
	if len(binned) == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "nothing by that name is in the recycle bin")
		return
	}
	if len(binned) > 1 {
		httpx.Fail(w, http.StatusConflict, "conflict", "more than one binned definition holds this name")
		return
	}
	agency := binned[0].owner
	purged, err := purgeBinned(r.Context(), s.db, s.logDirValue(r.Context()), kind, name, binned[0].uid)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	if !purged {
		httpx.Fail(w, http.StatusNotFound, "not_found", "nothing by that name is in the recycle bin")
		return
	}
	cat := revKindCategory[kind]
	_ = workflow.InsertChangeLog(r.Context(), s.db, agency, id.Email, cat, "Purged", name, "Purged from the recycle bin")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		AgencyID: agency, Kind: "config", Outcome: "success", Actor: id.Email, Category: cat,
		Action: "Purged", Target: name, Summary: "Permanently deleted " + name,
	})
	s.forceScheduleReload(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// purgeDefinition hard-deletes one binned definition. Shared by the manual
// purge route and the retention reaper so the two cannot diverge on what
// "permanently deleted" means.
//
// Returns false when nothing by that name is binned, which both callers treat
// as "nothing to do" rather than an error.
func (s *Server) purgeDefinition(ctx context.Context, kind, name string) (bool, error) {
	return PurgeDefinition(ctx, s.db, s.logDirValue(ctx), kind, name)
}

// PurgeDefinition is the package-level form, exported so the retention reaper can
// call it without a *Server.
//
// The reaper starts before the API server is constructed, so a method value could
// not be handed to it at wiring time; taking the pool and the log directory as
// arguments removes the ordering constraint entirely. internal/db cannot import
// internal/api (that is the cycle), so main injects this.
func PurgeDefinition(ctx context.Context, database *sql.DB, logDir, kind, name string) (bool, error) {
	return purgeBinned(ctx, database, logDir, kind, name, "")
}

// purgeBinned is PurgeDefinition narrowed, when uid is set, to that binned
// definition of the name (the route's, SO-2): another agency's binned row of
// the name is not touched, and does not make the purge ambiguous.
func purgeBinned(ctx context.Context, database *sql.DB, logDir, kind, name, uid string) (bool, error) {
	table, ok := revKindTable[kind]
	if !ok {
		return false, fmt.Errorf("unknown kind %q", kind)
	}
	ecKind := entitycode.KindJob
	if kind == revKindWorkflow {
		ecKind = entitycode.KindWorkflow
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// R2-2 — read the uid BEFORE the row goes, because the belt-and-braces
	// cleanup below runs after it and could not resolve the identity from a row
	// that no longer exists. Empty when the definition predates the uid backfill;
	// the cleanup then falls back to the name, as the triggers do.
	// R2-5 — multiplicity refuses. Two same-named binned siblings and a
	// name-addressed purge is a coin flip over which department's history dies;
	// the caller must purge one at a time once the other is restored or renamed.
	which, whichArgs := ` WHERE source='cronomicon' AND name = ? AND deleted_at IS NOT NULL`, []any{name}
	if uid != "" {
		which, whichArgs = which+` AND uid = ?`, append(whichArgs, uid)
	}
	var binned int
	var purgedUID sql.NullString
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*), MAX(uid) FROM `+table+which, whichArgs...).Scan(&binned, &purgedUID)
	if binned > 1 {
		return false, fmt.Errorf("more than one binned definition holds this name")
	}

	// The real DELETE — which is what finally fires trg_def_schedules_*_delete
	// and trg_paused_jobs_*_delete. The soft delete could not, being an UPDATE.
	res, err := tx.ExecContext(ctx, `DELETE FROM `+table+which, whichArgs...)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	// Belt-and-braces, matching the compose deletes: explicit rather than relying
	// on triggers a future table rebuild could silently drop (the 170/200 note).
	// Keyed on the uid when there is one so this mirror stays as narrow as the
	// trigger it duplicates — a name-only delete would reach into a sibling
	// agency's rows the moment names may repeat.
	if purgedUID.Valid && purgedUID.String != "" {
		_, _ = tx.ExecContext(ctx,
			`DELETE FROM paused_jobs WHERE owner_kind=? AND owner_uid=?`, kind, purgedUID.String)
	} else {
		_, _ = tx.ExecContext(ctx,
			`DELETE FROM paused_jobs WHERE source='cronomicon' AND owner_kind=? AND name=?`, kind, name)
	}
	if kind == revKindSchedule {
		// Schedules have no delete trigger; their runtime expansions are keyed by
		// schedule_uid, exactly as deleteScheduleDef cascades them (1300). The
		// arm that also took every entry with NO schedule uid and this NAME is
		// gone: such an entry is one that could not be attributed to a single
		// schedule, and a purge of one schedule must not delete the timing of a
		// definition bound to another schedule of the same name.
		if purgedUID.Valid && purgedUID.String != "" {
			_, _ = tx.ExecContext(ctx,
				`DELETE FROM definition_schedules WHERE schedule_uid=?`, purgedUID.String)
		}
	}

	var code string
	if kind != revKindSchedule && purgedUID.Valid {
		code, _ = entitycode.Lookup(ctx, tx, ecKind, purgedUID.String)
		if err := entitycode.MarkDeleted(ctx, tx, ecKind, purgedUID.String); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// LU-7: annotate the log folder rather than destroying it — the no-reuse
	// guarantee the stamp above provides is what makes keeping it safe.
	if code != "" {
		runner.RefreshEntityMeta(ctx, database, logDir, code)
	}
	return true, nil
}

// recycleBinRetentionDays reads the operator's purge window. 0 means keep
// forever, matching every other retention knob.
func (s *Server) recycleBinRetentionDays(ctx context.Context) int {
	var raw sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='auditCompliance'`).Scan(&raw)
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return defaultRecycleBinDays
	}
	var parsed struct {
		RetentionDays struct {
			RecycleBin *int `json:"recycleBin"`
		} `json:"retentionDays"`
	}
	if err := json.Unmarshal([]byte(raw.String), &parsed); err != nil {
		return defaultRecycleBinDays
	}
	if parsed.RetentionDays.RecycleBin == nil {
		return defaultRecycleBinDays
	}
	return *parsed.RetentionDays.RecycleBin
}

// defaultRecycleBinDays mirrors settings.defaultAuditCompliance().
const defaultRecycleBinDays = 30
