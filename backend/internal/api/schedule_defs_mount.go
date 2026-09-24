package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
	"github.com/ResetSmith/cronomicon/internal/workflow"
)

// mountScheduleDefs owns the read-only first-class Schedules catalog (A10a,
// amadeus-v20.md Phase 1).
//
// A first-class Schedule is a standalone named cron (+ optional env) parsed from
// schedules/*.yaml in the GitLab clone and cached read-only. Jobs/workflows
// reference one via scheduleRefs, expanded into the runtime definition_schedules
// cache at sync. Authoring of git schedules goes through the Git publish flow;
// operator (source='amadeus') schedules land via the Phase-3 compose write API.
//
// This is a DISTINCT resource from GET /api/v1/schedules (decision Q-E), which
// remains the per-binding owner projection consumed by the Schedule Inventory tab.
//
// Routes:
//
//	GET /api/v1/schedule-defs
//	GET /api/v1/schedule-defs/{name}   (includes the usedBy reverse index; ?source=git|amadeus, default git)
//	PUT /api/v1/schedule-tags/{name}   (set operator-owned tags; ?source=git|amadeus, default git; RequireCSRF)
func (s *Server) mountScheduleDefs(mux *http.ServeMux) {
	requireSession := s.auth.RequireSession
	mux.Handle("GET /api/v1/schedule-defs",
		requireSession(http.HandlerFunc(s.listScheduleDefs)))
	mux.Handle("GET /api/v1/schedule-defs/{name}",
		requireSession(http.HandlerFunc(s.getScheduleDef)))
	// Operator-owned tags (tags-support.md D4): SQLite-only, sync-preserved, any
	// logged-in user (session + CSRF). Keyed by (source, name) — a git and an
	// amadeus schedule may share a name — so this is the first write route on this
	// otherwise read-only catalog and needs its own CSRF wrapping.
	mux.Handle("PUT /api/v1/schedule-tags/{name}",
		s.auth.RequireSession(s.auth.RequireCSRF(http.HandlerFunc(s.updateScheduleTags))))
}

// scheduleUsedBy is one job/workflow that references a first-class schedule.
type scheduleUsedBy struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // job | workflow
	// R2F-3 — the owner's identity and (for job owners) its derived agencies, so
	// a reverse index listing two same-named owners can say which is which.
	// Additive: `name` and `kind` are unchanged.
	UID      string   `json:"uid,omitempty"`
	Agencies []string `json:"agencies,omitempty"`
}

// scheduleDefRow is the JSON shape of the Schedule schema in openapi.yaml.
type scheduleDefRow struct {
	// UID — the stable surrogate identity (AF-4a); see jobRow.UID.
	UID            string            `json:"uid,omitempty"`
	Name           string            `json:"name"`
	Source         string            `json:"source"`
	Description    *string           `json:"description,omitempty"`
	Cron           string            `json:"cron"`
	Env            map[string]string `json:"env,omitempty"`
	ContentHash    string            `json:"contentHash"`
	SourcePath     *string           `json:"sourcePath,omitempty"`
	SyncedAt       *string           `json:"syncedAt,omitempty"`
	CreatedAt      *string           `json:"createdAt"`
	LastModifiedAt *string           `json:"lastModifiedAt"`
	// Tags are user-authored, SQLite-only labels (migration 290, tags-support.md):
	// WRITABLE via PUT /api/v1/schedule-tags/{name}, NEVER parsed from Git, and
	// preserved across syncs (omitted from upsertSchedules). Same model as Script.tags.
	Tags        []string         `json:"tags"`
	UsedByCount int              `json:"usedByCount"`
	UsedBy      []scheduleUsedBy `json:"usedBy,omitempty"`
	// Activation window (AW-7): optional RFC3339 bounds on when this schedule's
	// cron may fire, absent when unbounded. Copied onto every entry expanded
	// from this schedule, so editing it here propagates to all referencing
	// definitions.
	StartAt *string `json:"startAt,omitempty"`
	EndAt   *string `json:"endAt,omitempty"`
	// Interval is the anchored-interval mode ("7d", "36h"), mutually exclusive
	// with cron; Mode is the derived firing mode (cron | interval | once).
	Interval *string `json:"interval,omitempty"`
	Mode     string  `json:"mode,omitempty"`
	// Working-calendar bindings (CAL-5). READ BACK deliberately: updateScheduleDef
	// treats its body as a full replace, so an editor that cannot read these would
	// silently clear them on any unrelated save — the exact preservedInline hazard
	// the AW review caught in the workflow editor.
	SkipCalendars []string `json:"skipCalendars,omitempty"`
	OnlyCalendars []string `json:"onlyCalendars,omitempty"`
}

func (s *Server) listScheduleDefs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sourceFilter := q.Get("source")
	search := q.Get("q")
	tagFilter := tagutil.ParseQuery(q)
	page, pageSize := pageParams(q)

	where := " WHERE deleted_at IS NULL" // RH
	var args []any
	if sourceFilter == "git" || sourceFilter == "amadeus" {
		where += " AND source = ?"
		args = append(args, sourceFilter)
	}
	if search != "" {
		where += " AND name LIKE ?"
		args = append(args, "%"+search+"%")
	}
	if frag, fargs := tagFilter.SQLFilter("tags"); frag != "" {
		where += " AND " + frag
		args = append(args, fargs...)
	}

	var total int
	_ = s.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM schedules"+where, args...).Scan(&total)

	// usedBy count is a correlated subquery (one round-trip; no nested-iterator
	// pool deadlock). Counted by source_ref (D1c) — the exact reverse index: only
	// entries actually expanded from this first-class schedule, never an inline
	// entry that coincidentally shares the name.
	pageArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize)
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT sc.name, sc.source, sc.description, sc.cron, sc.env, sc.content_hash,
		       sc.source_path, sc.synced_at, sc.created_at, sc.last_modified_at, sc.tags,
		       sc.start_at, sc.end_at, sc.interval, sc.skip_calendars, sc.only_calendars, sc.uid,
		       -- FX-A4: binned owners cannot fire, so they are not "used by" anyone.
		       (SELECT COUNT(*) FROM definition_schedules ds
		         WHERE ds.source_ref = sc.name
		           AND NOT EXISTS (SELECT 1 FROM jobs j2 WHERE ds.owner_kind='job'
		                            AND j2.source=ds.owner_source AND j2.name=ds.owner_name
		                            AND j2.deleted_at IS NOT NULL)
		           AND NOT EXISTS (SELECT 1 FROM workflows w2 WHERE ds.owner_kind='workflow'
		                            AND w2.source=ds.owner_source AND w2.name=ds.owner_name
		                            AND w2.deleted_at IS NOT NULL)) AS used_by
		FROM schedules sc`+where+`
		ORDER BY sc.source, sc.name LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()

	items := []scheduleDefRow{}
	for rows.Next() {
		sd, err := scanScheduleDef(rows)
		if err != nil {
			continue
		}
		items = append(items, sd)
	}

	httpx.JSON(w, http.StatusOK, pageEnvelope(page, pageSize, total, items))
}

func (s *Server) getScheduleDef(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	source := r.URL.Query().Get("source")
	if source != "amadeus" {
		source = "git"
	}
	row := s.db.QueryRowContext(r.Context(), `
		SELECT name, source, description, cron, env, content_hash, source_path, synced_at, created_at, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars, uid, 0
		FROM schedules WHERE source = ? AND name = ?`, source, name)
	sd, err := scanScheduleDef(row)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
		return
	}

	// usedBy reverse index: jobs/workflows whose definition_schedules entries were
	// expanded from this first-class schedule (exact, by source_ref — D1c).
	sd.UsedBy = []scheduleUsedBy{}
	drows, err := s.db.QueryContext(r.Context(),
		`SELECT owner_kind, owner_name, ds.owner_uid,
		        (SELECT GROUP_CONCAT(a.name, '\x1f')
		           FROM jobs j
		           JOIN scopes sc         ON sc.name = j.scope
		           JOIN scope_agencies sa ON sa.scope_id = sc.id
		           JOIN agencies a        ON a.id = sa.agency_id
		          WHERE ds.owner_kind = 'job' AND j.source = ds.owner_source AND j.name = ds.owner_name)
		   FROM definition_schedules ds
		  WHERE source_ref = ?
		    AND NOT EXISTS (SELECT 1 FROM jobs j2 WHERE ds.owner_kind='job'
		                     AND j2.source=ds.owner_source AND j2.name=ds.owner_name
		                     AND j2.deleted_at IS NOT NULL)
		    AND NOT EXISTS (SELECT 1 FROM workflows w2 WHERE ds.owner_kind='workflow'
		                     AND w2.source=ds.owner_source AND w2.name=ds.owner_name
		                     AND w2.deleted_at IS NOT NULL)
		  ORDER BY owner_kind, owner_name`, name)
	if err == nil {
		defer drows.Close()
		for drows.Next() {
			var kind, owner string
			var uid, agencies sql.NullString
			if drows.Scan(&kind, &owner, &uid, &agencies) == nil {
				sd.UsedBy = append(sd.UsedBy, scheduleUsedBy{
					Name: owner, Kind: kind, UID: uid.String, Agencies: splitAgencies(agencies),
				})
			}
		}
	}
	sd.UsedByCount = len(sd.UsedBy)

	httpx.JSON(w, http.StatusOK, sd)
}

// updateScheduleTags sets a first-class schedule's user-authored tags
// (tags-support.md). Tags are SQLite-only (migration 290) and survive Git syncs
// (upsertSchedules omits them). A schedule is keyed by (source, name) since a git
// and an amadeus schedule may share a name; ?source defaults to git. Full replace
// of the tag set; reuses the shared tagutil.Normalize helper + caps.
// Gate is session + CSRF only (D4: any logged-in user) — wired in mountScheduleDefs.
func (s *Server) updateScheduleTags(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.mustActor(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	source := r.URL.Query().Get("source")
	if source != "amadeus" {
		source = "git"
	}

	// Decode/normalize/UPDATE + RowsAffected==0 → 404 is the shared skeleton (CC.12);
	// composite (source,name) key. Audit stays BEFORE the re-fetch below.
	if _, ok := s.writeTagsUpdate(w, r, "schedules", "source = ? AND name = ?", "schedule not found", source, name); !ok {
		return
	}

	_ = workflow.InsertChangeLog(r.Context(), s.db, actor, "Schedules", "Updated", name, "Schedule tags updated")
	_ = workflow.EmitActivity(r.Context(), s.db, workflow.ActivityParams{
		Kind: "config", Actor: actor, Category: "Schedules", Action: "Updated", Target: name,
	})

	// Re-read the updated schedule with its usedBy reverse index — same shape as
	// GET /schedule-defs/{name}.
	row := s.db.QueryRowContext(r.Context(), `
		SELECT name, source, description, cron, env, content_hash, source_path, synced_at, created_at, last_modified_at, tags, start_at, end_at, interval, skip_calendars, only_calendars, uid, 0
		FROM schedules WHERE source = ? AND name = ?`, source, name)
	sd, err := scanScheduleDef(row)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "schedule not found")
		return
	}
	sd.UsedBy = []scheduleUsedBy{}
	drows, derr := s.db.QueryContext(r.Context(),
		`SELECT owner_kind, owner_name FROM definition_schedules ds
		  WHERE source_ref = ?
		    AND NOT EXISTS (SELECT 1 FROM jobs j2 WHERE ds.owner_kind='job'
		                     AND j2.source=ds.owner_source AND j2.name=ds.owner_name
		                     AND j2.deleted_at IS NOT NULL)
		    AND NOT EXISTS (SELECT 1 FROM workflows w2 WHERE ds.owner_kind='workflow'
		                     AND w2.source=ds.owner_source AND w2.name=ds.owner_name
		                     AND w2.deleted_at IS NOT NULL)
		  ORDER BY owner_kind, owner_name`, name)
	if derr == nil {
		defer drows.Close()
		for drows.Next() {
			var kind, owner string
			if drows.Scan(&kind, &owner) == nil {
				sd.UsedBy = append(sd.UsedBy, scheduleUsedBy{Name: owner, Kind: kind})
			}
		}
	}
	sd.UsedByCount = len(sd.UsedBy)

	httpx.JSON(w, http.StatusOK, sd)
}

func scanScheduleDef(sc rowScanner) (scheduleDefRow, error) {
	var (
		sd                               scheduleDefRow
		description, envJSON, sourcePath sql.NullString
		syncedAt                         sql.NullString
		createdAt, lastModifiedAt        sql.NullString
		tags                             sql.NullString
		startAt, endAt, interval         sql.NullString
		skipCals, onlyCals               sql.NullString
		uid                              sql.NullString
	)
	if err := sc.Scan(&sd.Name, &sd.Source, &description, &sd.Cron, &envJSON, &sd.ContentHash,
		&sourcePath, &syncedAt, &createdAt, &lastModifiedAt, &tags, &startAt, &endAt, &interval,
		&skipCals, &onlyCals, &uid, &sd.UsedByCount); err != nil {
		return scheduleDefRow{}, err
	}
	sd.UID = uid.String
	sd.SkipCalendars = calendar.ParseNames(skipCals.String)
	sd.OnlyCalendars = calendar.ParseNames(onlyCals.String)
	sd.StartAt, sd.EndAt = windowStrings(startAt, endAt)
	sd.Interval = nullableOf(interval.String)
	sd.Mode = cronutil.Spec{Cron: sd.Cron, Interval: interval.String,
		Window: cronutil.NewWindow(windowTimes(startAt, endAt))}.Mode()
	sd.Tags = tagutil.Parse(tags.String)
	if description.Valid && strings.TrimSpace(description.String) != "" {
		sd.Description = &description.String
	}
	if envJSON.Valid && strings.TrimSpace(envJSON.String) != "" {
		m := map[string]string{}
		if json.Unmarshal([]byte(envJSON.String), &m) == nil && len(m) > 0 {
			sd.Env = m
		}
	}
	if sourcePath.Valid {
		sd.SourcePath = &sourcePath.String
	}
	if syncedAt.Valid && syncedAt.String != "" {
		sd.SyncedAt = &syncedAt.String
	}
	if createdAt.Valid {
		sd.CreatedAt = &createdAt.String
	}
	if lastModifiedAt.Valid {
		sd.LastModifiedAt = &lastModifiedAt.String
	}
	return sd, nil
}
