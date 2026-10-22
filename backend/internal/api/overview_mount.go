package api

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/calendar"
	"github.com/ResetSmith/cronomicon/internal/cronutil"
	"github.com/ResetSmith/cronomicon/internal/httpx"
)

// The Operations overview and the switcher's badges (2.4.4 S5; plan-2.4.4
// §3, SW-12 to SW-14). The overview is ONE read-only page across every agency
// for those whose grant reaches every agency (the operations team, and global
// administrators): a row per agency, what needs attention across agencies,
// and a search by name. The lists stay one agency at a time (SW-2). Owners are
// read by the same expressions the lists narrow by (agencyid), and runs by
// their own owner (1480), so an agency's counts here are what its own view
// shows.
//
//	GET /api/v1/overview          → the rows and the attention list
//	GET /api/v1/overview/search   → jobs and workflows by name, with their agency
//	GET /api/v1/me/agency-badges  → per agency among the caller's choices: open
//	                                notices and failed runs in the last day

// attentionCap bounds the attention list; the total is said beside it.
const attentionCap = 100

type overviewRepo struct {
	Status   *string `json:"status"`
	SyncedAt *string `json:"syncedAt"`
}

type overviewAgency struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	Jobs            int            `json:"jobs"`
	PausedJobs      int            `json:"pausedJobs"`
	Workflows       int            `json:"workflows"`
	PausedWorkflows int            `json:"pausedWorkflows"`
	Runs24h         map[string]int `json:"runs24h"`
	NextRunAt       *string        `json:"nextRunAt"`
	Repository      *overviewRepo  `json:"repository"`
	OpenNotices     int            `json:"openNotices"`
}

type attentionItem struct {
	Kind       string  `json:"kind"` // job_failed | workflow_failed | run_lost | run_overdue | sync_failed
	AgencyID   string  `json:"agencyId"`
	AgencyName string  `json:"agencyName"`
	Name       string  `json:"name"`
	ID         *string `json:"id,omitempty"`
	TraceID    *string `json:"traceId,omitempty"`
	// Who to call (2.5.2 OP-1): the job's or workflow's annotation, read by
	// annotateAttention. Absent on a sync item and on an unannotated one.
	Contact  string `json:"contact,omitempty"`
	Critical bool   `json:"critical,omitempty"`
	At       string `json:"at"`
}

type overviewResponse struct {
	Agencies       []overviewAgency `json:"agencies"`
	Attention      []attentionItem  `json:"attention"`
	AttentionTotal int              `json:"attentionTotal"`
	GeneratedAt    string           `json:"generatedAt"`
}

func (s *Server) mountOverview(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/overview", s.auth.RequireSession(http.HandlerFunc(s.getOverview)))
	mux.Handle("GET /api/v1/overview/search", s.auth.RequireSession(http.HandlerFunc(s.searchOverview)))
	mux.Handle("GET /api/v1/me/agency-badges", s.auth.RequireSession(http.HandlerFunc(s.getAgencyBadges)))
}

// requireOverview answers 403 to anyone whose grant does not reach every
// agency: a person granted on two agencies included.
func requireOverview(w http.ResponseWriter, r *http.Request) bool {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok || !canSeeOverview(id) {
		httpx.Fail(w, http.StatusForbidden, "forbidden", "the overview is for those whose access reaches every agency")
		return false
	}
	return true
}

// nullStrPtr is a nullable text column as an optional JSON field.
func nullStrPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

// runOutcome is a run's bucket in the overview's 24-hour counts: its status,
// or "lost" for one the reaper closed because its agent went away.
func runOutcome(status, reason string) string {
	if status == "failure" && reason == "runner_lost" {
		return "lost"
	}
	return status
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	if !requireOverview(w, r) {
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour).Format(time.RFC3339)
	fail := func(err error) { httpx.Fail500(w, s.log, "db_error", err) }

	names := map[string]string{}
	var order []string
	byID := map[string]*overviewAgency{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name FROM agencies ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var a overviewAgency
		if err := rows.Scan(&a.ID, &a.Name); err != nil {
			rows.Close()
			fail(err)
			return
		}
		a.Runs24h = map[string]int{}
		names[a.ID] = a.Name
		order = append(order, a.ID)
		byID[a.ID] = &a
	}
	rows.Close()
	at := func(owner string) *overviewAgency {
		if owner == "" {
			owner = agencyid.Global
		}
		return byID[owner] // nil for an owner that is no agency: not counted
	}

	// Jobs and workflows, and how many are paused (disabled, or a pause recorded).
	count := func(q string, add func(a *overviewAgency, n, paused int)) error {
		rows, err := s.db.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var owner string
			var n, paused int
			if err := rows.Scan(&owner, &n, &paused); err != nil {
				return err
			}
			if a := at(owner); a != nil {
				add(a, n, paused)
			}
		}
		return rows.Err()
	}
	if err := count(`SELECT `+agencyid.JobOwnerSQL("j")+`, COUNT(*),
		       SUM(CASE WHEN j.enabled = 0 OR EXISTS (SELECT 1 FROM paused_jobs p WHERE p.owner_kind = 'job'
		             AND (p.owner_uid = j.uid OR (p.owner_uid IS NULL AND p.source = j.source AND p.name = j.name))) THEN 1 ELSE 0 END)
		  FROM jobs j WHERE j.deleted_at IS NULL GROUP BY 1`,
		func(a *overviewAgency, n, p int) { a.Jobs, a.PausedJobs = a.Jobs+n, a.PausedJobs+p }); err != nil {
		fail(err)
		return
	}
	if err := count(`SELECT `+agencyid.WorkflowOwnerSQL+`, COUNT(*),
		       SUM(CASE WHEN w.enabled = 0 OR EXISTS (SELECT 1 FROM paused_jobs p WHERE p.owner_kind = 'workflow'
		             AND (p.owner_uid = w.uid OR (p.owner_uid IS NULL AND p.source = w.source AND p.name = w.name))) THEN 1 ELSE 0 END)
		  FROM workflows w WHERE w.deleted_at IS NULL GROUP BY 1`,
		func(a *overviewAgency, n, p int) { a.Workflows, a.PausedWorkflows = a.Workflows+n, a.PausedWorkflows+p }); err != nil {
		fail(err)
		return
	}

	// Runs in the last 24 hours, by outcome (1480's owner, its index).
	rows, err = s.db.QueryContext(ctx, `SELECT `+runOwnerSQL+`, status, COALESCE(queued_reason, ''), COUNT(*)
		  FROM runs WHERE created_at >= ? AND kind = 'job' GROUP BY 1, 2, 3`, since)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var owner, status, reason string
		var n int
		if err := rows.Scan(&owner, &status, &reason, &n); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if a := at(owner); a != nil {
			a.Runs24h[runOutcome(status, reason)] += n
		}
	}
	rows.Close()

	// Each agency's repository's last sync.
	rows, err = s.db.QueryContext(ctx, `SELECT agency_id, last_status, last_synced_at FROM git_repos`)
	if err != nil {
		fail(err)
		return
	}
	var attention []attentionItem
	for rows.Next() {
		var owner string
		var status, synced sql.NullString
		if err := rows.Scan(&owner, &status, &synced); err != nil {
			rows.Close()
			fail(err)
			return
		}
		a := at(owner)
		if a == nil {
			continue
		}
		a.Repository = &overviewRepo{Status: nullStrPtr(status), SyncedAt: nullStrPtr(synced)}
		if status.String == "failed" || status.String == "partial" {
			attention = append(attention, attentionItem{Kind: "sync_failed", AgencyID: a.ID, AgencyName: a.Name,
				Name: a.Name + "'s repository", At: synced.String})
		}
	}
	rows.Close()

	// Open notices.
	if err := count(`SELECT agency_id, COUNT(*), 0 FROM notices WHERE resolved_at IS NULL AND dismissed_at IS NULL GROUP BY 1`,
		func(a *overviewAgency, n, _ int) { a.OpenNotices += n }); err != nil {
		fail(err)
		return
	}

	// The next scheduled run, as the Upcoming tab projects it.
	next, err := s.nextFireByAgency(ctx, now)
	if err != nil {
		fail(err)
		return
	}
	for owner, t := range next {
		if a := at(owner); a != nil {
			v := t.UTC().Format(time.RFC3339)
			a.NextRunAt = &v
		}
	}

	more, err := s.overviewAttention(ctx, now, since, names)
	if err != nil {
		fail(err)
		return
	}
	attention = append(attention, more...)
	sort.SliceStable(attention, func(i, j int) bool { return attention[i].At > attention[j].At })
	out := overviewResponse{Agencies: []overviewAgency{}, AttentionTotal: len(attention), GeneratedAt: now.Format(time.RFC3339)}
	if len(attention) > attentionCap {
		attention = attention[:attentionCap]
	}
	s.annotateAttention(ctx, attention)
	out.Attention = append([]attentionItem{}, attention...)
	for _, idv := range order {
		out.Agencies = append(out.Agencies, *byID[idv])
	}
	httpx.JSON(w, http.StatusOK, out)
}

// attentionOwnerKind is the annotation owner kind an attention item's ID names:
// a workflow's uid for a failed workflow, a job's uid for the run kinds, and
// nothing for a repository.
func attentionOwnerKind(kind string) string {
	switch kind {
	case "workflow_failed":
		return "workflow"
	case "job_failed", "run_lost", "run_overdue":
		return "job"
	}
	return ""
}

// annotateAttention gives each job and workflow item its definition's contact
// and critical flag (2.5.2 OP-1), in one read of the annotations sidecar for the
// items listed (the cap is applied first). Keyed on (kind, uid) like every read
// of that table. Display-only, as the run detail and the notification are: a
// failed read leaves the items un-annotated and never costs the overview.
func (s *Server) annotateAttention(ctx context.Context, items []attentionItem) {
	type key struct{ kind, uid string }
	var uids []any
	seen := map[string]bool{}
	for _, it := range items {
		if attentionOwnerKind(it.Kind) == "" || it.ID == nil || *it.ID == "" || seen[*it.ID] {
			continue
		}
		seen[*it.ID] = true
		uids = append(uids, *it.ID)
	}
	if len(uids) == 0 {
		return
	}
	rows, err := s.db.QueryContext(ctx, `SELECT owner_kind, owner_uid, critical, contact FROM annotations
		 WHERE owner_uid IN (`+strings.Repeat(",?", len(uids))[1:]+`)`, uids...)
	if err != nil {
		s.log.Warn("annotation lookup failed; attention items render un-annotated", "err", err)
		return
	}
	defer rows.Close()
	type ann struct {
		contact  string
		critical bool
	}
	got := map[key]ann{}
	for rows.Next() {
		var k key
		var crit int
		var contact string
		if err := rows.Scan(&k.kind, &k.uid, &crit, &contact); err != nil {
			s.log.Warn("annotation lookup failed; attention items render un-annotated", "err", err)
			return
		}
		got[k] = ann{contact: contact, critical: crit == 1}
	}
	for i := range items {
		if items[i].ID == nil {
			continue
		}
		if a, ok := got[key{attentionOwnerKind(items[i].Kind), *items[i].ID}]; ok {
			items[i].Contact, items[i].Critical = a.contact, a.critical
		}
	}
}

// overviewAttention is what needs a person, across agencies: jobs and
// workflows whose last run failed, runs lost in the last day, and runs still
// running past their job's timeout. (A repository whose last sync failed is
// added by the caller.)
func (s *Server) overviewAttention(ctx context.Context, now time.Time, since string, names map[string]string) ([]attentionItem, error) {
	var out []attentionItem
	add := func(kind, owner, name string, idv, trace sql.NullString, at string) {
		if owner == "" {
			owner = agencyid.Global
		}
		if _, ok := names[owner]; !ok {
			return
		}
		out = append(out, attentionItem{Kind: kind, AgencyID: owner, AgencyName: names[owner], Name: name,
			ID: nullStrPtr(idv), TraceID: nullStrPtr(trace), At: at})
	}
	scan := func(kind, q string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var owner, name, at string
			var idv, trace sql.NullString
			if err := rows.Scan(&owner, &name, &idv, &trace, &at); err != nil {
				return err
			}
			add(kind, owner, name, idv, trace, at)
		}
		return rows.Err()
	}
	if err := scan("job_failed", `SELECT `+agencyid.JobOwnerSQL("j")+`, j.name, j.uid, r.id, r.created_at
		  FROM jobs j
		  JOIN runs r ON r.id = (SELECT r2.id FROM runs r2 WHERE r2.job_uid = j.uid AND r2.kind = 'job'
		                           AND r2.status NOT IN ('queued', 'running', 'skipped')
		                         ORDER BY r2.created_at DESC LIMIT 1)
		 WHERE j.deleted_at IS NULL AND r.status IN ('failure', 'killed')`); err != nil {
		return nil, err
	}
	if err := scan("workflow_failed", `SELECT `+agencyid.WorkflowOwnerSQL+`, w.name, w.uid, wr.id, wr.created_at
		  FROM workflows w
		  JOIN workflow_runs wr ON wr.id = (SELECT wr2.id FROM workflow_runs wr2 WHERE wr2.workflow_uid = w.uid
		                                      AND wr2.status NOT IN ('queued', 'running', 'skipped')
		                                    ORDER BY wr2.created_at DESC LIMIT 1)
		 WHERE w.deleted_at IS NULL AND wr.status IN ('failure', 'killed')`); err != nil {
		return nil, err
	}
	if err := scan("run_lost", `SELECT `+runOwnerSQL+`, job_name, job_uid, id, created_at
		  FROM runs WHERE created_at >= ? AND kind = 'job' AND status = 'failure' AND queued_reason = 'runner_lost'`, since); err != nil {
		return nil, err
	}
	// Running past the job's timeout: compared here, as the timestamps are RFC
	// 3339 text and the timeout is the job's.
	rows, err := s.db.QueryContext(ctx, `SELECT `+agencyid.Owner("r.owner_agency")+`, r.job_name, r.job_uid, r.id, r.started_at, j.timeout_seconds
		  FROM runs r JOIN jobs j ON j.uid = r.job_uid
		 WHERE r.status = 'running' AND r.started_at IS NOT NULL AND j.timeout_seconds > 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var owner, name, started string
		var idv, trace sql.NullString
		var timeout int
		if err := rows.Scan(&owner, &name, &idv, &trace, &started, &timeout); err != nil {
			return nil, err
		}
		t, perr := time.Parse(time.RFC3339, started)
		if perr == nil && now.Sub(t) > time.Duration(timeout)*time.Second {
			add("run_overdue", owner, name, idv, trace, started)
		}
	}
	return out, rows.Err()
}

// nextFireByAgency is each agency's next scheduled run in the coming week, as
// the Upcoming tab projects it (listUpcomingSchedules): enabled and unpaused
// entries only, a fire a calendar suppresses skipped, none while the
// installation is frozen.
func (s *Server) nextFireByAgency(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if f, err := calendar.ReadFreeze(ctx, s.db); err == nil {
		if _, frozen := calendar.FreezeVerdict(f); frozen {
			return out, nil
		}
	}
	srows, err := s.drainScheduleRows(ctx)
	if err != nil {
		return nil, err
	}
	enabledSet, pausedSet := s.scheduleStates(ctx, srows)
	appLoc := s.appLocation()
	end := now.Add(7 * 24 * time.Hour)
	var allCalNames []string
	for _, sr := range srows {
		allCalNames = append(allCalNames, calendar.ParseNames(sr.skipCals.String)...)
		allCalNames = append(allCalNames, calendar.ParseNames(sr.onlyCals.String)...)
	}
	sets := map[string]calendar.Set{}
	for _, sr := range srows {
		key := defKey{sr.source, sr.kind, sr.owner}
		if !enabledSet[key] || pausedSet[key] {
			continue
		}
		owner := sr.ownerAgency
		if owner == "" {
			owner = agencyid.Global
		}
		set, ok := sets[owner]
		if !ok {
			if set, err = calendar.Load(ctx, s.db, owner, allCalNames); err != nil {
				set = calendar.Set{}
			}
			sets[owner] = set
		}
		skip, only := calendar.ParseNames(sr.skipCals.String), calendar.ParseNames(sr.onlyCals.String)
		for _, t := range cronutil.NextNSpec(sr.spec(), now, end, 50, appLoc) {
			if calendar.EvaluateFor(set, owner, skip, only, calendar.DayOf(t, appLoc)).Suppressed {
				continue
			}
			if cur, ok := out[owner]; !ok || t.Before(cur) {
				out[owner] = t
			}
			break
		}
	}
	return out, nil
}

type overviewHit struct {
	Kind       string `json:"kind"` // job | workflow
	ID         string `json:"id"`
	Name       string `json:"name"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
}

func (s *Server) searchOverview(w http.ResponseWriter, r *http.Request) {
	if !requireOverview(w, r) {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	hits := []overviewHit{}
	if q == "" {
		httpx.JSON(w, http.StatusOK, hits)
		return
	}
	like := "%" + q + "%"
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT kind, uid, name, owner, COALESCE((SELECT a.name FROM agencies a WHERE a.id = owner), owner) FROM (
		  SELECT 'job' AS kind, j.uid AS uid, j.name AS name, `+agencyid.JobOwnerSQL("j")+` AS owner
		    FROM jobs j WHERE j.deleted_at IS NULL AND j.name LIKE ?
		  UNION ALL
		  SELECT 'workflow', w.uid, w.name, `+agencyid.WorkflowOwnerSQL+`
		    FROM workflows w WHERE w.deleted_at IS NULL AND w.name LIKE ?)
		 ORDER BY name COLLATE NOCASE, kind LIMIT 50`, like, like)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var h overviewHit
		var uid sql.NullString
		if err := rows.Scan(&h.Kind, &uid, &h.Name, &h.AgencyID, &h.AgencyName); err != nil {
			httpx.Fail500(w, s.log, "db_error", err)
			return
		}
		h.ID = uid.String
		hits = append(hits, h)
	}
	httpx.JSON(w, http.StatusOK, hits)
}

type agencyBadge struct {
	AgencyID      string `json:"agencyId"`
	OpenNotices   int    `json:"openNotices"`
	FailedRuns24h int    `json:"failedRuns24h"`
}

// getAgencyBadges is, per agency among the caller's choices (the overview
// excepted), what they would want to know of an agency they are not viewing
// (SW-14): its open notices they may act on, as the bell counts them, and its
// failed runs in the last day.
func (s *Server) getAgencyBadges(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.IdentityFrom(r.Context())
	if !ok {
		httpx.Fail(w, http.StatusUnauthorized, "unauthorized", "login required")
		return
	}
	ctx := r.Context()
	choices, err := s.agencyChoices(ctx, id)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	notices, failed := map[string]int{}, map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT agency_id, COUNT(*) FROM notices
		 WHERE resolved_at IS NULL AND dismissed_at IS NULL GROUP BY 1`)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err == nil {
			notices[a] = n
		}
	}
	rows.Close()
	since := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	rows, err = s.db.QueryContext(ctx, `SELECT `+runOwnerSQL+`, COUNT(*) FROM runs
		 WHERE created_at >= ? AND kind = 'job' AND status IN ('failure', 'killed') GROUP BY 1`, since)
	if err != nil {
		httpx.Fail500(w, s.log, "db_error", err)
		return
	}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err == nil {
			failed[a] = n
		}
	}
	rows.Close()
	out := []agencyBadge{}
	for _, c := range choices {
		if c.Kind != "agency" {
			continue
		}
		b := agencyBadge{AgencyID: c.ID, FailedRuns24h: failed[c.ID]}
		if id.CanAgency(auth.PermConfigureApp, c.ID) {
			b.OpenNotices = notices[c.ID]
		}
		out = append(out, b)
	}
	httpx.JSON(w, http.StatusOK, out)
}
