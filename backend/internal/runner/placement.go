package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/auditlog"
	"github.com/ResetSmith/cronomicon/internal/tagutil"
)

// Deregistration reasons recorded on a placement snapshot. The distinction
// matters to whoever reads the history: an operator deregistration is a
// deliberate act, while a reaper sweep means the runner simply stayed offline
// past CRONOMICON_RUNNER_DEREGISTER_AFTER — which during an outage is most of them.
const (
	deregisterViaOperator = "operator"
	deregisterViaReaper   = "reaper"
)

// capturePlacement snapshots a runner's operator-owned placement into
// runner_placement_history (DR-7 / DR-Q6), inside the transaction that is about
// to delete the runner row.
//
// ORDER IS LOAD-BEARING. `runner_agencies` carries ON DELETE CASCADE on
// runner_id (migration 440) and foreign keys are enforced, so the membership
// rows are destroyed by the DELETE. Capturing after it would record an empty
// agency set every single time — and the resulting history would read as "this
// runner had no placement", which is indistinguishable from the truth for a
// runner that genuinely had none. Call this BEFORE the delete, always.
//
// Being in the same transaction is the other half: a crash between capture and
// delete must not be able to destroy placement without recording it.
func capturePlacement(ctx context.Context, tx *sql.Tx, runnerID, name, actor, via, at string) error {
	var tags, caps, lastIP sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(tags,'[]'), COALESCE(capabilities,'[]'), last_client_ip
		   FROM runners WHERE id = ?`, runnerID,
	).Scan(&tags, &caps, &lastIP); err != nil {
		return fmt.Errorf("read runner placement: %w", err)
	}

	agencies, err := agencyIDsFor(ctx, tx, runnerID)
	if err != nil {
		return err
	}
	agencyJSON, err := json.Marshal(agencies)
	if err != nil {
		return fmt.Errorf("encode agency ids: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO runner_placement_history
			(runner_id, name, agency_ids, tags, capabilities, last_client_ip,
			 deregistered_at, deregistered_by, deregistered_via)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runnerID, name, string(agencyJSON), tags.String, caps.String,
		nullStrOrNil(lastIP.String), at, actor, via,
	); err != nil {
		return fmt.Errorf("insert placement history: %w", err)
	}
	return nil
}

// agencyIDsFor reads a runner's agency membership. Returns a non-nil empty slice
// for a runner in no agency, so the stored JSON is "[]" rather than "null" — the
// column is NOT NULL and a reader should never have to handle both spellings of
// empty.
func agencyIDsFor(ctx context.Context, tx *sql.Tx, runnerID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id`, runnerID)
	if err != nil {
		return nil, fmt.Errorf("read runner agencies: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("read runner agencies: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read runner agencies: %w", err)
	}
	return out, nil
}

// placementSuggestion is the offer shown against an unbound runner (DR-7 (c)).
//
// DR-Q2: it is a SUGGESTION AN OPERATOR CONFIRMS, never an automatic re-bind.
// The runner `name` it is looked up by is SELF-DECLARED at registration, so
// healing placement on name alone would let any agent inherit another runner's
// agency by claiming its name — turning an enrollment credential into a
// placement credential. Every signal is carried on the wire, labelled by whether
// it is observed or self-declared, so the person clicking can see what the match
// actually rests on.
type placementSuggestion struct {
	HistoryID        int64       `json:"historyId"`
	PreviousRunnerID string      `json:"previousRunnerId"`
	Agencies         []agencyRef `json:"agencies"`
	Tags             []string    `json:"tags"`
	DeregisteredAt   string      `json:"deregisteredAt"`
	DeregisteredVia  string      `json:"deregisteredVia"`
	// DR-Q7: the observed client IP is the ONE signal the agent cannot freely
	// assert, so it carries the security weight. A mismatch does NOT suppress the
	// offer — a host legitimately rebuilt during recovery lands on a new lease —
	// but it is surfaced so the weaker match is visible rather than implied.
	PreviousClientIP *string `json:"previousClientIp"`
	CurrentClientIP  *string `json:"currentClientIp"`
	ClientIPMatches  bool    `json:"clientIpMatches"`
}

// suggestionsFor resolves a placement offer for each unbound runner, keyed by
// runner id. Structured as grouped queries rather than a per-row lookup for the
// same reason HandleListRunners attaches agencies that way: a nested cursor on a
// shared SQLite connection deadlocks.
//
// Only runners with NO agency membership get an offer — a placed runner needs no
// suggestion — and only history rows that actually recorded a placement are
// candidates ([] agency set means there is nothing to restore).
func suggestionsFor(ctx context.Context, q queryer, unbound map[string]string) (map[string]*placementSuggestion, error) {
	if len(unbound) == 0 {
		return nil, nil
	}

	agencyNames := map[string]string{}
	if rows, err := q.QueryContext(ctx, `SELECT id, name FROM agencies`); err == nil {
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err == nil {
				agencyNames[id] = name
			}
		}
		rows.Close()
	}

	currentIP := map[string]*string{}
	if rows, err := q.QueryContext(ctx, `SELECT id, last_client_ip FROM runners`); err == nil {
		for rows.Next() {
			var id string
			var ip sql.NullString
			if err := rows.Scan(&id, &ip); err == nil {
				if ip.Valid && ip.String != "" {
					v := ip.String
					currentIP[id] = &v
				} else {
					currentIP[id] = nil
				}
			}
		}
		rows.Close()
	}

	// Most recent placement-bearing snapshot per name. id is AUTOINCREMENT, so
	// MAX(id) is "latest" without relying on timestamp formatting.
	rows, err := q.QueryContext(ctx, `
		SELECT h.name, h.id, h.runner_id, h.agency_ids, h.tags, h.last_client_ip,
		       h.deregistered_at, h.deregistered_via
		  FROM runner_placement_history h
		  JOIN (SELECT name, MAX(id) AS mx
		          FROM runner_placement_history
		         WHERE agency_ids <> '[]' AND dismissed_at IS NULL
		         GROUP BY name) latest
		    ON latest.name = h.name AND latest.mx = h.id`)
	if err != nil {
		return nil, fmt.Errorf("read placement history: %w", err)
	}
	defer rows.Close()

	byName := map[string]*placementSuggestion{}
	for rows.Next() {
		var name, agencyJSON, tagsJSON string
		var s placementSuggestion
		var prevIP sql.NullString
		if err := rows.Scan(&name, &s.HistoryID, &s.PreviousRunnerID, &agencyJSON, &tagsJSON,
			&prevIP, &s.DeregisteredAt, &s.DeregisteredVia); err != nil {
			return nil, fmt.Errorf("scan placement history: %w", err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(agencyJSON), &ids); err != nil {
			continue // a snapshot we cannot read is not a snapshot we should offer
		}
		for _, id := range ids {
			if n, ok := agencyNames[id]; ok {
				s.Agencies = append(s.Agencies, agencyRef{ID: id, Name: n})
			}
			// An agency deleted since the snapshot is deliberately dropped rather
			// than offered: applying it would fail the FK, and naming a department
			// that no longer exists helps nobody decide.
		}
		if len(s.Agencies) == 0 {
			continue
		}
		s.Tags = []string{}
		_ = json.Unmarshal([]byte(tagsJSON), &s.Tags)
		if prevIP.Valid && prevIP.String != "" {
			v := prevIP.String
			s.PreviousClientIP = &v
		}
		byName[name] = &s
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read placement history: %w", err)
	}

	out := map[string]*placementSuggestion{}
	for runnerID, name := range unbound {
		cand, ok := byName[name]
		if !ok {
			continue
		}
		s := *cand // copy: one snapshot may match several same-named runners
		s.CurrentClientIP = currentIP[runnerID]
		s.ClientIPMatches = s.PreviousClientIP != nil && s.CurrentClientIP != nil &&
			*s.PreviousClientIP == *s.CurrentClientIP
		out[runnerID] = &s
	}
	return out, nil
}

// queryer is the read seam shared by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ErrPlacementForbidden is returned when the caller may not grant one of the
// agencies the suggestion would apply. Mapped to 403 by the API layer.
var ErrPlacementForbidden = errors.New("placement forbidden")

// ErrPlacementTags is returned when the union of current and restored tags
// violates the tag rules (count cap, length). Mapped to 422: the operator must
// trim before restoring, since a silent truncation would drop tags unseen.
var ErrPlacementTags = errors.New("placement tags invalid")

// ErrPlacementGone is returned when the suggestion no longer applies — it was
// already accepted, the runner has since been placed, or the snapshot aged out
// of the retention window. Mapped to 409: the operator's view is simply stale.
var ErrPlacementGone = errors.New("placement no longer applicable")

// ApplyPlacement accepts a suggestion: it restores the recorded agency
// membership and tags onto a currently-unbound runner.
//
// DR-Q7 authorisation — `permits` reports whether the caller may grant a given
// agency, and EVERY agency in the snapshot must pass. Accepting must never be a
// cheaper route to a placement than making it by hand, or the one-click becomes
// a privilege-amplification path.
//
// The write goes through the same columns the manual placement path uses, in one
// transaction, so the two cannot drift into different results.
//
// DRF-1: the activity row is written INSIDE the transaction, so a placement
// without its audit row cannot exist. DRF-2 / DRF-Q3: restored tags are
// UNIONED with the runner's current ones (current first), normalised by the
// same rules the tag editor applies, so the result is one that editor could
// have produced.
func (s *Service) ApplyPlacement(ctx context.Context, runnerID string, historyID int64, actor string, permits func(agencyID string) bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// The runner must still exist and still be unbound. Re-checked inside the
	// transaction because the list that produced the offer is a snapshot of a
	// moment, and two operators may be looking at the same screen.
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runners WHERE id = ?`, runnerID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrPlacementGone
	}
	var placed int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runner_agencies WHERE runner_id = ?`, runnerID).Scan(&placed); err != nil {
		return err
	}
	if placed > 0 {
		return ErrPlacementGone
	}

	var agencyJSON, tagsJSON, snapName string
	var dismissed sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT agency_ids, tags, name, dismissed_at FROM runner_placement_history WHERE id = ?`, historyID,
	).Scan(&agencyJSON, &tagsJSON, &snapName, &dismissed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementGone
		}
		return err
	}
	if dismissed.Valid {
		return ErrPlacementGone
	}
	// The snapshot must belong to this runner's name. Without this an operator
	// could paste any historyId and place a runner into an agency it was never
	// associated with — the offer would be honest and the endpoint would not.
	var curName, curTagsJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT name, COALESCE(tags,'[]') FROM runners WHERE id = ?`, runnerID).Scan(&curName, &curTagsJSON); err != nil {
		return err
	}
	if curName != snapName {
		return ErrPlacementGone
	}

	var ids []string
	if err := json.Unmarshal([]byte(agencyJSON), &ids); err != nil {
		return fmt.Errorf("decode snapshot agencies: %w", err)
	}
	if len(ids) == 0 {
		return ErrPlacementGone
	}
	for _, id := range ids {
		if !permits(id) {
			return ErrPlacementForbidden
		}
	}

	for _, id := range ids {
		// A snapshot may name an agency deleted since; the FK refuses it and the
		// whole accept rolls back rather than applying a partial placement.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO runner_agencies (runner_id, agency_id) VALUES (?, ?)`, runnerID, id); err != nil {
			return fmt.Errorf("restore agency %s: %w", id, err)
		}
	}
	// DRF-Q3: current ∪ restored, current first. Normalize de-dupes
	// case-insensitively (first casing wins) and enforces the caps; a violation
	// is the operator's to resolve, not ours to truncate silently.
	merged, err := tagutil.Normalize(append(tagutil.Parse(curTagsJSON), tagutil.Parse(tagsJSON)...))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPlacementTags, err)
	}
	mergedJSON, _ := json.Marshal(merged)
	if _, err := tx.ExecContext(ctx,
		`UPDATE runners SET tags = ? WHERE id = ?`, string(mergedJSON), runnerID); err != nil {
		return fmt.Errorf("restore tags: %w", err)
	}

	names := make([]string, 0, len(ids))
	for _, id := range ids {
		var n string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM agencies WHERE id = ?`, id).Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		At:         now(),
		Kind:       "config",
		Actor:      actor,
		Target:     "runner:" + curName,
		RunnerName: curName,
		Summary:    "placement restored: " + strings.Join(names, ", ") + " (tags: " + strings.Join(merged, ", ") + ")",
	}); err != nil {
		return fmt.Errorf("record placement: %w", err)
	}
	return tx.Commit()
}

// DismissPlacement records that a snapshot is not to be restored (DRF-3,
// DRF-Q4). The mark goes on the snapshot, so it is withdrawn from every runner
// it could have been offered to; the row itself is kept as history.
func (s *Service) DismissPlacement(ctx context.Context, runnerID string, historyID int64, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var curName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM runners WHERE id = ?`, runnerID).Scan(&curName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementGone
		}
		return err
	}
	ts := now()
	// Guarded on dismissed_at IS NULL so a double-click is a clean 409, not a
	// second audit row.
	res, err := tx.ExecContext(ctx,
		`UPDATE runner_placement_history SET dismissed_at = ?, dismissed_by = ?
		  WHERE id = ? AND name = ? AND dismissed_at IS NULL`, ts, actor, historyID, curName)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPlacementGone
	}
	if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      actor,
		Target:     "runner:" + curName,
		RunnerName: curName,
		Summary:    "placement suggestion dismissed",
	}); err != nil {
		return fmt.Errorf("record dismissal: %w", err)
	}
	return tx.Commit()
}
