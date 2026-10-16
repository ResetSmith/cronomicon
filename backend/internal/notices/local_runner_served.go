package notices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// KindLocalRunnerServed — the local runner served an agency until 2.4.0, and
// serves Global only now (§0a, IN-3). Migration 1380 narrowed its list and
// recorded which agencies were on it (LocalRunnerServedKey). That agency's
// shell jobs wait for an agent of its own: this tells its administrators so.
//
// Subject: the agency's id. Filed under the agency. It clears when an agent
// that agency owns serves it, or the agency is gone.
const KindLocalRunnerServed = "local_runner_served"

// LocalRunnerServedKey is the settings row migration 1380 wrote: the agencies
// the local runner served, as a JSON list of ids.
const LocalRunnerServedKey = "upgrade.2.4.0.localRunnerServed"

func checkLocalRunnerServed(ctx context.Context, database *sql.DB) error {
	var raw string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, LocalRunnerServedKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Reconcile(ctx, database, KindLocalRunnerServed, nil)
	}
	if err != nil {
		return err
	}
	var served []string
	if err := json.Unmarshal([]byte(raw), &served); err != nil {
		return fmt.Errorf("read the record of what the local runner served: %w", err)
	}
	var found []Finding
	for _, agency := range served {
		var name string
		var hasAgent bool
		err := database.QueryRowContext(ctx, `
			SELECT a.name, EXISTS (SELECT 1 FROM runners r JOIN runner_agencies ra ON ra.runner_id = r.id
			                        WHERE r.kind = 'agent' AND r.owner_agency = a.id AND ra.agency_id = a.id)
			  FROM agencies a WHERE a.id = ?`, agency).Scan(&name, &hasAgent)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && hasAgent) {
			continue
		}
		if err != nil {
			return err
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  agency,
			Detail: fmt.Sprintf("Until 2.4.0 the local runner served %s, so the server ran its shell jobs. The local runner "+
				"is Global's and serves Global only now: %s's shell jobs wait for an agent of its own. Enrol one for %s "+
				"(Runners → Add runner). This notice clears when one serves it.", name, name, name),
		})
	}
	return Reconcile(ctx, database, KindLocalRunnerServed, found)
}
