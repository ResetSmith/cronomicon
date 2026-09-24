package gitlab

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/metrics"
	"github.com/ResetSmith/cronomicon/internal/settings"
	"github.com/ResetSmith/cronomicon/internal/sortparam"
)

// Handlers wraps Service and exposes http.HandlerFunc methods for each route.
type Handlers struct {
	svc *Service
}

// NewHandlers creates handler wrappers around the Service.
func NewHandlers(svc *Service) *Handlers {
	return &Handlers{svc: svc}
}

// ──────────────────────────────────────────────────────────────────────────────
// GET /api/v1/git/sync — sync status
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) GetSyncStatus(w http.ResponseWriter, r *http.Request) {
	state, err := h.svc.GetSyncState(r.Context())
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
	startedAt := time.Now().UTC()
	h.svc.TriggerSync(r.Context(), "manual")
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
	events, total, err := h.svc.ListSyncEvents(r.Context(), page, pageSize, orderBy)
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
func (h *Handlers) WebhookGitLab(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Gitlab-Token")
	if token == "" || !h.svc.ValidateWebhookToken(r.Context(), token) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid X-Gitlab-Token")
		return
	}
	policy := settings.GetWebhookPolicy(r.Context(), h.svc.db)
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
	h.svc.TriggerSync(r.Context(), "webhook")
	metrics.WebhookSync("ok") // C.2
	w.WriteHeader(http.StatusAccepted)
}

// ──────────────────────────────────────────────────────────────────────────────
// POST /api/v1/schedules/publish — schedule write-path (A2)
// ──────────────────────────────────────────────────────────────────────────────

func (h *Handlers) PublishSchedule(actor string, w http.ResponseWriter, r *http.Request) {
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

	result, err := h.svc.Publish(r.Context(), req, baseSHA, actor)
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
			_ = h.svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed",
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
			_ = h.svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed", ve.Message)
			return
		}
		// GitLab unreachable or other push failure (503).
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"code":    "gitlab_unreachable",
			"message": "Failed to push to GitLab: " + err.Error(),
		})
		_ = h.svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, "", "failed", err.Error())
		return
	}

	// Record successful push.
	_ = h.svc.RecordPush(r.Context(), actor, req.FilePath, baseSHA, result.CommitSHA, "success", "")
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
	return filepath.Join("/var/lib/amadeus/git-cache", "job-definitions")
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
	// 1. Get scopes before sync
	beforeScopes, err := settings.ListScopes(r.Context(), h.svc.db, "git")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db_error", "failed to list scopes before sync: "+err.Error())
		return
	}

	// 2. Call h.svc.SyncBlocking(...)
	res := h.svc.SyncBlocking(r.Context(), actor)

	// 3. Get scopes after sync
	afterScopes, err := settings.ListScopes(r.Context(), h.svc.db, "git")
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

	afterMap := make(map[string]settings.Scope)
	for _, sc := range afterScopes {
		afterMap[sc.Scope] = sc
		before, exists := beforeMap[sc.Scope]
		if !exists {
			typesStr := strings.Join(sc.Capability.Types, ", ")
			deltas = append(deltas, delta{
				Scope:  sc.Scope,
				Change: fmt.Sprintf("added capability: [%s]", typesStr),
			})
		} else {
			if !equalStrings(before.Capability.Types, sc.Capability.Types) {
				oldTypes := strings.Join(before.Capability.Types, ", ")
				newTypes := strings.Join(sc.Capability.Types, ", ")
				deltas = append(deltas, delta{
					Scope:  sc.Scope,
					Change: fmt.Sprintf("changed capability: [%s] -> [%s]", oldTypes, newTypes),
				})
			}
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

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ma := make(map[string]bool)
	for _, x := range a {
		ma[x] = true
	}
	for _, x := range b {
		if !ma[x] {
			return false
		}
	}
	return true
}

// contextKey is a package-local context key type.
type contextKey int

const _ contextKey = iota

// StartBackgroundSync kicks off the first sync in the background.
// Call this once at startup after the service is initialized.
func StartBackgroundSync(ctx context.Context, svc *Service) {
	svc.TriggerSync(ctx, "poll")
}
