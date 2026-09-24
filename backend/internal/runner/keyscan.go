package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Host-key scan & approve (runner-install-update-2.md Phase 5, D4: 4A).
//
// ── Operator: request a scan ──────────────────────────────────────────────────

type keyscanRequest struct {
	Hosts []string `json:"hosts"`
}

// HandleKeyscan queues a host-key scan for a runner (D4: 4A stage 1). Operator,
// CSRF required. The next poll delivers PollControl{Op:"keyscan", Hosts:…}; the
// agent scans each host from its own vantage and uploads what it saw.
// POST /api/v1/runners/{id}/keyscan
func (s *Service) HandleKeyscan(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	var req keyscanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	hosts := normalizeHosts(req.Hosts)
	if len(hosts) == 0 {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "at least one host is required")
		return
	}

	var name string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT name FROM runners WHERE id = ?`, runnerID).
		Scan(&name); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// Merge with any already-pending hosts (union) so a second request before the
	// next poll doesn't drop the first.
	var existingRaw *string
	_ = s.db.QueryRowContext(r.Context(),
		`SELECT keyscan_requested FROM runners WHERE id = ?`, runnerID).Scan(&existingRaw)
	merged := mergeHosts(parseHostList(existingRaw), hosts)
	mergedJSON, _ := json.Marshal(merged)
	if _, err := s.db.ExecContext(r.Context(),
		`UPDATE runners SET keyscan_requested = ? WHERE id = ?`, string(mergedJSON), runnerID); err != nil {
		s.log.Error("queue keyscan", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	ts := now()
	_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      sessionActor(r),
		Target:     "runner:" + name,
		RunnerName: name,
		Summary:    "host-key scan requested (" + strings.Join(hosts, ", ") + ")",
	})

	httpx.JSON(w, http.StatusAccepted, map[string]any{"hosts": merged})
}

// takeKeyscan consumes a pending scan request, returning the hosts to scan
// (deliver-once, like takeResync). Reads the value, then clears it with a
// consuming UPDATE — the poll whose UPDATE affects the row delivers; a
// concurrent poll gets 0 rows and returns nil. (RETURNING can't help here: it
// yields the post-update NULL, not the old host list.)
func (s *Service) takeKeyscan(ctx context.Context, runnerID string) []string {
	var raw *string
	if err := s.db.QueryRowContext(ctx,
		`SELECT keyscan_requested FROM runners WHERE id = ?`, runnerID).Scan(&raw); err != nil {
		return nil
	}
	hosts := parseHostList(raw)
	if len(hosts) == 0 {
		return nil
	}
	// Compare-and-swap on the exact value read: if an operator merged more hosts
	// in between (HandleKeyscan), the value changed → 0 rows → we deliver nothing
	// this poll and the fuller list rides the next one (no host dropped). Also
	// covers a concurrent poll consuming it first.
	res, err := s.db.ExecContext(ctx, `
		UPDATE runners SET keyscan_requested = NULL
		WHERE id = ? AND keyscan_requested = ?`, runnerID, *raw)
	if err != nil {
		return nil
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	return hosts
}

// ── Runner: upload scanned keys ───────────────────────────────────────────────

type uploadHostKeysRequest struct {
	Entries []scannedHostKey `json:"entries"`
}

type scannedHostKey struct {
	Host           string `json:"host"`
	KeyType        string `json:"keyType"`
	Fingerprint    string `json:"fingerprint"`
	KnownHostsLine string `json:"knownHostsLine"`
}

// HandleUploadHostKeys ingests the keys a runner scanned (runner-key auth) into
// pending_host_keys for operator approval. A re-scan of the same (host, keyType)
// REPLACEs the pending row (resetting its approval state).
// POST /api/v1/runners/{id}/hostkeys
func (s *Service) HandleUploadHostKeys(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	var req uploadHostKeysRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}

	ts := now()
	inserted := 0
	for _, e := range req.Entries {
		if strings.TrimSpace(e.Host) == "" || strings.TrimSpace(e.KnownHostsLine) == "" ||
			strings.TrimSpace(e.KeyType) == "" || strings.TrimSpace(e.Fingerprint) == "" {
			continue // skip malformed entries rather than fail the whole upload
		}
		// Reject any field carrying a newline/control char: the known_hosts line
		// is appended VERBATIM by the agent on approval, so an embedded newline
		// would smuggle a second trusted host past the operator's single-line
		// approval. The operator only ever eyeballs one host + fingerprint.
		if hasCtrl(e.KnownHostsLine) || hasCtrl(e.Host) || hasCtrl(e.KeyType) || hasCtrl(e.Fingerprint) {
			s.log.Warn("rejecting host-key entry with control chars", "runner_id", runnerID, "host", e.Host)
			continue
		}
		// INSERT OR REPLACE on the (runner_id, host, key_type) unique index: a
		// re-scan supersedes the prior pending row and its approval state.
		if _, err := s.db.ExecContext(r.Context(), `
			INSERT OR REPLACE INTO pending_host_keys
			  (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			db.NewID(), runnerID, e.Host, e.KeyType, e.Fingerprint, e.KnownHostsLine, ts); err != nil {
			s.log.Error("insert pending host key", "runner_id", runnerID, "error", err)
			continue
		}
		inserted++
	}

	httpx.JSON(w, http.StatusOK, map[string]any{"accepted": inserted})
}

// ── Operator: list pending & resolve ──────────────────────────────────────────

type pendingHostKey struct {
	ID          string `json:"id"`
	RunnerID    string `json:"runnerId"`
	RunnerName  string `json:"runnerName"`
	Host        string `json:"host"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	ScannedAt   string `json:"scannedAt"`
}

// HandleListPendingHostKeys returns the unresolved (neither approved nor
// rejected) scanned keys awaiting approval, newest first, with runner names.
// GET /api/v1/runners/host-keys/pending
func (s *Service) HandleListPendingHostKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT p.id, p.runner_id, COALESCE(rn.name, p.runner_id), p.host, p.key_type,
		       p.fingerprint, p.scanned_at
		FROM pending_host_keys p
		LEFT JOIN runners rn ON rn.id = p.runner_id
		WHERE p.approved_at IS NULL AND p.rejected_at IS NULL
		ORDER BY p.scanned_at DESC`)
	if err != nil {
		s.log.Error("list pending host keys", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer rows.Close()

	out := []pendingHostKey{}
	for rows.Next() {
		var p pendingHostKey
		if err := rows.Scan(&p.ID, &p.RunnerID, &p.RunnerName, &p.Host, &p.KeyType,
			&p.Fingerprint, &p.ScannedAt); err != nil {
			continue
		}
		out = append(out, p)
	}
	httpx.JSON(w, http.StatusOK, out)
}

type resolveHostKeyRequest struct {
	Action string `json:"action"` // "approve" | "reject"
}

// HandleResolveHostKey approves or rejects a pending scanned key. Approval marks
// the row (the next poll delivers its known_hosts line as a trust-hosts op);
// rejection marks it rejected and it is never trusted. Operator, CSRF required.
// Approving a fingerprint without out-of-band verification is still TOFU — the
// UI shows the full SHA256 for comparison (docs stress this).
// POST /api/v1/runners/host-keys/{keyId}/resolve
func (s *Service) HandleResolveHostKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyId")

	var req resolveHostKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if req.Action != "approve" && req.Action != "reject" {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "action must be 'approve' or 'reject'")
		return
	}

	var host, keyType, fingerprint, runnerID string
	var approvedAt, rejectedAt *string
	if err := s.db.QueryRowContext(r.Context(), `
		SELECT host, key_type, fingerprint, runner_id, approved_at, rejected_at
		FROM pending_host_keys WHERE id = ?`, keyID).
		Scan(&host, &keyType, &fingerprint, &runnerID, &approvedAt, &rejectedAt); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host key not found")
		return
	}
	if approvedAt != nil || rejectedAt != nil {
		httpx.Fail(w, http.StatusConflict, "conflict", "this host key was already resolved")
		return
	}

	actor := sessionActor(r)
	ts := now()
	var col string
	if req.Action == "approve" {
		col = "approved"
	} else {
		col = "rejected"
	}
	if _, err := s.db.ExecContext(r.Context(), fmt.Sprintf(`
		UPDATE pending_host_keys SET %s_at = ?, %s_by = ? WHERE id = ?`, col, col),
		ts, actor, keyID); err != nil {
		s.log.Error("resolve host key", "id", keyID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}

	_ = settings.WriteChangeLog(r.Context(), s.db, actor, "Host Keys", req.Action+"d",
		host+" ("+keyType+")", fingerprint)

	httpx.JSON(w, http.StatusOK, map[string]any{"id": keyID, "action": req.Action})
}

// takeTrustHosts returns the approved-but-not-yet-delivered known_hosts lines
// for a runner and marks them delivered (deliver-once).
func (s *Service) takeTrustHosts(ctx context.Context, runnerID string) []string {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, known_hosts_line FROM pending_host_keys
		WHERE runner_id = ? AND approved_at IS NOT NULL AND trusted_at IS NULL`, runnerID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ids, lines []string
	for rows.Next() {
		var id, line string
		if err := rows.Scan(&id, &line); err != nil {
			continue
		}
		ids = append(ids, id)
		lines = append(lines, line)
	}
	if len(ids) == 0 {
		return nil
	}
	// Mark delivered so the op isn't re-sent every poll. Delivery is at-most-once
	// (marked before the response is written): if the HTTP write is lost the host
	// simply stays untrusted — fail-closed — and the operator re-scans to retry.
	// A duplicate delivery (two concurrent polls) is harmless: the agent dedupes
	// on append.
	ts := now()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE pending_host_keys SET trusted_at = ? WHERE id = ?`, ts, id); err != nil {
			s.log.Error("mark host key trusted", "id", id, "error", err)
		}
	}
	return lines
}

// appendHostKeyControl appends any pending keyscan / trust-hosts ops for this
// runner to the poll's control slice (deliver-once). Called at the top
// of the poll; an approval landing mid-long-poll rides the next poll (host-key
// trust is not sub-second-critical, unlike a kill).
func (s *Service) appendHostKeyControl(ctx context.Context, runnerID string, control []runnerproto.PollControl) []runnerproto.PollControl {
	if hosts := s.takeKeyscan(ctx, runnerID); len(hosts) > 0 {
		control = append(control, runnerproto.PollControl{Op: "keyscan", Hosts: hosts})
	}
	if lines := s.takeTrustHosts(ctx, runnerID); len(lines) > 0 {
		control = append(control, runnerproto.PollControl{Op: "trust-hosts", Entries: lines})
	}
	return control
}

// ── helpers ───────────────────────────────────────────────────────────────────

// hasCtrl reports whether s contains a newline, carriage return, or other
// control character — used to reject host-key fields that could inject an extra
// known_hosts line on append.
func hasCtrl(s string) bool {
	for _, r := range s {
		if r == '\n' || r == '\r' || r < 0x20 {
			return true
		}
	}
	return false
}

func normalizeHosts(hosts []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

func mergeHosts(a, b []string) []string {
	return normalizeHosts(append(append([]string{}, a...), b...))
}

func parseHostList(raw *string) []string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	var hosts []string
	_ = json.Unmarshal([]byte(*raw), &hosts)
	return hosts
}
