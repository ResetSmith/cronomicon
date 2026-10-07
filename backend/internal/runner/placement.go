package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
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
	var owner string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(tags,'[]'), COALESCE(capabilities,'[]'), last_client_ip, owner_agency
		   FROM runners WHERE id = ?`, runnerID,
	).Scan(&tags, &caps, &lastIP, &owner); err != nil {
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
			 deregistered_at, deregistered_by, deregistered_via, owner_agency)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runnerID, name, string(agencyJSON), tags.String, caps.String,
		nullStrOrNil(lastIP.String), at, actor, via, owner,
	); err != nil {
		return fmt.Errorf("insert placement history: %w", err)
	}
	return nil
}

// agencyIDsFor reads the agencies a runner SERVED, other than Global. Returns a
// non-nil empty slice for a runner that served Global only, so the stored JSON
// is "[]" rather than "null" — the column is NOT NULL and a reader should never
// have to handle both spellings of empty.
//
// The list is history since 2.3.0: nothing restores it (MA-32). It is kept
// because it says what a deregistered runner was, which the Runners view and an
// operator reading the table still want to know. Global stays left out, as it
// always was, so old and new rows read alike.
func agencyIDsFor(ctx context.Context, tx *sql.Tx, runnerID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT agency_id FROM runner_agencies WHERE runner_id = ? AND agency_id <> ? ORDER BY agency_id`,
		runnerID, agencyid.Global)
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

// placementSuggestion is the offer shown against a re-enrolled runner (DR-7 (c)).
//
// What it restores changed in 2.3.0 (MA-32). An agent's placement is its owner,
// and its owner is its registration token's: a re-enrolled agent already serves
// the right agency, or it was enrolled for the wrong one and no restore may fix
// that by adding a serve row. What a new id still lacks is the SCOPE BINDINGS
// the old id holds (a binding outlives its runner, and keeps its scope closed),
// and the tags. So the offer is: re-point, at this runner, the bindings the
// previous id still holds on scopes of THIS runner's own agency.
//
// DR-Q2 still holds: it is a SUGGESTION AN OPERATOR CONFIRMS, never an automatic
// re-bind. The runner `name` it is looked up by is SELF-DECLARED at
// registration. What makes that safe now is what the restore can reach: only
// scopes of the agency that owns this runner, which that agency's administrator
// could bind to it by hand (LR-62). Claiming another agency's runner name gets
// an agent nothing of that agency's.
type placementSuggestion struct {
	HistoryID        int64  `json:"historyId"`
	PreviousRunnerID string `json:"previousRunnerId"`
	// Agencies is the ONE agency the restore is for: this runner's owner, whose
	// scopes are the ones re-pointed. (A list since DR-7, when a restore put a
	// runner back into every agency it had been in. Nothing does that now.)
	Agencies []agencyRef `json:"agencies"`
	Tags     []string    `json:"tags"`
	// Scopes are the names of the scopes of this runner's agency still bound to
	// the PREVIOUS runner id (SB-1). They are closed until the bindings are
	// re-pointed — accepting does that. Never empty: with none there is no offer.
	Scopes          []string `json:"scopes"`
	DeregisteredAt  string   `json:"deregisteredAt"`
	DeregisteredVia string   `json:"deregisteredVia"`
	// DR-Q7: the observed client IP is the ONE signal the agent cannot freely
	// assert, so it carries the security weight. A mismatch does NOT suppress the
	// offer — a host legitimately rebuilt during recovery lands on a new lease —
	// but it is surfaced so the weaker match is visible rather than implied.
	PreviousClientIP *string `json:"previousClientIp"`
	CurrentClientIP  *string `json:"currentClientIp"`
	ClientIPMatches  bool    `json:"clientIpMatches"`
}

// restorableBindingsSQL selects the bindings a restore may re-point from a
// previous runner id (?1) to a live runner (?2): those on a scope of the live
// runner's OWNER, which the runner in fact serves. Both halves matter. The
// first is the limit MA-32 sets (an agency's bindings go to that agency's
// agent and nobody else's). The second keeps a binding from being moved to a
// runner that could not claim the scope's runs: a legacy placement can be
// owned by Global and not serve it.
//
// It is the one statement of the rule, shared by the offer and the accept, so
// the two cannot disagree about what "restorable" means.
const restorableBindingsSQL = `
	SELECT sr.scope_id, sc.name
	  FROM scope_runners sr
	  JOIN scopes sc ON sc.id = sr.scope_id
	  JOIN runners rn ON rn.id = ?2
	 WHERE sr.runner_id = ?1
	   AND EXISTS (SELECT 1 FROM scope_agencies sa
	                WHERE sa.scope_id = sr.scope_id AND sa.agency_id = rn.owner_agency)
	   AND EXISTS (SELECT 1 FROM runner_agencies ra
	                WHERE ra.runner_id = rn.id AND ra.agency_id = rn.owner_agency)
	 ORDER BY sc.name`

// suggestionsFor resolves a placement offer for each live runner that has one,
// keyed by runner id. Structured as grouped queries rather than a per-row
// lookup for the same reason HandleListRunners attaches agencies that way: a
// nested cursor on a shared SQLite connection deadlocks.
//
// A runner gets an offer when a snapshot that has not been dismissed carries its
// name, the snapshot's runner id is gone, and that id still holds a binding on
// a scope of this runner's own agency (restorableBindingsSQL, evaluated here in
// memory). The newest such snapshot wins.
func suggestionsFor(ctx context.Context, q queryer) (map[string]*placementSuggestion, error) {
	// Bindings held by a runner id that has no row, by that id and by each
	// agency of the bound scope.
	type orphanKey struct{ runnerID, agencyID string }
	orphan := map[orphanKey][]string{}
	orphanIDs := map[string]bool{}
	rows, err := q.QueryContext(ctx, `
		SELECT sr.runner_id, sa.agency_id, sc.name
		  FROM scope_runners sr
		  JOIN scopes sc ON sc.id = sr.scope_id
		  JOIN scope_agencies sa ON sa.scope_id = sr.scope_id
		 WHERE NOT EXISTS (SELECT 1 FROM runners rn WHERE rn.id = sr.runner_id)
		 ORDER BY sc.name`)
	if err != nil {
		return nil, fmt.Errorf("read orphaned bindings: %w", err)
	}
	for rows.Next() {
		var k orphanKey
		var scope string
		if err := rows.Scan(&k.runnerID, &k.agencyID, &scope); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read orphaned bindings: %w", err)
		}
		orphan[k] = append(orphan[k], scope)
		orphanIDs[k.runnerID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read orphaned bindings: %w", err)
	}
	rows.Close()
	if len(orphanIDs) == 0 {
		return nil, nil // nothing anywhere is waiting to be re-pointed
	}

	// Snapshots that could be the source of an offer, newest first per name. id
	// is AUTOINCREMENT, so descending id is "latest" without relying on
	// timestamp formatting.
	type snap struct {
		s    placementSuggestion
		tags string
	}
	byName := map[string][]snap{}
	rows, err = q.QueryContext(ctx, `
		SELECT name, id, runner_id, tags, last_client_ip, deregistered_at, deregistered_via
		  FROM runner_placement_history
		 WHERE dismissed_at IS NULL
		 ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("read placement history: %w", err)
	}
	for rows.Next() {
		var name string
		var sn snap
		var prevIP sql.NullString
		if err := rows.Scan(&name, &sn.s.HistoryID, &sn.s.PreviousRunnerID, &sn.tags,
			&prevIP, &sn.s.DeregisteredAt, &sn.s.DeregisteredVia); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan placement history: %w", err)
		}
		if !orphanIDs[sn.s.PreviousRunnerID] {
			continue // its bindings are gone or were never there: nothing to restore
		}
		if prevIP.Valid && prevIP.String != "" {
			v := prevIP.String
			sn.s.PreviousClientIP = &v
		}
		byName[name] = append(byName[name], sn)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read placement history: %w", err)
	}
	rows.Close()
	if len(byName) == 0 {
		return nil, nil
	}

	// The live runners, each with its owner, whether it serves that owner, and
	// the address it was last seen at.
	out := map[string]*placementSuggestion{}
	rows, err = q.QueryContext(ctx, `
		SELECT rn.id, rn.name, rn.owner_agency, COALESCE(ag.name, ''), rn.last_client_ip,
		       EXISTS (SELECT 1 FROM runner_agencies ra
		                WHERE ra.runner_id = rn.id AND ra.agency_id = rn.owner_agency)
		  FROM runners rn LEFT JOIN agencies ag ON ag.id = rn.owner_agency`)
	if err != nil {
		return nil, fmt.Errorf("read runners: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, owner, ownerName string
		var ip sql.NullString
		var servesOwner bool
		if err := rows.Scan(&id, &name, &owner, &ownerName, &ip, &servesOwner); err != nil {
			return nil, fmt.Errorf("read runners: %w", err)
		}
		if !servesOwner {
			continue
		}
		for _, sn := range byName[name] {
			scopes := orphan[orphanKey{sn.s.PreviousRunnerID, owner}]
			if len(scopes) == 0 {
				continue
			}
			s := sn.s // copy: one snapshot may match several same-named runners
			s.Scopes = scopes
			s.Agencies = []agencyRef{{ID: owner, Name: ownerName}}
			s.Tags = []string{}
			_ = json.Unmarshal([]byte(sn.tags), &s.Tags)
			if ip.Valid && ip.String != "" {
				v := ip.String
				s.CurrentClientIP = &v
			}
			s.ClientIPMatches = s.PreviousClientIP != nil && s.CurrentClientIP != nil &&
				*s.PreviousClientIP == *s.CurrentClientIP
			out[id] = &s
			break
		}
	}
	return out, rows.Err()
}

// queryer is the read seam shared by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ErrPlacementTags is returned when the union of current and restored tags
// violates the tag rules (count cap, length). Mapped to 422: the operator must
// trim before restoring, since a silent truncation would drop tags unseen.
var ErrPlacementTags = errors.New("placement tags invalid")

// ErrPlacementGone is returned when the suggestion no longer applies — it was
// already accepted, the bindings were re-pointed by hand, or the snapshot aged
// out of the retention window. Mapped to 409: the operator's view is simply
// stale.
var ErrPlacementGone = errors.New("placement no longer applicable")

// ApplyPlacement accepts a suggestion: it re-points, at this runner, the scope
// bindings the previous runner id still holds on scopes of this runner's own
// agency (SB-1, MA-32), and merges the recorded tags.
//
// IT NEVER WRITES A SERVE ROW. Until 2.3.0 this is where a re-enrolled runner
// got its agencies back, every one the snapshot named. An agent serves exactly
// the agency that owns it now, set by its token; a restore that re-created an
// old list would make a new shared agent, which nothing may (MA-11). So a
// snapshot of a runner that served several agencies restores only what belongs
// to the agency of the runner it is applied to, and the bindings the previous
// id holds on other agencies' scopes stay where they are: those scopes stay
// closed until their own agency re-points them.
//
// Authority is the caller's over this runner (the route's owner gate). The
// owner is the agency whose scopes are touched, so the accept reaches nothing a
// hand-made binding by the same person could not (LR-62).
//
// DRF-1: the activity row is written INSIDE the transaction, so a restore
// without its audit row cannot exist. DRF-2 / DRF-Q3: restored tags are
// UNIONED with the runner's current ones (current first), normalised by the
// same rules the tag editor applies, so the result is one that editor could
// have produced.
func (s *Service) ApplyPlacement(ctx context.Context, runnerID string, historyID int64, actor string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	// Re-read inside the transaction: the list that produced the offer is a
	// snapshot of a moment, and two operators may be looking at the same screen.
	var curName, curTagsJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT name, COALESCE(tags,'[]') FROM runners WHERE id = ?`, runnerID).Scan(&curName, &curTagsJSON); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementGone
		}
		return err
	}

	var tagsJSON, snapName, prevRunnerID string
	var dismissed sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT tags, name, runner_id, dismissed_at FROM runner_placement_history WHERE id = ?`, historyID,
	).Scan(&tagsJSON, &snapName, &prevRunnerID, &dismissed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementGone
		}
		return err
	}
	if dismissed.Valid {
		return ErrPlacementGone
	}
	// The snapshot must belong to this runner's name. Without this an operator
	// could paste any historyId and take over the bindings of a runner this one
	// was never associated with — the offer would be honest and the endpoint
	// would not.
	if curName != snapName || prevRunnerID == "" || prevRunnerID == runnerID {
		return ErrPlacementGone
	}
	// A snapshot is of a runner that was deleted. Its id is never reused, so a
	// live row under it means the table was edited by hand; do not move the
	// bindings of a runner that is there.
	var prevLive int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runners WHERE id = ?`, prevRunnerID).Scan(&prevLive); err != nil {
		return err
	}
	if prevLive > 0 {
		return ErrPlacementGone
	}

	type binding struct{ scopeID, scopeName string }
	var restore []binding
	rows, err := tx.QueryContext(ctx, restorableBindingsSQL, prevRunnerID, runnerID)
	if err != nil {
		return fmt.Errorf("read scope bindings: %w", err)
	}
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.scopeID, &b.scopeName); err != nil {
			rows.Close()
			return fmt.Errorf("read scope bindings: %w", err)
		}
		restore = append(restore, b)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read scope bindings: %w", err)
	}
	rows.Close()
	if len(restore) == 0 {
		return ErrPlacementGone // nothing of this runner's agency is left to restore
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

	// The bindings, scope by scope. OR IGNORE then delete, as ReplaceScopeRunner
	// does: a scope an operator has already bound to this runner by hand would
	// collide on the primary key, and the old row is then simply dropped.
	ts := now()
	scopeNames := make([]string, 0, len(restore))
	for _, b := range restore {
		if _, err := tx.ExecContext(ctx, `
			UPDATE OR IGNORE scope_runners
			   SET runner_id = ?, runner_name = ?, bound_by = ?, bound_at = ?
			 WHERE runner_id = ? AND scope_id = ?`, runnerID, curName, actor, ts, prevRunnerID, b.scopeID); err != nil {
			return fmt.Errorf("restore scope bindings: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM scope_runners WHERE runner_id = ? AND scope_id = ?`, prevRunnerID, b.scopeID); err != nil {
			return fmt.Errorf("restore scope bindings: %w", err)
		}
		scopeNames = append(scopeNames, b.scopeName)
	}

	summary := "placement restored: scopes " + strings.Join(scopeNames, ", ") +
		" re-bound from this runner's previous enrolment (tags: " + strings.Join(merged, ", ") + ")"
	if err := auditlog.WriteActivity(ctx, tx, auditlog.ActivityParams{
		At:         ts,
		Kind:       "config",
		Actor:      actor,
		Target:     "runner:" + curName,
		RunnerName: curName,
		Summary:    summary,
	}); err != nil {
		return fmt.Errorf("record placement: %w", err)
	}
	return tx.Commit()
}

// ErrPlacementShared is returned when a dismissal would withdraw an offer that
// is another agency's as well. Mapped 409 `placement_shared`.
var ErrPlacementShared = errors.New("the snapshot also holds bindings that are not this runner's agency's")

// DismissPlacement records that a snapshot is not to be restored (DRF-3,
// DRF-Q4). The mark goes on the snapshot, so it is withdrawn from every runner
// it could have been offered to; the row itself is kept as history.
//
// Because the mark is on the snapshot, who may set it is narrower than "anyone
// with a runner of that name" — the name is the agent's own word, and since
// 2.3.0 any agency can enrol an agent under any name. A dismissal is accepted
// only for a snapshot that is on offer to THIS runner (something of its own
// agency's is restorable, the accept's own test), and only when that is all
// the snapshot still holds. One that also holds another agency's bindings is
// that agency's offer too: dismissing it here would close their one-click way
// back, so it is refused (ErrPlacementShared), and this agency settles its own
// scopes by accepting or by re-binding them, after which the offer is gone for
// it anyway.
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
	var prevRunnerID string
	if err := tx.QueryRowContext(ctx,
		`SELECT runner_id FROM runner_placement_history WHERE id = ? AND name = ? AND dismissed_at IS NULL`,
		historyID, curName).Scan(&prevRunnerID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPlacementGone
		}
		return err
	}
	var restorable, held int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (`+restorableBindingsSQL+`)`, prevRunnerID, runnerID).Scan(&restorable); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scope_runners WHERE runner_id = ?`, prevRunnerID).Scan(&held); err != nil {
		return err
	}
	if restorable == 0 || prevRunnerID == runnerID {
		return ErrPlacementGone // not on offer to this runner: not its to dismiss
	}
	if held > restorable {
		return ErrPlacementShared
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
