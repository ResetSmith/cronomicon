package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// Host-key scan & approve (runner-install-update-2.md Phase 5, D4: 4A).
//
// ── Operator: request a scan ──────────────────────────────────────────────────

type keyscanRequest struct {
	// Hosts is a typed list: host or host:port. ScopeID asks for every host of
	// a scope instead. Exactly one of the two.
	Hosts   []string `json:"hosts"`
	ScopeID string   `json:"scopeId"`
}

// skippedScanHost is a scope host a scope scan left out, and why.
type skippedScanHost struct {
	Host   string `json:"host"`
	Reason string `json:"reason"`
	// Pattern is the known_hosts host the runner will look this host up under
	// — what a pasted key line for it has to start with.
	Pattern string `json:"pattern"`
}

// validScanTarget reports whether a typed scan target is a plain host or
// host:port. A target becomes a known_hosts host, so anything that would mean
// more than one host there — a comma, a wildcard, a negation, a hash — is
// refused before the runner is asked to dial it.
func validScanTarget(t string) bool {
	if t == "" || len(t) > 255 || hasCtrl(t) || strings.ContainsAny(t, " ,*?!|@#") {
		return false
	}
	// And the known_hosts host it becomes must be one the runner's verifier can
	// load: a target like "[x" would write a line that fails its whole file.
	return knownhostsline.ValidPattern(hostkeys.Pattern(t))
}

// HandleKeyscan queues a host-key scan for a runner (D4: 4A stage 1). Operator,
// CSRF required. The next poll delivers PollControl{Op:"keyscan", Hosts:…}; the
// agent scans each host from its own vantage and uploads what it saw.
//
// A scope scan (SB) expands the scope to dial addresses here, on the server:
// the runner is told addresses, never scope names. It is offered only for a
// scope the runner is eligible for by agency — otherwise the request would be a
// way to read one department's host list with another's runner — and it
// returns the hosts it could not include, each with the reason.
// POST /api/v1/runners/{id}/keyscan
func (s *Service) HandleKeyscan(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")

	var req keyscanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	hosts := normalizeHosts(req.Hosts)
	if (len(hosts) == 0) == (req.ScopeID == "") {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "give either hosts or scopeId")
		return
	}
	// LR-63: a caller who is here for their own agency's scopes on a runner they
	// do not own may scan such a scope and nothing else. A typed host has no
	// scope to answer for it. (A scope's hosts are its agency's to choose, so a
	// guest does decide which addresses are dialled; what they may do with the
	// answer is bounded in resolveBatch, where a guest never replaces a key.)
	if limit, limited := hostKeyScopes(r.Context()); limited && !limit[req.ScopeID] {
		httpx.Fail(w, http.StatusForbidden, "forbidden",
			"this runner is not your agency's: you may scan the hosts of a scope of your own agency with it, "+
				"and nothing else. Scanning typed hosts, or another scope, takes the runner's owner")
		return
	}

	var name string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT name FROM runners WHERE id = ?`, runnerID).
		Scan(&name); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}

	// What each target stands for, so the key that comes back can be shown — and
	// recorded — against the host the operator knows. A typed target stands for
	// itself; it still gets a row, to displace a stale scope mapping.
	type target struct{ target, scopeID, scopeName, hostName string }
	var targets []target
	skipped := []skippedScanHost{}
	summary := "host-key scan requested (" + strings.Join(hosts, ", ") + ")"
	if req.ScopeID != "" {
		var scopeName string
		err := s.db.QueryRowContext(r.Context(), `SELECT name FROM scopes WHERE id = ?`, req.ScopeID).Scan(&scopeName)
		id, ok := auth.IdentityFrom(r.Context())
		if err != nil || !ok || !auth.ScopeReadable(id, scopeName) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
			return
		}
		eligible, err := execspec.RunnerEligibleForScope(r.Context(), s.db, req.ScopeID, runnerID)
		if err != nil {
			s.log.Error("keyscan: scope eligibility", "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if !eligible {
			httpx.Fail(w, http.StatusUnprocessableEntity, "runner_not_eligible",
				"this runner cannot serve that scope (a scope takes a runner that serves its agency; a scope that is Global's takes a runner that serves Global), so it cannot scan it")
			return
		}
		plan, err := hostkeys.PlanScope(r.Context(), s.db, scopeName)
		if err != nil {
			s.log.Error("keyscan: expand scope", "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if len(plan) == 0 {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "that scope has no hosts")
			return
		}
		bastions := map[string]bool{}
		for _, h := range plan {
			// A bastion is dialled directly, so IT can be scanned even though
			// the host behind it cannot — and the runner checks both keys.
			if h.Via != "" && !bastions[h.Via] && validScanTarget(h.Via) {
				bastions[h.Via] = true
				hosts = append(hosts, h.Via)
				targets = append(targets, target{h.Via, req.ScopeID, scopeName, h.Via + " (bastion)"})
			}
			if h.Target == "" {
				skipped = append(skipped, skippedScanHost{Host: h.Host, Reason: h.NotScannable, Pattern: h.Pattern})
				continue
			}
			hosts = append(hosts, h.Target)
			targets = append(targets, target{h.Target, req.ScopeID, scopeName, h.Host})
		}
		hosts = normalizeHosts(hosts)
		summary = fmt.Sprintf("host-key scan requested (scope %s: %d host(s), %d not scannable)", scopeName, len(hosts), len(skipped))
	} else {
		for _, h := range hosts {
			if !validScanTarget(h) {
				httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
					fmt.Sprintf("%q is not a host or host:port", h))
				return
			}
			targets = append(targets, target{target: h})
		}
	}

	ts := now()
	actor := sessionActor(r)
	if len(hosts) > 0 {
		// Rows nobody answered (an unreachable host never uploads a key; a runner
		// may be gone) are dropped once they are a day old — any runner's, since
		// a deleted runner will never come back to sweep its own.
		_, _ = s.db.ExecContext(r.Context(), `
			DELETE FROM host_key_scan_targets
			 WHERE requested_at < strftime('%Y-%m-%dT%H:%M:%SZ', 'now', '-1 day')`)
		for _, t := range targets {
			if _, err := s.db.ExecContext(r.Context(), `
				INSERT OR REPLACE INTO host_key_scan_targets
				    (runner_id, target, scope_id, scope_name, host_name, requested_by, requested_at)
				VALUES (?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
				runnerID, t.target, t.scopeID, t.scopeName, t.hostName, actor, ts); err != nil {
				s.log.Error("record scan target", "runner_id", runnerID, "error", err)
				httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
				return
			}
		}

		// Merge with any already-pending hosts (union) so a second request before
		// the next poll doesn't drop the first.
		var existingRaw *string
		_ = s.db.QueryRowContext(r.Context(),
			`SELECT keyscan_requested FROM runners WHERE id = ?`, runnerID).Scan(&existingRaw)
		mergedJSON, _ := json.Marshal(mergeHosts(parseHostList(existingRaw), hosts))
		if _, err := s.db.ExecContext(r.Context(),
			`UPDATE runners SET keyscan_requested = ? WHERE id = ?`, string(mergedJSON), runnerID); err != nil {
			s.log.Error("queue keyscan", "runner_id", runnerID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		_ = auditlog.WriteActivity(r.Context(), s.db, auditlog.ActivityParams{
			At:         ts,
			Kind:       "config",
			Actor:      actor,
			Target:     "runner:" + name,
			RunnerName: name,
			Summary:    summary,
		})
	}
	if hosts == nil {
		hosts = []string{}
	}
	// hosts is what THIS request queued. A scope whose every host is unscannable
	// queues nothing and still answers: the skipped list is the answer.
	httpx.JSON(w, http.StatusAccepted, map[string]any{"hosts": hosts, "skipped": skipped})
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
		target := strings.TrimSpace(e.Host)
		if !validScanTarget(target) {
			continue // skip malformed entries rather than fail the whole upload
		}
		// The agent's own key type and fingerprint are not used. The line is
		// parsed here, the fingerprint computed from the key it carries, and the
		// line that will later be delivered is rendered from that key for that
		// one host. So what the operator is shown is the key that will be
		// trusted, and a line cannot carry a second host, a wildcard, a marker
		// or a newline past a one-row approval — which matters beyond this
		// runner, because an approved key can be carried to another one.
		hosts, key, problem := parseSingleKeyLine(strings.TrimSpace(e.KnownHostsLine))
		pattern := hostkeys.Pattern(target)
		if problem == "" && (len(hosts) != 1 || hosts[0] != pattern || knownhostsline.IsHashed(pattern)) {
			problem = "the line is not for the host it was scanned as"
		}
		if problem != "" {
			s.log.Warn("rejecting scanned host key", "runner_id", runnerID, "host", target, "reason", problem)
			continue
		}

		var scopeID, scopeName, hostName sql.NullString
		if err := s.db.QueryRowContext(r.Context(), `
			SELECT scope_id, scope_name, host_name FROM host_key_scan_targets
			 WHERE runner_id = ? AND target = ?`, runnerID, target).
			Scan(&scopeID, &scopeName, &hostName); err != nil && !errors.Is(err, sql.ErrNoRows) {
			s.log.Error("read scan target", "runner_id", runnerID, "error", err)
		}
		// INSERT OR REPLACE on the (runner_id, host, key_type) unique index: a
		// re-scan supersedes the prior pending row. Its earlier resolution, if it
		// had one, is in the ledger.
		if _, err := s.db.ExecContext(r.Context(), `
			INSERT OR REPLACE INTO pending_host_keys
			  (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at,
			   scope_id, scope_name, host_name)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			db.NewID(), runnerID, pattern, key.Type(), ssh.FingerprintSHA256(key),
			renderKnownHostsLine(pattern, key), ts, scopeID, scopeName, hostName); err != nil {
			s.log.Error("insert pending host key", "runner_id", runnerID, "error", err)
			continue
		}
		_, _ = s.db.ExecContext(r.Context(),
			`DELETE FROM host_key_scan_targets WHERE runner_id = ? AND target = ?`, runnerID, target)
		inserted++
	}

	httpx.JSON(w, http.StatusOK, map[string]any{"accepted": inserted})
}

// ── Operator: list pending & resolve ──────────────────────────────────────────

// PendingHostKey is one scanned key awaiting review, as the fleet-wide list
// reports it.
type PendingHostKey struct {
	ID          string `json:"id"`
	RunnerID    string `json:"runnerId"`
	RunnerName  string `json:"runnerName"`
	Host        string `json:"host"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	ScannedAt   string `json:"scannedAt"`
}

// PendingHostKeys returns the unresolved (neither approved nor rejected)
// scanned keys of EVERY runner, newest first. It is not an HTTP handler on
// purpose: the rows name hosts, so the route that serves them (api, GET
// /runners/host-keys/pending) first drops the runners the caller has no
// authority over — a check this package cannot make.
func (s *Service) PendingHostKeys(ctx context.Context) ([]PendingHostKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.runner_id, COALESCE(rn.name, p.runner_id), p.host, p.key_type,
		       p.fingerprint, p.scanned_at
		FROM pending_host_keys p
		LEFT JOIN runners rn ON rn.id = p.runner_id
		WHERE p.approved_at IS NULL AND p.rejected_at IS NULL
		ORDER BY p.scanned_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PendingHostKey{}
	for rows.Next() {
		var p PendingHostKey
		if err := rows.Scan(&p.ID, &p.RunnerID, &p.RunnerName, &p.Host, &p.KeyType,
			&p.Fingerprint, &p.ScannedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
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

	// One key is a batch of one: the same transaction, the same ledger row, the
	// same change-log shape as a reviewed list.
	var runnerID string
	if err := s.db.QueryRowContext(r.Context(),
		`SELECT runner_id FROM pending_host_keys WHERE id = ?`, keyID).Scan(&runnerID); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host key not found")
		return
	}
	var approve, reject []string
	if req.Action == "approve" {
		approve = []string{keyID}
	} else {
		reject = []string{keyID}
	}
	// A single key approved by id was approved knowingly, changed or not.
	_, err := s.resolveBatch(r.Context(), runnerID, approve, reject, approve, sessionActor(r))
	if s.failResolve(w, err) {
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"id": keyID, "action": req.Action})
}

// hostKeyWorkProbe is the long-poll loop's per-iteration read of the runner row,
// widened to say whether any host-key op is waiting for this runner: a scan
// request, a report request, a key to deliver, or a key to take away. It reads
// only; the take* functions do the consuming. The two ledger probes are written
// to match the partial indexes of migration 1200 term for term, so each is an
// index lookup that finds nothing on an idle runner however many keys it trusts.
const hostKeyWorkProbe = `
	SELECT status, resync_requested,
	       keyscan_requested IS NOT NULL
	       OR known_hosts_requested = 1
	       OR EXISTS (SELECT 1 FROM host_key_ledger
	                   WHERE runner_id = runners.id AND decision = 'approved'
	                     AND delivered_at IS NULL AND superseded_at IS NULL)
	       OR EXISTS (SELECT 1 FROM host_key_ledger
	                   WHERE runner_id = runners.id AND decision = 'approved'
	                     AND superseded_at IS NOT NULL AND untrusted_at IS NULL)
	  FROM runners WHERE id = ?`

// appendHostKeyControl appends any pending host-key ops for this runner to the
// poll's control slice (deliver-once). Called at the top of the poll, and again
// from inside the long-poll loop when hostKeyWorkProbe says something arrived.
//
// Order matters and the agent applies ops in slice order: removals before
// additions, so replacing a key never leaves a moment where the new line was
// appended and then swept away with the old; and the report last, so it shows
// the file as those two left it.
func (s *Service) appendHostKeyControl(ctx context.Context, runnerID string, control []runnerproto.PollControl) []runnerproto.PollControl {
	if hosts := s.takeKeyscan(ctx, runnerID); len(hosts) > 0 {
		control = append(control, runnerproto.PollControl{Op: "keyscan", Hosts: hosts})
	}
	changed := false
	if lines := s.takeUntrustHosts(ctx, runnerID); len(lines) > 0 {
		control = append(control, runnerproto.PollControl{Op: "untrust-hosts", Entries: lines})
		changed = true
	}
	if lines := s.takeTrustHosts(ctx, runnerID); len(lines) > 0 {
		control = append(control, runnerproto.PollControl{Op: "trust-hosts", Entries: lines})
		changed = true
	}
	if s.takeKnownHostsRequest(ctx, runnerID) || changed {
		control = append(control, runnerproto.PollControl{Op: "known-hosts-report"})
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
