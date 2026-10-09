package gitlab

import (
	"database/sql"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/repoid"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sortparam"
)

// Handlers exposes http.HandlerFunc methods for each route. It answers either
// from one fixed Service, or from a Registry that it asks at every request,
// because a repository's Service is replaced whenever its connection is
// written (GR-11) and a handler that kept the one it was built with would go on
// syncing with the old URL and token.
type Handlers struct {
	svc *Service
	reg *Registry
}

// NewHandlers creates handler wrappers around one fixed Service.
func NewHandlers(svc *Service) *Handlers {
	return &Handlers{svc: svc}
}

// NewRegistryHandlers creates handler wrappers that ask the registry.
func NewRegistryHandlers(reg *Registry) *Handlers {
	return &Handlers{reg: reg}
}

// service is the Service a request is answered by. Every route mounted here
// existed before a repository was a row, and means Global's (GR-20, GR-27):
// the routes that name a repository are Phase R6's. It answers 503 and returns
// nil when there is none: the server is shutting down, or the repository's row
// could not be read when the server started. (A restart swaps one Service for
// the next in one step, so a saved connection is never such a moment.)
func (h *Handlers) service(w http.ResponseWriter) *Service {
	svc := h.serviceOf(repoid.Global)
	if svc == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable",
			"the repository's sync service is not running; try again in a moment")
	}
	return svc
}

// serviceOf is the running Service of one repository, or nil when it has none.
// It writes nothing. (Handlers built around one fixed Service answer for that
// Service's repository and for no other.)
func (h *Handlers) serviceOf(repoID string) *Service {
	if h.reg != nil {
		return h.reg.Service(repoID)
	}
	if h.svc != nil && h.svc.repo() == repoID {
		return h.svc
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /api/v1/git/sync — sync status
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) GetSyncStatus(w http.ResponseWriter, r *http.Request) {
	svc := h.service(w)
	if svc == nil {
		return
	}
	state, err := svc.GetSyncState(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"lastSHA":      state.LastSHA,
		"lastSyncedAt": state.LastSyncedAt,
		"lastStatus":   state.LastStatus,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /api/v1/git/sync — trigger resync
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) PostSync(w http.ResponseWriter, r *http.Request) {
	svc := h.service(w)
	if svc == nil {
		return
	}
	startedAt := time.Now().UTC()
	svc.TriggerSync(r.Context(), "manual")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"startedAt": startedAt.Format(time.RFC3339),
	})
}

// maxPageSize bounds list responses, mirroring the api package's clamp and the
// OpenAPI pageSize.maximum (PP-H5) — these gitlab-package list handlers parse
// page/pageSize themselves, so they need their own clamp.
const maxPageSize = 200

// pageParams parses page/pageSize from the request, defaulting (1, 50) and
// clamping pageSize to maxPageSize so an over-large value can't drive an
// unbounded LIMIT (PP-H5). page is not capped (a large OFFSET is cheap).
func pageParams(r *http.Request) (page, pageSize int) {
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ = strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /api/v1/git/history — list sync events
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) ListGitHistory(w http.ResponseWriter, r *http.Request) {
	svc := h.service(w)
	if svc == nil {
		return
	}
	page, pageSize := pageParams(r)
	// TS-20: allowlisted ?sort=&order=. `id` is the chronological tiebreak (the
	// table's insert order); timestamp maps to started_at, action to the
	// trigger provenance the Action column displays.
	orderBy, sortErr := sortparam.OrderBy(r.URL.Query(), map[string]string{
		"timestamp": "started_at",
		"action":    "triggered_by",
		"status":    "status",
	}, " ORDER BY id DESC", "id DESC")
	if sortErr != nil {
		writeError(w, http.StatusBadRequest, "bad_sort", sortErr.Error())
		return
	}
	events, total, err := svc.ListSyncEvents(r.Context(), page, pageSize, orderBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	totalPages := int(math.Ceil(float64(total) / float64(pageSize)))
	writeJSON(w, http.StatusOK, map[string]any{
		"page":       page,
		"pageSize":   pageSize,
		"totalItems": total,
		"totalPages": totalPages,
		"items":      events,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /api/v1/webhooks/gitlab — GitLab webhook
// ──────────────────────────────────────────────────────────────────────────────

// F2-3 — the Webhook Enabled / Webhook Events settings are read here, and until
// this they were not read anywhere. They were parsed, persisted and surfaced in
// Settings → Integrations, and the handler synced regardless; the feature and
// the configuration were both real, only the join was missing.
//
// Order matters: the token is validated FIRST, so an unauthenticated caller
// learns nothing about how this install is configured — a 403 and a 401 must not
// be distinguishable to someone without the secret.
//
// A disabled webhook REFUSES (403) rather than silently accepting. GitLab shows
// a failed delivery with the reason in its own hook log, which is where an
// operator debugging "why did my push not sync" actually looks. An event type
// that is merely unselected ACCEPTS (202) and does nothing: the delivery is not
// an error, it is simply not one we react to.
//
// Which repository a delivery is for is in the path (GR-20):
// /webhooks/gitlab/{repoId}. The route with no id is the one that existed
// before a repository had one, and means Global's: the hooks installations
// already have point at it. A delivery is checked against THAT repository's
// secret set and policy, and starts that repository's sync and no other.
//
// A repository that does not exist answers exactly as a wrong token does. The
// route is unauthenticated, and "no such repository" would tell anyone who can
// reach it which ids are real.
func (h *Handlers) WebhookGitLab(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repoId")
	if repoID == "" {
		repoID = repoid.Global
	}
	svc := h.serviceOf(repoID)
	if svc == nil && repoID == repoid.Global {
		// Global's always exists; with no Service it is the server that is not
		// ready (see service), and a hook's sender should try again.
		h.service(w)
		return
	}
	token := r.Header.Get("X-Gitlab-Token")
	if svc == nil && token != "" && h.reg != nil && h.reg.db != nil {
		// The read a real repository's check makes, made for one that is not
		// there: without it "no such repository" answers sooner than "wrong
		// token", and the clock says what the status does not.
		var a, b, c sql.NullString
		_ = h.reg.db.QueryRowContext(r.Context(), `
			SELECT webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until
			FROM git_repos WHERE id=?`, repoID).Scan(&a, &b, &c)
	}
	if svc == nil || token == "" || !svc.ValidateWebhookToken(r.Context(), token) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid X-Gitlab-Token")
		return
	}
	policy := settings.GetWebhookPolicy(r.Context(), svc.db, svc.repo())
	if !policy.Enabled {
		metrics.WebhookSync("disabled")
		writeError(w, http.StatusForbidden, "webhook_disabled",
			"GitLab webhook delivery is turned off in Cronomicon (Settings → Integrations → Webhook Enabled)")
		return
	}
	if event := r.Header.Get("X-Gitlab-Event"); !policy.Accepts(event) {
		metrics.WebhookSync("ignored")
		w.WriteHeader(http.StatusAccepted)
		return
	}
	svc.TriggerSync(r.Context(), "webhook")
	metrics.WebhookSync("ok") // C.2
	w.WriteHeader(http.StatusAccepted)
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /api/v1/schedules/publish — schedule write-path (A2)
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) PublishSchedule(actor string, authorize PublishAuthorizer, w http.ResponseWriter, r *http.Request) {
	svc := h.service(w)
	if svc == nil {
		return
	}
	baseSHA := r.Header.Get("If-Match")
	if baseSHA == "" {
		writeError(w, http.StatusBadRequest, "missing_if_match", "If-Match header with base_sha is required")
		return
	}

	var req PublishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "request body must be valid JSON: "+err.Error())
		return
	}
	if req.FilePath == "" || req.Content == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "filePath and content are required")
		return
	}
	// PP-B3: reject path traversal / out-of-tree writes before touching the FS.
	if err := validatePublishPath(req.FilePath); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"code":    "validation_failed",
			"message": err.Error(),
			"errors":  []map[string]string{{"field": "filePath", "message": err.Error()}},
		})
		return
	}
	// GC-9: the route's permission says the caller may publish SOMEWHERE; this
	// asks whether they may publish THIS file. Decided before Publish takes the
	// sync mutex, and before anything is written.
	if authorize != nil {
		if msg := authorize(r, svc.publishTarget(req)); msg != "" {
			writeError(w, http.StatusForbidden, "forbidden", msg)
			return
		}
	}

	result, err := svc.Publish(r.Context(), req, baseSHA, actor)
	if err != nil {
		// Check for precondition failure (412).
		if pe, ok := err.(*PreconditionError); ok {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{
				"code":       "base_sha_mismatch",
				"message":    "File changed since your edit was based on this commit — review the diff and retry.",
				"baseSha":    pe.BaseSHA,
				"currentSha": pe.CurrentSHA,
				"diff":       pe.Diff,
			})
			// Record failed push for audit.
			_ = svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed",
				"base_sha mismatch: current="+pe.CurrentSHA)
			return
		}
		// Validation errors (422).
		if ve, ok := err.(*ValidationFailedError); ok {
			type lineErr struct {
				File    string `json:"file,omitempty"`
				Line    int    `json:"line,omitempty"`
				Field   string `json:"field,omitempty"`
				Message string `json:"message"`
			}
			errs := make([]lineErr, len(ve.Errors))
			for i, e := range ve.Errors {
				errs[i] = lineErr{File: e.File, Line: e.Line, Field: e.Field, Message: e.Message}
			}
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"code":    "validation_failed",
				"message": ve.Message,
				"errors":  errs,
			})
			_ = svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed", ve.Message)
			return
		}
		// GitLab unreachable or other push failure (503).
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code":    "gitlab_unreachable",
			"message": "Failed to push to GitLab: " + err.Error(),
		})
		_ = svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed", err.Error())
		return
	}

	// Record successful push.
	_ = svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, result.CommitSHA, "success", "")
	metrics.SchedulePublish() // C.2

	// Return SchedulePush shape.
	now := time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":            result.CommitSHA, // use commit SHA as the push ID
		"userEmail":     actor,
		"commitSha":     result.CommitSHA,
		"branch":        result.Branch,
		"filesChanged":  []string{req.FilePath},
		"scheduleSlots": req.FilePath,
		"status":        "success",
		"errorMessage":  nil,
		"timestamp":     now,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /api/v1/schedule-pushes — list push audit records
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) ListSchedulePushes(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	page, pageSize := pageParams(r)
	offset := (page - 1) * pageSize

	args := []any{}
	where := "1=1"
	if statusFilter != "" {
		where += " AND status=?"
		args = append(args, statusFilter)
	}
	if from != "" {
		where += " AND at >= ?"
		args = append(args, from)
	}
	if to != "" {
		where += " AND at < ?"
		args = append(args, to)
	}

	// TS-20: allowlisted ?sort=&order=; keys match the wire fields the UI shows.
	orderBy, sortErr := sortparam.OrderBy(r.URL.Query(), map[string]string{
		"timestamp": "at",
		"user":      "actor",
		"file":      "schedule_file",
		"status":    "status",
	}, " ORDER BY id DESC", "id DESC")
	if sortErr != nil {
		writeError(w, http.StatusBadRequest, "bad_sort", sortErr.Error())
		return
	}

	var total int
	countArgs := append([]any{}, args...)
	if err := db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM schedule_pushes WHERE "+where, countArgs...).Scan(&total); err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}

	listArgs := append(args, pageSize, offset)
	rows, err := db.QueryContext(r.Context(),
		"SELECT id, at, actor, schedule_file, base_sha, new_sha, status, details FROM schedule_pushes WHERE "+where+orderBy+" LIMIT ? OFFSET ?",
		listArgs...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", err.Error())
		return
	}
	defer rows.Close()

	type push struct {
		ID            int      `json:"id"`
		UserEmail     string   `json:"userEmail"`
		CommitSha     any      `json:"commitSha"`
		Branch        string   `json:"branch"`
		FilesChanged  []string `json:"filesChanged"`
		ScheduleSlots string   `json:"scheduleSlots"`
		Status        string   `json:"status"`
		ErrorMessage  any      `json:"errorMessage"`
		Timestamp     string   `json:"timestamp"`
	}

	var items []push
	for rows.Next() {
		var (
			id        int
			at        string
			actor     string
			schedFile string
			baseSHA   sql.NullString
			newSHA    sql.NullString
			status    string
			details   sql.NullString
		)
		if err := rows.Scan(&id, &at, &actor, &schedFile, &baseSHA, &newSHA, &status, &details); err != nil {
			writeError(w, http.StatusInternalServerError, "db_error", err.Error())
			return
		}
		p := push{
			ID:            id,
			UserEmail:     actor,
			Branch:        "main",
			FilesChanged:  []string{schedFile},
			ScheduleSlots: schedFile,
			Status:        status,
			Timestamp:     at,
		}
		if newSHA.Valid {
			p.CommitSha = newSHA.String
		}
		if details.Valid && details.String != "" {
			p.ErrorMessage = details.String
		}
		items = append(items, p)
	}
	if items == nil {
		items = []push{}
	}
	totalPages := int(math.Ceil(float64(total) / float64(pageSize)))
	writeJSON(w, http.StatusOK, map[string]any{
		"page":       page,
		"pageSize":   pageSize,
		"totalItems": total,
		"totalPages": totalPages,
		"items":      items,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// Service builder from env/config (called by git_mount.go)
// ──────────────────────────────────────────────────────────────────────────────

// DefaultCloneDir returns the default git clone directory under the data volume.
func DefaultCloneDir() string {
	if d := os.Getenv("CRONOMICON_GIT_CACHE_DIR"); d != "" {
		return d
	}
	return filepath.Join("/var/lib/cronomicon/git-cache", "job-definitions")
}

// ──────────────────────────────────────────────────────────────────────────────
// HTTP helpers
// ──────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{
		"code":    code,
		"message": message,
	})
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /api/v1/scopes/resync — trigger scope resync (blocking)
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) ResyncScopes(actor string, w http.ResponseWriter, r *http.Request) {
	svc := h.service(w)
	if svc == nil {
		return
	}
	// 1. Get scopes before sync
	beforeScopes, err := settings.ListScopes(r.Context(), svc.db, "git")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", "failed to list scopes before sync: "+err.Error())
		return
	}

	// 2. Call svc.SyncBlocking(...)
	// "manual", not the actor: git_sync_events.triggered_by is CHECKed to
	// poll/webhook/manual, and the insert error is discarded, so passing an
	// email here left a scope resync with no history row at all.
	_ = actor
	res := svc.SyncBlocking(r.Context(), "manual")

	// 3. Get scopes after sync
	afterScopes, err := settings.ListScopes(r.Context(), svc.db, "git")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", "failed to list scopes after sync: "+err.Error())
		return
	}

	// 4. Calculate deltas
	beforeMap := make(map[string]settings.Scope)
	for _, sc := range beforeScopes {
		beforeMap[sc.Scope] = sc
	}

	type delta struct {
		Scope  string `json:"scope"`
		Change string `json:"change"`
	}
	var deltas []delta

	// The deltas name scopes that appeared or disappeared in this sync. (Until
	// 2.1.0 they also reported run-type capability changes; those are gone with
	// the capability itself, migration 1170.)
	afterMap := make(map[string]settings.Scope)
	for _, sc := range afterScopes {
		afterMap[sc.Scope] = sc
		if _, exists := beforeMap[sc.Scope]; !exists {
			deltas = append(deltas, delta{Scope: sc.Scope, Change: "added scope"})
		}
	}

	for name := range beforeMap {
		if _, exists := afterMap[name]; !exists {
			deltas = append(deltas, delta{
				Scope:  name,
				Change: "removed scope",
			})
		}
	}

	// 5. Build errors list
	type responseLineError struct {
		File    string `json:"file,omitempty"`
		Line    int    `json:"line,omitempty"`
		Field   string `json:"field,omitempty"`
		Message string `json:"message"`
	}
	var errs []responseLineError
	for _, e := range res.Errors {
		errs = append(errs, responseLineError{
			File:    e.File,
			Line:    e.Line,
			Field:   e.Field,
			Message: e.Message,
		})
	}

	// If the sync completely failed (e.g. clone error), add that system error to the list
	if res.Status == "failed" && res.ErrorMessage != "" {
		errs = append(errs, responseLineError{
			Message: res.ErrorMessage,
		})
	}

	// 6. Return response
	writeJSON(w, http.StatusOK, map[string]any{
		"scopesSynced": res.ScopesSynced,
		"errors":       errs,
		"deltas":       deltas,
	})
}
