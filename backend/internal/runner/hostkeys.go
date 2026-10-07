package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/ResetSmith/cronomicon/internal/auth"
	"github.com/ResetSmith/cronomicon/internal/db"
	"github.com/ResetSmith/cronomicon/internal/execspec"
	"github.com/ResetSmith/cronomicon/internal/hostkeys"
	"github.com/ResetSmith/cronomicon/internal/httpx"
	"github.com/ResetSmith/cronomicon/internal/knownhostsline"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// Host-key trust — the ledger, and everything that writes to it (SB band;
// migrations 1200/1210, runner protocol 14).
//
// A runner refuses any host not in its known_hosts file. Three things can put a
// key there, and all three end in the same place — APPROVED rows in
// host_key_ledger, delivered to the runner on its next poll:
//
//   - scan     the runner scans hosts from its own network position (a typed
//              list, or a whole scope) and an operator approves what it saw;
//   - pasted   an operator supplies known_hosts lines they already have;
//   - carried  an operator copies, on review, the keys another runner trusts —
//              the cheap half of replacing a runner.
//
// The ledger is the record. It is append-only for decisions: a re-scan, a
// changed key, a removal add rows and never rewrite one. What it cannot see is
// the file itself — a known_hosts seeded on the host by hand, or a delivery
// that never landed — which is what the runner's own report (runner_known_hosts)
// is for. The two are shown side by side and never merged.
//
// Every key the ledger holds is keyed on ONE host in known_hosts form (the
// first field of the line: `host`, or `[host]:port`), and every line it
// delivers was rendered here from a parsed key. Nothing an agent or an operator
// typed reaches a runner's file verbatim.

// Key statuses, as the review screen shows them.
const (
	keyStatusNew     = "new"     // nothing is known about this host's key
	keyStatusMatch   = "match"   // equals the key the SERVER pins for this host
	keyStatusChanged = "changed" // differs from a key already trusted for this host
	keyStatusTrusted = "trusted" // this runner already trusts exactly this key
)

// maxProvidedLines bounds one pasted batch; maxKnownHostsEntries bounds one
// runner report. Both exist to catch a mistake (a whole fleet's file pasted, a
// runaway file), not to express a policy.
const (
	maxProvidedLines     = 500
	maxKnownHostsEntries = 5000
)

// ErrHostKeyNotPending is returned when a batch names a key that is not an
// unresolved pending key OF THE RUNNER the batch is for. Mapped 409: the
// caller's list is stale, or the id belongs to another runner.
var ErrHostKeyNotPending = errors.New("host key is not pending for this runner")

// ErrHostKeyUnverifiable is returned when a pending key's stored line does not
// carry the key its row claims, for the host its row claims. Rows written by
// this version cannot be in that state (the upload renders them); a row scanned
// before the upgrade can, because the agent's word was taken then. Approving it
// is refused — reject it and scan again.
var ErrHostKeyUnverifiable = errors.New("stored host key does not match its fingerprint")

// ErrReviewStale is returned when a commit no longer matches what the operator
// reviewed: a key they ticked is not among the candidates any more (its
// fingerprint changed at the source), or a key they accepted as new would now
// REPLACE one this runner trusts. Either way the list they read is not the list
// that would be written, so nothing is. Mapped 409 `review_stale`.
var ErrReviewStale = errors.New("the keys changed after they were reviewed")

// failResolve maps a batch's errors. It reports whether it wrote a response.
func (s *Service) failResolve(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrReviewStale):
		httpx.Fail(w, http.StatusConflict, "review_stale",
			err.Error()+"; nothing was trusted. Reload the list and review it again")
	case errors.Is(err, ErrHostKeyNotPending):
		httpx.Fail(w, http.StatusConflict, "conflict",
			"one or more of these keys is no longer pending for this runner; reload the list")
	case errors.Is(err, ErrHostKeyUnverifiable):
		httpx.Fail(w, http.StatusUnprocessableEntity, "host_key_unverifiable",
			err.Error()+"; reject it and scan the host again")
	default:
		s.log.Error("resolve host keys", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
	}
	return true
}

// renderKnownHostsLine is the one place a known_hosts line is built.
func renderKnownHostsLine(pattern string, key ssh.PublicKey) string {
	return knownhostsline.Render(pattern, key)
}

// parseSingleKeyLine parses a known_hosts line that must carry one plain key:
// no marker. It reads what the runner's verifier reads (knownhostsline), so a
// trailing comment of any length is ignored rather than refused.
func parseSingleKeyLine(line string) (hosts []string, key ssh.PublicKey, problem string) {
	if hasCtrl(line) {
		return nil, nil, "contains a control character"
	}
	l, err := knownhostsline.Parse(line)
	switch {
	case err != nil:
		return nil, nil, "not a known_hosts line (" + err.Error() + ")"
	case l.Marker != "":
		return nil, nil, "@" + l.Marker + " lines are not accepted here; give the host's own key line"
	}
	return l.Hosts, l.Key, ""
}

// patternPort splits a known_hosts host into the bare host and its port.
func patternPort(pattern string) (host string, port int) {
	if h, p, err := net.SplitHostPort(pattern); err == nil {
		if n, err := strconv.Atoi(p); err == nil {
			return h, n
		}
	}
	return strings.Trim(pattern, "[]"), 22
}

// keyClass is the classification of one candidate key against what is already
// trusted.
type keyClass struct {
	Status              string
	PreviousFingerprint string
	// PreviousSource says whose key PreviousFingerprint is: "runner" when this
	// runner already trusted a different key for the host, "server" when the
	// key differs from the one the server itself pins.
	PreviousSource   string
	MatchedServerPin bool
}

// serverPin returns the type and fingerprint of the key the SERVER pins for a
// host (ssh_hosts.host_key, captured when the server itself connected), or ""
// when it pins none. The host is looked up by its scope name when the decision
// is for a scope, and otherwise by address and port.
//
// Whose record it reads matters since records have owners (LR-69): the pin is
// the "independent second opinion" that lets a key be approved by exception, so
// a record another agency wrote for the same host name or address must not be
// the one consulted — its administrator could pin any key they liked there.
// For a scope, the record is the one that scope's runs would resolve
// (execspec.HostRecordForScopeSQL). By address, it is a record the RUNNER's own
// agencies could have written: imported for one of their scopes, hand-written
// by one of them, or Global's.
func serverPin(ctx context.Context, q hostkeys.Queryer, runnerID, hostName, scopeName, pattern string) (keyType, fingerprint string) {
	var raw sql.NullString
	if hostName != "" {
		_ = q.QueryRowContext(ctx, `
			SELECT h.host_key FROM ssh_hosts h
			 WHERE h.hostname = ? AND COALESCE(h.host_key, '') <> ''
			   AND `+execspec.HostRecordForScopeSQL+`
			 ORDER BY `+execspec.HostRecordOrderSQL+`
			 LIMIT 1`, hostName, scopeName, scopeName).Scan(&raw)
	} else {
		bare, port := patternPort(pattern)
		_ = q.QueryRowContext(ctx, `
			SELECT h.host_key FROM ssh_hosts h
			 WHERE (h.address = ? OR (COALESCE(h.address, '') = '' AND h.hostname = ?))
			   AND COALESCE(NULLIF(h.port, 0), 22) = ? AND COALESCE(h.host_key, '') <> ''
			   AND ((h.scope_id IS NULL AND (h.owner_agency = ? OR h.owner_agency IN (
			            SELECT ra.agency_id FROM runner_agencies ra WHERE ra.runner_id = ?)))
			        OR EXISTS (SELECT 1 FROM scope_agencies sa JOIN runner_agencies ra ON ra.agency_id = sa.agency_id
			                    WHERE sa.scope_id = h.scope_id AND ra.runner_id = ?))
			 ORDER BY h.last_modified_at DESC, h.id DESC LIMIT 1`, bare, bare, port, agencyid.Global, runnerID, runnerID).Scan(&raw)
	}
	if !raw.Valid || raw.String == "" {
		return "", ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(raw.String))
	if err != nil {
		return "", ""
	}
	return pub.Type(), ssh.FingerprintSHA256(pub)
}

// classifyKey says how a candidate key relates to what is already trusted. What
// this RUNNER trusts for the host is asked first, because that is what an
// approval would replace; the server's own pin is the independent second
// opinion that lets a whole scope's worth of keys be reviewed by exception.
// A pin of a different key TYPE says nothing either way — hosts present
// several — so it neither matches nor contradicts.
func classifyKey(ctx context.Context, q hostkeys.Queryer, runnerID, pattern, hostName, scopeName, keyType, fingerprint string) keyClass {
	var inForce sql.NullString
	_ = q.QueryRowContext(ctx, `
		SELECT fingerprint FROM host_key_ledger
		 WHERE runner_id = ? AND host = ? AND key_type = ?
		   AND decision = 'approved' AND superseded_at IS NULL
		 ORDER BY id DESC LIMIT 1`, runnerID, pattern, keyType).Scan(&inForce)

	pinType, pinFP := serverPin(ctx, q, runnerID, hostName, scopeName, pattern)
	matched := pinType == keyType && pinFP == fingerprint

	switch {
	case inForce.Valid && inForce.String == fingerprint:
		return keyClass{Status: keyStatusTrusted, MatchedServerPin: matched}
	case inForce.Valid:
		return keyClass{Status: keyStatusChanged, PreviousFingerprint: inForce.String, PreviousSource: "runner", MatchedServerPin: matched}
	case matched:
		return keyClass{Status: keyStatusMatch, MatchedServerPin: true}
	case pinType == keyType && pinFP != "":
		return keyClass{Status: keyStatusChanged, PreviousFingerprint: pinFP, PreviousSource: "server"}
	default:
		return keyClass{Status: keyStatusNew}
	}
}

// ledgerEntry is one row about to be written to host_key_ledger.
type ledgerEntry struct {
	scopeID, scopeName, host, hostName string
	keyType, fingerprint, line         string
	decision, source                   string
	class                              keyClass
}

// writeLedger appends decisions for one runner in one batch, inside tx. An
// APPROVAL supersedes whatever this runner trusted for the same (host, key
// type): the older row stops being in force and, if it had been delivered, the
// next poll tells the runner to drop its line. A rejection or a removal is
// recorded already-superseded: it is the record of an act, never a key in force.
func writeLedger(ctx context.Context, tx *sql.Tx, batchID, runnerID, runnerName, actor, ts string, entries []ledgerEntry) error {
	for _, e := range entries {
		var superseded any
		if e.decision == "approved" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE host_key_ledger SET superseded_at = ?
				 WHERE runner_id = ? AND host = ? AND key_type = ?
				   AND decision = 'approved' AND superseded_at IS NULL`,
				ts, runnerID, e.host, e.keyType); err != nil {
				return fmt.Errorf("supersede host key: %w", err)
			}
		} else {
			superseded = ts
		}
		var prev any
		if e.class.PreviousFingerprint != "" {
			prev = e.class.PreviousFingerprint
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO host_key_ledger
			    (batch_id, runner_id, runner_name, scope_id, scope_name, host, host_name,
			     key_type, fingerprint, known_hosts_line, decision, source,
			     matched_server_pin, previous_fingerprint, actor, decided_at, superseded_at)
			VALUES (?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			batchID, runnerID, runnerName, e.scopeID, e.scopeName, e.host, e.hostName,
			e.keyType, e.fingerprint, e.line, e.decision, e.source,
			e.class.MatchedServerPin, prev, actor, ts, superseded); err != nil {
			return fmt.Errorf("record host key decision: %w", err)
		}
	}
	return nil
}

// redeliverUnconfirmed queues an in-force key for delivery again when the
// runner has never reported it. It is what "approve" means for a key that is
// already trusted: there is no decision left to record, but if the earlier
// delivery was lost this is the operator saying so. The agent skips a line it
// already has, so a needless re-send changes nothing.
func redeliverUnconfirmed(ctx context.Context, tx *sql.Tx, runnerID, pattern, keyType string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE host_key_ledger SET delivered_at = NULL
		 WHERE runner_id = ? AND host = ? AND key_type = ?
		   AND decision = 'approved' AND superseded_at IS NULL AND confirmed_at IS NULL`,
		runnerID, pattern, keyType)
	return err
}

func (s *Service) runnerName(ctx context.Context, q hostkeys.Queryer, runnerID string) (string, bool) {
	var name string
	if err := q.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&name); err != nil {
		return "", false
	}
	return name, true
}

// ── Pending keys of one runner, classified ──────────────────────────────────

type pendingKeyRow struct {
	ID                  string  `json:"id"`
	Host                string  `json:"host"` // known_hosts form: what the runner will look up
	HostName            *string `json:"hostName"`
	ScopeName           *string `json:"scopeName"`
	KeyType             string  `json:"keyType"`
	Fingerprint         string  `json:"fingerprint"`
	ScannedAt           string  `json:"scannedAt"`
	Status              string  `json:"status"`
	PreviousFingerprint string  `json:"previousFingerprint,omitempty"`
	PreviousSource      string  `json:"previousSource,omitempty"`
	MatchedServerPin    bool    `json:"matchedServerPin"`
}

// HandleListRunnerPendingHostKeys returns one runner's unresolved scanned keys,
// each classified against what is already trusted — the review screen's rows.
// GET /api/v1/runners/{id}/host-keys/pending
func (s *Service) HandleListRunnerPendingHostKeys(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT id, host, host_name, scope_name, key_type, fingerprint, scanned_at
		  FROM pending_host_keys
		 WHERE runner_id = ? AND approved_at IS NULL AND rejected_at IS NULL
		 ORDER BY COALESCE(scope_name, ''), COALESCE(host_name, host), key_type`, runnerID)
	if err != nil {
		s.log.Error("list runner pending host keys", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	out := []pendingKeyRow{}
	for rows.Next() {
		var p pendingKeyRow
		var hostName, scopeName sql.NullString
		if err := rows.Scan(&p.ID, &p.Host, &hostName, &scopeName, &p.KeyType, &p.Fingerprint, &p.ScannedAt); err != nil {
			rows.Close()
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		if hostName.Valid {
			p.HostName = &hostName.String
		}
		if scopeName.Valid {
			p.ScopeName = &scopeName.String
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	rows.Close()
	// Classified after the cursor is closed: classification reads, and the
	// pool must not be asked for a second connection while one is held open.
	for i := range out {
		p := &out[i]
		p.Host = hostkeys.Pattern(p.Host) // a row scanned before the upgrade holds host:port
		cls := classifyKey(r.Context(), s.db, runnerID, p.Host, deref(p.HostName), deref(p.ScopeName), p.KeyType, p.Fingerprint)
		p.Status, p.PreviousFingerprint, p.PreviousSource, p.MatchedServerPin =
			cls.Status, cls.PreviousFingerprint, cls.PreviousSource, cls.MatchedServerPin
	}
	httpx.JSON(w, http.StatusOK, out)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Batch resolve ───────────────────────────────────────────────────────────

type resolveBatchRequest struct {
	Approve []string `json:"approve"`
	Reject  []string `json:"reject"`
	// AcknowledgeChanged names the approved keys the operator saw marked as
	// CHANGED and ticked anyway. A key that turns out to replace a trusted one
	// without being named here refuses the batch (409 review_stale).
	AcknowledgeChanged []string `json:"acknowledgeChanged"`
}

type batchResult struct {
	// BatchID is empty when nothing was recorded (every key was already trusted).
	BatchID  string `json:"batchId"`
	Approved int    `json:"approved"`
	Rejected int    `json:"rejected"`
	// Unchanged counts approved keys the runner already trusted: acknowledged,
	// with nothing new to record.
	Unchanged int `json:"unchanged"`
}

// resolveBatch approves and rejects pending scanned keys of ONE runner in one
// transaction and one batch. Every id must be an unresolved pending key of that
// runner, or nothing is written.
func (s *Service) resolveBatch(ctx context.Context, runnerID string, approve, reject, acknowledgeChanged []string, actor string) (batchResult, error) {
	var res batchResult
	approve, reject = normalizeHosts(approve), normalizeHosts(reject)
	acknowledged := map[string]bool{}
	for _, id := range acknowledgeChanged {
		acknowledged[id] = true
	}
	if len(approve)+len(reject) == 0 {
		return res, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck

	name, ok := s.runnerName(ctx, tx, runnerID)
	if !ok {
		return res, ErrHostKeyNotPending
	}

	ts := now()
	var entries []ledgerEntry
	scopes := map[string]bool{}
	for _, group := range []struct {
		ids      []string
		decision string
	}{{approve, "approved"}, {reject, "rejected"}} {
		for _, id := range group.ids {
			var e ledgerEntry
			var scopeID, scopeName, hostName sql.NullString
			err := tx.QueryRowContext(ctx, `
				SELECT host, key_type, fingerprint, known_hosts_line, scope_id, scope_name, host_name
				  FROM pending_host_keys
				 WHERE id = ? AND runner_id = ? AND approved_at IS NULL AND rejected_at IS NULL`, id, runnerID).
				Scan(&e.host, &e.keyType, &e.fingerprint, &e.line, &scopeID, &scopeName, &hostName)
			if errors.Is(err, sql.ErrNoRows) {
				return res, ErrHostKeyNotPending
			}
			if err != nil {
				return res, err
			}
			e.scopeID, e.scopeName, e.hostName = scopeID.String, scopeName.String, hostName.String
			e.decision, e.source = group.decision, "scan"

			// trusted_at is stamped with the decision: delivery is the ledger's
			// job now, and nothing should read this row as still to be sent.
			// group.decision is one of two literals above, never caller text.
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
				UPDATE pending_host_keys SET %[1]s_at = ?, %[1]s_by = ?, trusted_at = ? WHERE id = ?`, group.decision),
				ts, actor, ts, id); err != nil {
				return res, fmt.Errorf("resolve host key: %w", err)
			}

			if group.decision == "approved" {
				// Re-derive the line from the key it carries, and hold it to the
				// host and fingerprint the operator was shown.
				hosts, key, problem := parseSingleKeyLine(e.line)
				if problem != "" || len(hosts) != 1 || hosts[0] != hostkeys.Pattern(e.host) ||
					key.Type() != e.keyType || ssh.FingerprintSHA256(key) != e.fingerprint {
					return res, fmt.Errorf("%w (%s, %s)", ErrHostKeyUnverifiable, e.host, e.keyType)
				}
				e.host = hosts[0]
				e.line = renderKnownHostsLine(e.host, key)
				e.class = classifyKey(ctx, tx, runnerID, e.host, e.hostName, e.scopeName, e.keyType, e.fingerprint)
				if e.class.Status == keyStatusTrusted {
					if err := redeliverUnconfirmed(ctx, tx, runnerID, e.host, e.keyType); err != nil {
						return res, err
					}
					res.Unchanged++
					continue
				}
				if e.class.Status == keyStatusChanged && !acknowledged[id] {
					return res, fmt.Errorf("%w (%s, %s now replaces a trusted key)", ErrReviewStale, e.host, e.keyType)
				}
				res.Approved++
			} else {
				res.Rejected++
			}
			entries = append(entries, e)
			if e.scopeName != "" {
				scopes[e.scopeName] = true
			}
		}
	}
	if len(entries) > 0 {
		res.BatchID = db.NewID()
		if err := writeLedger(ctx, tx, res.BatchID, runnerID, name, actor, ts, entries); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if len(entries) > 0 {
		var scopeNames []string
		for sc := range scopes {
			scopeNames = append(scopeNames, sc)
		}
		sort.Strings(scopeNames)
		s.auditHostKeyBatch(ctx, actor, name, scopeNames, "scan", res.Approved, res.Rejected, res.BatchID)
	}
	return res, nil
}

// auditHostKeyBatch writes the ONE change-log row a batch gets. It names the
// runner — the thing the pre-ledger row left out — and the batch id, which is
// what the change log expands into the individual keys.
func (s *Service) auditHostKeyBatch(ctx context.Context, actor, runnerName string, scopes []string, source string, approved, rejected int, batchID string) {
	target := "runner:" + runnerName
	if len(scopes) > 0 {
		target += " (scope " + strings.Join(scopes, ", ") + ")"
	}
	var parts []string
	if approved > 0 {
		parts = append(parts, strconv.Itoa(approved)+" approved")
	}
	if rejected > 0 {
		parts = append(parts, strconv.Itoa(rejected)+" rejected")
	}
	details := strings.Join(parts, ", ") + "; source " + source + "; batch " + batchID
	_ = settings.WriteChangeLog(ctx, s.db, actor, "Host Keys", "resolved", target, details)
}

// HandleResolveHostKeyBatch approves and rejects pending keys of one runner.
// POST /api/v1/runners/{id}/host-keys/resolve-batch
func (s *Service) HandleResolveHostKeyBatch(w http.ResponseWriter, r *http.Request) {
	var req resolveBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if len(req.Approve)+len(req.Reject) == 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "approve or reject must name at least one key")
		return
	}
	rejecting := map[string]bool{}
	for _, id := range req.Reject {
		rejecting[id] = true
	}
	for _, id := range req.Approve {
		if rejecting[id] {
			httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "a key cannot be both approved and rejected")
			return
		}
	}
	res, err := s.resolveBatch(r.Context(), r.PathValue("id"), req.Approve, req.Reject, req.AcknowledgeChanged, sessionActor(r))
	if s.failResolve(w, err) {
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// ── Provide (paste) and carry ───────────────────────────────────────────────

// keyCandidate is one key on the review screen that did not come from a scan:
// a pasted line, or a key carried from another runner.
type keyCandidate struct {
	// Input is the pasted line this candidate came from, as typed. Empty for a
	// carried key.
	Input               string `json:"input,omitempty"`
	Host                string `json:"host"`
	Hashed              bool   `json:"hashed"`
	KeyType             string `json:"keyType"`
	Fingerprint         string `json:"fingerprint"`
	Status              string `json:"status"`
	PreviousFingerprint string `json:"previousFingerprint,omitempty"`
	PreviousSource      string `json:"previousSource,omitempty"`
	MatchedServerPin    bool   `json:"matchedServerPin"`
	// Error is why this line cannot be accepted. Any candidate with one makes
	// the whole paste uncommittable: it is all or nothing.
	Error string `json:"error,omitempty"`

	line string // the known_hosts line that would be delivered
}

func (c *keyCandidate) classify(cls keyClass) {
	c.Status, c.PreviousFingerprint, c.PreviousSource, c.MatchedServerPin =
		cls.Status, cls.PreviousFingerprint, cls.PreviousSource, cls.MatchedServerPin
}

// parseProvidedLine turns one pasted known_hosts line into candidates, one per
// host on the line, so each host is reviewed — and later replaced or removed —
// on its own. A marker line is refused, because "trust this key for this host"
// is the only thing this screen can mean. So is a wildcard or negated host: a
// pasted `*` would trust one key for every host the runner ever dials, behind a
// row that looks like any other.
func parseProvidedLine(raw string) []keyCandidate {
	input := strings.TrimSpace(raw)
	hosts, key, problem := parseSingleKeyLine(input)
	if problem != "" {
		return []keyCandidate{{Input: input, Error: problem}}
	}
	out := make([]keyCandidate, 0, len(hosts))
	for _, h := range hosts {
		c := keyCandidate{Input: input, Host: h, KeyType: key.Type(), Fingerprint: ssh.FingerprintSHA256(key)}
		switch {
		case h == "":
			c.Error = "empty host on the line"
		case knownhostsline.IsHashed(h):
			// A hashed host is taken as it is: the name cannot be recovered, and
			// re-normalising it would break the hash.
			c.Hashed = true
		case strings.ContainsAny(h, "*?!"):
			c.Error = "wildcard and negated hosts are not accepted here; name the host"
		default:
			c.Host = knownhosts.Normalize(h)
		}
		// The runner's verifier refuses its WHOLE file over one host it cannot
		// read (an empty one, an unbalanced bracket, a malformed hash), and
		// every run on that runner then fails. Nothing of that shape is written.
		if c.Error == "" && !knownhostsline.ValidPattern(c.Host) {
			c.Error = "not a host a runner can look up (expected a name or address, or [host]:port)"
		}
		c.line = renderKnownHostsLine(c.Host, key)
		out = append(out, c)
	}
	return out
}

// keySelection names one row an operator ticked on the review screen, by what
// they were shown: the host, the key (its type and FINGERPRINT), and the status
// it had. The fingerprint is what binds a commit to the review — host and type
// alone would let a key that changed at the source since the screen was drawn
// ride through under the old row.
type keySelection struct {
	Host        string `json:"host"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
}

// selectCandidates keeps the candidates the operator ticked. Every selection
// must still be among the candidates, key for key, or the review is stale.
func selectCandidates(candidates []keyCandidate, sel []keySelection) ([]keyCandidate, error) {
	have := map[string]keyCandidate{}
	for _, c := range candidates {
		have[c.Host+"\x00"+c.KeyType+"\x00"+c.Fingerprint] = c
	}
	out := make([]keyCandidate, 0, len(sel))
	for _, s := range sel {
		c, ok := have[s.Host+"\x00"+s.KeyType+"\x00"+s.Fingerprint]
		if !ok {
			return nil, fmt.Errorf("%w (%s, %s)", ErrReviewStale, s.Host, s.KeyType)
		}
		c.Status = s.Status // what the operator saw; the commit compares it with what is true now
		out = append(out, c)
	}
	return out, nil
}

type provideRequest struct {
	Lines   []string `json:"lines"`
	ScopeID string   `json:"scopeId"`
	DryRun  bool     `json:"dryRun"`
	// Select names the reviewed rows to commit. Required on a commit: there is
	// no "trust whatever these lines turn out to say".
	Select *[]keySelection `json:"select"`
}

type candidatesResponse struct {
	Candidates []keyCandidate `json:"candidates"`
	// Acceptable is false when any candidate carries an error; committing would
	// be refused.
	Acceptable bool `json:"acceptable"`
	// Unverifiable counts source keys left out of a carry because their stored
	// line does not hold up (see HandleCarryHostKeys). Always 0 for a paste.
	Unverifiable int `json:"unverifiable"`
}

// HandleProvideHostKeys accepts known_hosts lines an operator already has. With
// dryRun it only parses and classifies — that is the confirm screen. Without,
// it records each as an approved ledger row (source `pasted`) and queues the
// delivery; a single unacceptable line refuses the whole paste.
// POST /api/v1/runners/{id}/host-keys/provide
func (s *Service) HandleProvideHostKeys(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	var req provideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	var lines []string
	for _, block := range req.Lines {
		for _, l := range strings.Split(block, "\n") {
			l = strings.TrimSpace(strings.TrimSuffix(l, "\r"))
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "at least one known_hosts line is required")
		return
	}
	if len(lines) > maxProvidedLines {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("too many lines (%d); provide at most %d at a time", len(lines), maxProvidedLines))
		return
	}
	name, ok := s.runnerName(r.Context(), s.db, runnerID)
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	var scopeName string
	if req.ScopeID != "" {
		// The scope's name is written to the ledger and the change log, so it
		// is only taken from a caller who could read it anyway.
		err := s.db.QueryRowContext(r.Context(), `SELECT name FROM scopes WHERE id = ?`, req.ScopeID).Scan(&scopeName)
		if id, ok := auth.IdentityFrom(r.Context()); err != nil || !ok || !auth.ScopeReadable(id, scopeName) {
			httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
			return
		}
	}

	var candidates []keyCandidate
	seen := map[string]bool{}
	bad := 0
	for _, l := range lines {
		for _, c := range parseProvidedLine(l) {
			if c.Error == "" {
				key := c.Host + "\x00" + c.KeyType
				if seen[key] {
					c.Error = "this host and key type appear more than once in the paste"
				}
				seen[key] = true
			}
			if c.Error == "" {
				c.classify(classifyKey(r.Context(), s.db, runnerID, c.Host, "", "", c.KeyType, c.Fingerprint))
			} else {
				bad++
			}
			candidates = append(candidates, c)
		}
	}
	if req.DryRun {
		httpx.JSON(w, http.StatusOK, candidatesResponse{Candidates: candidates, Acceptable: bad == 0})
		return
	}
	if bad > 0 {
		httpx.FailDetails(w, http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("%d line(s) cannot be accepted; nothing was trusted", bad),
			map[string]any{"candidates": candidates})
		return
	}
	if req.Select == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			"select is required: review the keys with dryRun, then name the ones to trust")
		return
	}
	chosen, err := selectCandidates(candidates, *req.Select)
	if s.failResolve(w, err) {
		return
	}
	res, err := s.commitCandidates(r.Context(), runnerID, name, req.ScopeID, scopeName, "pasted", chosen, sessionActor(r))
	if s.failResolve(w, err) {
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// commitCandidates records the selected candidates as approved ledger rows in
// one batch. Classification is redone inside the transaction, so what is
// recorded as replaced is what was in force at the moment of the write. If that
// would REPLACE a trusted key and the operator did not see the row as a changed
// one (each candidate's Status is what they were shown), the whole commit is
// refused as stale: a changed key is accepted knowingly or not at all. A key
// the runner already trusts is not recorded again.
func (s *Service) commitCandidates(ctx context.Context, runnerID, runnerName, scopeID, scopeName, source string, candidates []keyCandidate, actor string) (batchResult, error) {
	var res batchResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck
	var entries []ledgerEntry
	for _, c := range candidates {
		cls := classifyKey(ctx, tx, runnerID, c.Host, "", "", c.KeyType, c.Fingerprint)
		if cls.Status == keyStatusTrusted {
			if err := redeliverUnconfirmed(ctx, tx, runnerID, c.Host, c.KeyType); err != nil {
				return res, err
			}
			res.Unchanged++
			continue
		}
		if cls.Status == keyStatusChanged && c.Status != keyStatusChanged {
			return res, fmt.Errorf("%w (%s, %s now replaces a trusted key)", ErrReviewStale, c.Host, c.KeyType)
		}
		entries = append(entries, ledgerEntry{
			scopeID: scopeID, scopeName: scopeName, host: c.Host,
			keyType: c.KeyType, fingerprint: c.Fingerprint, line: c.line,
			decision: "approved", source: source, class: cls,
		})
	}
	res.Approved = len(entries)
	if len(entries) > 0 {
		res.BatchID = db.NewID()
		if err := writeLedger(ctx, tx, res.BatchID, runnerID, runnerName, actor, now(), entries); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if len(entries) > 0 {
		var scopes []string
		if scopeName != "" {
			scopes = []string{scopeName}
		}
		s.auditHostKeyBatch(ctx, actor, runnerName, scopes, source, res.Approved, 0, res.BatchID)
	}
	return res, nil
}

type carryRequest struct {
	DryRun bool `json:"dryRun"`
	// Select names the reviewed rows to commit. Required on a commit.
	Select *[]keySelection `json:"select"`
}

// HandleCarryHostKeys copies the keys ANOTHER runner trusts onto this one: the
// keys in force for the source that this runner does not already trust. It is
// the cheap half of replacing a runner, and deliberately not automatic — the
// replacement sits at a different network position, so the same address is not
// guaranteed to be the same machine, and the dryRun answer is the review screen
// an operator reads before saying it is. The source may be deregistered; its
// ledger rows outlive it.
// POST /api/v1/runners/{id}/host-keys/carry?from={runnerId}
func (s *Service) HandleCarryHostKeys(w http.ResponseWriter, r *http.Request) {
	runnerID, fromID := r.PathValue("id"), r.URL.Query().Get("from")
	var req carryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if fromID == "" || fromID == runnerID {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error", "from must name another runner")
		return
	}
	name, ok := s.runnerName(r.Context(), s.db, runnerID)
	if !ok {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT host, key_type, fingerprint, known_hosts_line
		  FROM host_key_ledger
		 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL
		 ORDER BY host, key_type`, fromID)
	if err != nil {
		s.log.Error("carry host keys", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	var source []keyCandidate
	for rows.Next() {
		var c keyCandidate
		if err := rows.Scan(&c.Host, &c.KeyType, &c.Fingerprint, &c.line); err != nil {
			rows.Close()
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		c.Hashed = strings.HasPrefix(c.Host, "|1|")
		source = append(source, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	rows.Close()

	// Drop what this runner already trusts: carrying it again would only add
	// noise to the review. And drop any row whose stored line is not exactly its
	// own host and key. Rows written by this version always are; a row carried
	// over by migration 1200 holds a line an agent supplied before the server
	// started rendering them, and that is good enough for the runner that
	// supplied it but not for handing to another.
	candidates := []keyCandidate{}
	unverifiable := 0
	for _, c := range source {
		hosts, key, problem := parseSingleKeyLine(c.line)
		if problem != "" || len(hosts) != 1 || hosts[0] != c.Host ||
			key.Type() != c.KeyType || ssh.FingerprintSHA256(key) != c.Fingerprint {
			unverifiable++
			continue
		}
		cls := classifyKey(r.Context(), s.db, runnerID, c.Host, "", "", c.KeyType, c.Fingerprint)
		if cls.Status == keyStatusTrusted {
			continue
		}
		c.classify(cls)
		candidates = append(candidates, c)
	}
	if req.DryRun {
		httpx.JSON(w, http.StatusOK, candidatesResponse{Candidates: candidates, Acceptable: true, Unverifiable: unverifiable})
		return
	}
	if req.Select == nil {
		httpx.Fail(w, http.StatusUnprocessableEntity, "validation_error",
			"select is required: review the keys with dryRun, then name the ones to trust")
		return
	}
	chosen, err := selectCandidates(candidates, *req.Select)
	if s.failResolve(w, err) {
		return
	}
	res, err := s.commitCandidates(r.Context(), runnerID, name, "", "", "carried", chosen, sessionActor(r))
	if s.failResolve(w, err) {
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// ── Remove and re-send ──────────────────────────────────────────────────────

// HandleRemoveHostKey takes an in-force key away from a runner: the approved
// row stops being in force, a `removed` row records who did it, and the next
// poll tells the runner to delete the line.
// POST /api/v1/runners/{id}/host-keys/{ledgerId}/remove
func (s *Service) HandleRemoveHostKey(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	ledgerID, err := strconv.ParseInt(r.PathValue("ledgerId"), 10, 64)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host key not found")
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer tx.Rollback() //nolint:errcheck

	var e ledgerEntry
	var scopeID, scopeName, hostName sql.NullString
	err = tx.QueryRowContext(r.Context(), `
		SELECT scope_id, scope_name, host, host_name, key_type, fingerprint, known_hosts_line, source
		  FROM host_key_ledger
		 WHERE id = ? AND runner_id = ? AND decision = 'approved' AND superseded_at IS NULL`, ledgerID, runnerID).
		Scan(&scopeID, &scopeName, &e.host, &hostName, &e.keyType, &e.fingerprint, &e.line, &e.source)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.Fail(w, http.StatusConflict, "conflict", "that key is not in force for this runner; reload the list")
		return
	}
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	name, ok := s.runnerName(r.Context(), tx, runnerID)
	if !ok {
		name = runnerID // the record outlives the runner; so can the removal
	}
	ts, actor := now(), sessionActor(r)
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE host_key_ledger SET superseded_at = ? WHERE id = ?`, ts, ledgerID); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	e.scopeID, e.scopeName, e.hostName = scopeID.String, scopeName.String, hostName.String
	e.decision = "removed"
	batchID := db.NewID()
	if err := writeLedger(r.Context(), tx, batchID, runnerID, name, actor, ts, []ledgerEntry{e}); err != nil {
		s.log.Error("remove host key", "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	_ = settings.WriteChangeLog(r.Context(), s.db, actor, "Host Keys", "removed",
		"runner:"+name, e.host+" ("+e.keyType+") "+e.fingerprint+"; batch "+batchID)
	httpx.JSON(w, http.StatusOK, batchResult{BatchID: batchID})
}

// HandleResendHostKey queues an in-force key for delivery again — the remedy
// for "approved here, not present in the runner's file".
// POST /api/v1/runners/{id}/host-keys/{ledgerId}/resend
func (s *Service) HandleResendHostKey(w http.ResponseWriter, r *http.Request) {
	ledgerID, err := strconv.ParseInt(r.PathValue("ledgerId"), 10, 64)
	if err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "host key not found")
		return
	}
	res, err := s.db.ExecContext(r.Context(), `
		UPDATE host_key_ledger SET delivered_at = NULL, confirmed_at = NULL
		 WHERE id = ? AND runner_id = ? AND decision = 'approved' AND superseded_at IS NULL`,
		ledgerID, r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusConflict, "conflict", "that key is not in force for this runner; reload the list")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ── Delivery to the runner ──────────────────────────────────────────────────

// takeLedgerLines returns the known_hosts lines of a runner's ledger rows
// matching `where` and stamps `stampCol` on them (deliver-once). Delivery is
// at-most-once, marked before the response is written: if the write is lost the
// host simply stays as it was — untrusted, or trusted a little longer — and the
// runner's next report shows the difference.
func (s *Service) takeLedgerLines(ctx context.Context, runnerID, where, stampCol string) []string {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, known_hosts_line FROM host_key_ledger WHERE runner_id = ? AND `+where+` ORDER BY id`, runnerID)
	if err != nil {
		return nil
	}
	var ids []int64
	var lines []string
	for rows.Next() {
		var id int64
		var line string
		if err := rows.Scan(&id, &line); err != nil {
			continue
		}
		ids = append(ids, id)
		lines = append(lines, line)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil
	}
	ts := now()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE host_key_ledger SET `+stampCol+` = ? WHERE id = ?`, ts, id); err != nil {
			s.log.Error("stamp host key delivery", "id", id, "column", stampCol, "error", err)
		}
	}
	return lines
}

// takeTrustHosts returns the approved, in-force lines not yet sent to a runner.
func (s *Service) takeTrustHosts(ctx context.Context, runnerID string) []string {
	return s.takeLedgerLines(ctx, runnerID,
		`decision = 'approved' AND delivered_at IS NULL AND superseded_at IS NULL`, "delivered_at")
}

// takeUntrustHosts returns the lines to REMOVE from a runner's file: approved
// rows that are no longer in force and whose removal has not been sent.
//
// Whether the row was ever DELIVERED is deliberately not asked. delivered_at is
// a to-do marker, not a history — "Send again" clears it — so a key that is in
// the runner's file can read as undelivered, and a removal gated on it would
// never be sent, leaving a retired key trusted. Removing a line that is not
// there costs nothing; the agent skips it.
//
// A line that an in-force row still carries is left out — two approvals can
// render the same line, and taking it away would untrust a key that is meant
// to be trusted.
func (s *Service) takeUntrustHosts(ctx context.Context, runnerID string) []string {
	// Settle the left-out rows first: there is nothing to send for them, and a
	// row left looking unsent is one retention will never prune.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE host_key_ledger SET untrusted_at = ?
		 WHERE runner_id = ? AND decision = 'approved'
		   AND superseded_at IS NOT NULL AND untrusted_at IS NULL
		   AND EXISTS (SELECT 1 FROM host_key_ledger f
		                WHERE f.runner_id = host_key_ledger.runner_id
		                  AND f.known_hosts_line = host_key_ledger.known_hosts_line
		                  AND f.decision = 'approved' AND f.superseded_at IS NULL)`, now(), runnerID); err != nil {
		s.log.Error("settle superseded host keys", "runner_id", runnerID, "error", err)
		return nil
	}
	return s.takeLedgerLines(ctx, runnerID,
		`decision = 'approved' AND superseded_at IS NOT NULL AND untrusted_at IS NULL`,
		"untrusted_at")
}

// takeKnownHostsRequest consumes an operator's request for a fresh report.
func (s *Service) takeKnownHostsRequest(ctx context.Context, runnerID string) bool {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runners SET known_hosts_requested = 0 WHERE id = ? AND known_hosts_requested = 1`, runnerID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n == 1
}

// HandleRequestKnownHosts asks a runner to report its known_hosts file again on
// its next poll. POST /api/v1/runners/{id}/known-hosts/refresh
func (s *Service) HandleRequestKnownHosts(w http.ResponseWriter, r *http.Request) {
	res, err := s.db.ExecContext(r.Context(),
		`UPDATE runners SET known_hosts_requested = 1 WHERE id = ?`, r.PathValue("id"))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// fileHoldsKey is the test "the runner's reported file holds the key of this
// ledger row", written for use inside EXISTS against the row being updated. A
// plain line only (a @revoked line carrying the key is the opposite), the same
// key, and — unless the line is hashed and so names nobody — the same HOST.
// Without the host a key approved for web01 would read as present because some
// other name carries it, and a lost delivery would look confirmed.
const fileHoldsKey = `
	SELECT 1 FROM runner_known_hosts k
	 WHERE k.runner_id = host_key_ledger.runner_id AND k.marker = ''
	   AND k.key_type = host_key_ledger.key_type
	   AND k.fingerprint = host_key_ledger.fingerprint
	   AND (k.hashed = 1 OR instr(',' || k.hosts || ',', ',' || host_key_ledger.host || ',') > 0)`

// untrustRetryAfter is how long a removal that the runner's own report shows
// did not land is left before it is sent again.
const untrustRetryAfter = time.Hour

type knownHostsEntry struct {
	Line        int    `json:"line"`
	Hosts       string `json:"hosts"`
	Hashed      bool   `json:"hashed"`
	Marker      string `json:"marker"`
	KeyType     string `json:"keyType"`
	Fingerprint string `json:"fingerprint"`
}

type knownHostsReport struct {
	Entries   []knownHostsEntry `json:"entries"`
	Truncated bool              `json:"truncated"`
}

// HandleUploadKnownHosts ingests a runner's report of its own known_hosts file
// (runner-key auth, ownership-guarded). The report REPLACES the previous one —
// it is the file as it is now — and any approved key whose fingerprint appears
// in it is stamped confirmed: the only evidence the server ever gets that a
// delivery landed.
// POST /api/v1/runners/{id}/known-hosts
func (s *Service) HandleUploadKnownHosts(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	callerRunnerID, ok := auth.RunnerIDFrom(r.Context())
	if !ok || callerRunnerID != runnerID {
		httpx.Fail(w, http.StatusNotFound, "not_found", "runner not found")
		return
	}
	var req knownHostsReport
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.Fail(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	truncated := req.Truncated
	if len(req.Entries) > maxKnownHostsEntries {
		req.Entries, truncated = req.Entries[:maxKnownHostsEntries], true
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(r.Context(), `DELETE FROM runner_known_hosts WHERE runner_id = ?`, runnerID); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	accepted := 0
	for _, e := range req.Entries {
		// The agent reports a file a human may have edited; nothing in it is
		// trusted as text. An entry carrying a control character, or a marker
		// that is not one of OpenSSH's two, is dropped.
		if e.KeyType == "" || e.Fingerprint == "" || hasCtrl(e.Hosts) || hasCtrl(e.KeyType) || hasCtrl(e.Fingerprint) {
			continue
		}
		if e.Marker != "" && e.Marker != "revoked" && e.Marker != "cert-authority" {
			continue
		}
		if e.Hashed {
			e.Hosts = "" // a hash is not a name; never store one as if it were
		}
		if _, err := tx.ExecContext(r.Context(), `
			INSERT OR REPLACE INTO runner_known_hosts (runner_id, line_no, hosts, hashed, marker, key_type, fingerprint)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runnerID, e.Line, e.Hosts, e.Hashed, e.Marker, e.KeyType, e.Fingerprint); err != nil {
			s.log.Error("store known_hosts entry", "runner_id", runnerID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
		accepted++
	}
	ts := now()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE runners SET known_hosts_reported_at = ?, known_hosts_truncated = ? WHERE id = ?`,
		ts, truncated, runnerID); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	// Reconcile the ledger with what the file actually holds. Three statements,
	// each the report being believed over the server's own bookkeeping.
	for _, st := range []struct {
		skip bool
		sql  string
		args []any
	}{
		// 1. An in-force key the file holds is confirmed.
		{sql: `
			UPDATE host_key_ledger SET confirmed_at = ?
			 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL
			   AND delivered_at IS NOT NULL AND confirmed_at IS NULL
			   AND EXISTS (` + fileHoldsKey + `)`, args: []any{ts, runnerID}},
		// 2. An in-force key the file does NOT hold is no longer confirmed, however
		//    it was confirmed before: the file was reset, or the line edited out.
		//    That is what lets a later approval of the same key send it again.
		//    Not on a truncated report — absence from part of a file proves nothing.
		{skip: truncated, sql: `
			UPDATE host_key_ledger SET confirmed_at = NULL
			 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL
			   AND confirmed_at IS NOT NULL
			   AND NOT EXISTS (` + fileHoldsKey + `)`, args: []any{runnerID}},
		// 3. A retired key whose removal was sent but which is STILL in the file,
		//    on a line of its own that the agent can remove, is queued for removal
		//    again: the poll that carried the first one may never have arrived.
		//    At most hourly per key, so a file the agent cannot rewrite does not
		//    turn every report into another removal and another report.
		{sql: `
			UPDATE host_key_ledger SET untrusted_at = NULL
			 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NOT NULL
			   AND untrusted_at IS NOT NULL AND untrusted_at < ?
			   AND EXISTS (SELECT 1 FROM runner_known_hosts k
			                WHERE k.runner_id = host_key_ledger.runner_id AND k.marker = '' AND k.hashed = 0
			                  AND k.hosts = host_key_ledger.host
			                  AND k.key_type = host_key_ledger.key_type
			                  AND k.fingerprint = host_key_ledger.fingerprint)
			   AND NOT EXISTS (SELECT 1 FROM host_key_ledger f
			                    WHERE f.runner_id = host_key_ledger.runner_id
			                      AND f.known_hosts_line = host_key_ledger.known_hosts_line
			                      AND f.decision = 'approved' AND f.superseded_at IS NULL)`,
			args: []any{runnerID, time.Now().UTC().Add(-untrustRetryAfter).Format(time.RFC3339)}},
	} {
		if st.skip {
			continue
		}
		if _, err := tx.ExecContext(r.Context(), st.sql, st.args...); err != nil {
			s.log.Error("reconcile host keys with the runner's report", "runner_id", runnerID, "error", err)
			httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"accepted": accepted, "truncated": truncated})
}

// ── Views ───────────────────────────────────────────────────────────────────

type ledgerRow struct {
	ID                  int64   `json:"id"`
	BatchID             string  `json:"batchId"`
	RunnerID            string  `json:"runnerId"`
	RunnerName          string  `json:"runnerName"`
	ScopeName           *string `json:"scopeName"`
	Host                string  `json:"host"`
	HostName            *string `json:"hostName"`
	KeyType             string  `json:"keyType"`
	Fingerprint         string  `json:"fingerprint"`
	Decision            string  `json:"decision"`
	Source              string  `json:"source"`
	MatchedServerPin    bool    `json:"matchedServerPin"`
	PreviousFingerprint *string `json:"previousFingerprint"`
	Actor               string  `json:"actor"`
	DecidedAt           string  `json:"decidedAt"`
	DeliveredAt         *string `json:"deliveredAt"`
	ConfirmedAt         *string `json:"confirmedAt"`
	SupersededAt        *string `json:"supersededAt"`
	// PresentInFile is set on in-force rows once the runner has reported its
	// file: whether this key is in it. Absent = no report yet.
	PresentInFile *bool `json:"presentInFile,omitempty"`
}

const ledgerSelect = `
	SELECT id, batch_id, runner_id, runner_name, scope_name, host, host_name, key_type, fingerprint,
	       decision, source, matched_server_pin, previous_fingerprint, actor, decided_at,
	       delivered_at, confirmed_at, superseded_at
	  FROM host_key_ledger`

func scanLedgerRows(rows *sql.Rows) ([]ledgerRow, error) {
	defer rows.Close()
	out := []ledgerRow{}
	for rows.Next() {
		var l ledgerRow
		var scope, hostName, prev, delivered, confirmed, superseded sql.NullString
		if err := rows.Scan(&l.ID, &l.BatchID, &l.RunnerID, &l.RunnerName, &scope, &l.Host, &hostName, &l.KeyType,
			&l.Fingerprint, &l.Decision, &l.Source, &l.MatchedServerPin, &prev, &l.Actor, &l.DecidedAt,
			&delivered, &confirmed, &superseded); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			src sql.NullString
			dst **string
		}{{scope, &l.ScopeName}, {hostName, &l.HostName}, {prev, &l.PreviousFingerprint},
			{delivered, &l.DeliveredAt}, {confirmed, &l.ConfirmedAt}, {superseded, &l.SupersededAt}} {
			if p.src.Valid {
				v := p.src.String
				*p.dst = &v
			}
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type knownHostsFileRow struct {
	knownHostsEntry
	// ApprovedHere reports whether this line's key is one in force in the
	// ledger for this runner. False is not an error: a file can be seeded by
	// hand, and this is how such keys are told apart.
	ApprovedHere bool `json:"approvedHere"`
}

type knownHostsView struct {
	// ReportedAt is when the runner last reported its file; null = never.
	ReportedAt *string             `json:"reportedAt"`
	Truncated  bool                `json:"truncated"`
	Entries    []knownHostsFileRow `json:"entries"`
}

type runnerHostKeysView struct {
	InForce    []ledgerRow    `json:"inForce"`
	History    []ledgerRow    `json:"history"`
	KnownHosts knownHostsView `json:"knownHosts"`
	// Pending counts the scanned keys still awaiting review for this runner.
	Pending int `json:"pending"`
}

// HandleRunnerHostKeys returns everything known about one runner's host-key
// trust, in the parts the panel shows separately: the keys in force in the
// ledger, the ledger's history, and the runner's own report of its file.
// GET /api/v1/runners/{id}/host-keys
func (s *Service) HandleRunnerHostKeys(w http.ResponseWriter, r *http.Request) {
	runnerID := r.PathValue("id")
	ctx := r.Context()
	fail := func(err error) {
		s.log.Error("runner host keys", "runner_id", runnerID, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
	}

	view := runnerHostKeysView{KnownHosts: knownHostsView{Entries: []knownHostsFileRow{}}}
	var reported sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT known_hosts_reported_at, known_hosts_truncated FROM runners WHERE id = ?`, runnerID).
		Scan(&reported, &view.KnownHosts.Truncated); err != nil && !errors.Is(err, sql.ErrNoRows) {
		fail(err)
		return
	}
	if reported.Valid {
		view.KnownHosts.ReportedAt = &reported.String
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_host_keys
		 WHERE runner_id = ? AND approved_at IS NULL AND rejected_at IS NULL`, runnerID).Scan(&view.Pending); err != nil {
		fail(err)
		return
	}

	rows, err := s.db.QueryContext(ctx, ledgerSelect+`
		 WHERE runner_id = ? AND decision = 'approved' AND superseded_at IS NULL
		 ORDER BY COALESCE(host_name, host), key_type`, runnerID)
	if err != nil {
		fail(err)
		return
	}
	if view.InForce, err = scanLedgerRows(rows); err != nil {
		fail(err)
		return
	}
	rows, err = s.db.QueryContext(ctx, ledgerSelect+`
		 WHERE runner_id = ? AND NOT (decision = 'approved' AND superseded_at IS NULL)
		 ORDER BY id DESC LIMIT 500`, runnerID)
	if err != nil {
		fail(err)
		return
	}
	if view.History, err = scanLedgerRows(rows); err != nil {
		fail(err)
		return
	}

	kRows, err := s.db.QueryContext(ctx, `
		SELECT line_no, hosts, hashed, marker, key_type, fingerprint
		  FROM runner_known_hosts WHERE runner_id = ? ORDER BY line_no`, runnerID)
	if err != nil {
		fail(err)
		return
	}
	for kRows.Next() {
		var e knownHostsFileRow
		if err := kRows.Scan(&e.Line, &e.Hosts, &e.Hashed, &e.Marker, &e.KeyType, &e.Fingerprint); err != nil {
			kRows.Close()
			fail(err)
			return
		}
		view.KnownHosts.Entries = append(view.KnownHosts.Entries, e)
	}
	if err := kRows.Err(); err != nil {
		kRows.Close()
		fail(err)
		return
	}
	kRows.Close()

	// The two lists are joined the way fileHoldsKey joins them: the same key on
	// a plain line that names the same host (a hashed line names nobody, so the
	// key alone has to do). Never on the key alone where there is a name — the
	// same key under another host is not this approval.
	holds := func(e knownHostsFileRow, k ledgerRow) bool {
		if e.Marker != "" || e.KeyType != k.KeyType || e.Fingerprint != k.Fingerprint {
			return false
		}
		return e.Hashed || slices.Contains(strings.Split(e.Hosts, ","), k.Host)
	}
	for i := range view.InForce {
		if view.KnownHosts.ReportedAt == nil {
			break
		}
		present := slices.ContainsFunc(view.KnownHosts.Entries, func(e knownHostsFileRow) bool { return holds(e, view.InForce[i]) })
		view.InForce[i].PresentInFile = &present
	}
	for i := range view.KnownHosts.Entries {
		e := &view.KnownHosts.Entries[i]
		e.ApprovedHere = slices.ContainsFunc(view.InForce, func(k ledgerRow) bool { return holds(*e, k) })
	}
	httpx.JSON(w, http.StatusOK, view)
}

// HandleHostKeyBatch returns the ledger rows of one batch — what a change-log
// summary row expands into. GET /api/v1/host-key-batches/{batchId}
func (s *Service) HandleHostKeyBatch(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), ledgerSelect+` WHERE batch_id = ? ORDER BY id`, r.PathValue("batchId"))
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	out, err := scanLedgerRows(rows)
	if err != nil {
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
		return
	}
	if len(out) == 0 {
		httpx.Fail(w, http.StatusNotFound, "not_found", "no such host-key batch")
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

type coverageRunner struct {
	RunnerID   string `json:"runnerId"`
	Name       string `json:"name"`
	Registered bool   `json:"registered"`
	// States is one entry per scope host, in the hosts' order: one of the
	// hostkeys.State* values. Missing counts the not-trusted ones.
	States  []string `json:"states"`
	Missing int      `json:"missing"`
	// ReportedAt is when this runner last reported its file; null = never, in
	// which case "in-file" cannot appear and States reflects approvals only.
	ReportedAt *string `json:"reportedAt"`
}

type scopeCoverage struct {
	Scope   string               `json:"scope"`
	Hosts   []hostkeys.ScopeHost `json:"hosts"`
	Runners []coverageRunner     `json:"runners"`
}

// HandleScopeHostKeyCoverage reports which of a scope's hosts each of its BOUND
// runners trusts. A bound runner that does not trust a host fails that host's
// runs at the first connection, and a host added to the scope last week is
// exactly how that happens.
// GET /api/v1/scopes/{scopeId}/host-key-coverage
func (s *Service) HandleScopeHostKeyCoverage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	scopeID := r.PathValue("scopeId")
	var cov scopeCoverage
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM scopes WHERE id = ?`, scopeID).Scan(&cov.Scope); err != nil {
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	if id, ok := auth.IdentityFrom(ctx); !ok || !auth.ScopeReadable(id, cov.Scope) {
		// The same answer as a scope that does not exist: host names are the
		// scope's contents.
		httpx.Fail(w, http.StatusNotFound, "not_found", "scope not found")
		return
	}
	fail := func(err error) {
		s.log.Error("scope host-key coverage", "scope", cov.Scope, "error", err)
		httpx.Fail(w, http.StatusInternalServerError, "internal", "db error")
	}
	hosts, err := hostkeys.PlanScope(ctx, s.db, cov.Scope)
	if err != nil {
		fail(err)
		return
	}
	cov.Hosts = hosts
	bound, err := execspec.BoundRunners(ctx, s.db, scopeID)
	if err != nil {
		fail(err)
		return
	}
	cov.Runners = make([]coverageRunner, 0, len(bound))
	for _, b := range bound {
		trusted, err := hostkeys.LoadTrusted(ctx, s.db, b.RunnerID)
		if err != nil {
			fail(err)
			return
		}
		cr := coverageRunner{RunnerID: b.RunnerID, Name: b.Name, Registered: b.Registered, States: make([]string, len(hosts))}
		var reported sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT known_hosts_reported_at FROM runners WHERE id = ?`, b.RunnerID).Scan(&reported)
		if reported.Valid {
			cr.ReportedAt = &reported.String
		}
		for i, h := range hosts {
			cr.States[i] = trusted.State(h)
			if cr.States[i] == hostkeys.StateNone {
				cr.Missing++
			}
		}
		cov.Runners = append(cov.Runners, cr)
	}
	httpx.JSON(w, http.StatusOK, cov)
}
