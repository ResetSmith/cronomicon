package hostkeys

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/notices"
)

// LocalRunnerMissingKeys finds, for the inbox, the scopes whose runs the local
// runner takes and whose hosts it has no approved key for
// (notices.KindLocalRunnerHostKeys). It is the standing answer to what the
// upgrade to 2.3.0 changed for the server's own connections: a host the server
// had never connected to used to be captured on its first connection, and is
// now refused until its key is approved.
//
// Only while the local runner is ON: off, it takes no run and nothing fails for
// want of a key. Only scopes it would be handed: its agency's, unbound or bound
// to it. Only hosts the server would dial: one with no host record is reached
// by an agent's inventory name and never by the server.
func LocalRunnerMissingKeys(ctx context.Context, database *sql.DB) ([]notices.Finding, error) {
	var localID, status string
	err := database.QueryRowContext(ctx, `SELECT id, COALESCE(status, '') FROM runners WHERE kind = 'server'`).Scan(&localID, &status)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && status != "online") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := database.QueryContext(ctx, `
		SELECT sc.id, sc.name,
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM scope_agencies sa WHERE sa.scope_id = sc.id), ?)
		  FROM scopes sc
		 WHERE EXISTS (SELECT 1 FROM scope_agencies sa JOIN runner_agencies ra ON ra.agency_id = sa.agency_id
		                WHERE sa.scope_id = sc.id AND ra.runner_id = ?)
		   AND (NOT EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id)
		        OR EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id AND sr.runner_id = ?))
		 ORDER BY sc.name`, agencyid.Global, localID, localID)
	if err != nil {
		return nil, err
	}
	type scope struct{ id, name, agency string }
	var scopes []scope
	for rows.Next() {
		var s scope
		if err := rows.Scan(&s.id, &s.name, &s.agency); err != nil {
			rows.Close()
			return nil, err
		}
		scopes = append(scopes, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	trusted, err := LoadTrusted(ctx, database, localID)
	if err != nil {
		return nil, err
	}
	var found []notices.Finding
	for _, s := range scopes {
		plan, err := PlanScopeFor(ctx, database, s.name, true)
		if err != nil {
			return nil, err
		}
		var missing []string
		dialled := 0
		for _, h := range plan {
			if h.Target == "" && h.Via == "" {
				continue // no host record: the server never dials it
			}
			dialled++
			if trusted.State(h) == StateNone {
				missing = append(missing, h.Host)
			}
		}
		if len(missing) == 0 {
			continue
		}
		names := missing
		more := ""
		if len(names) > 5 {
			more = fmt.Sprintf(", and %d more", len(names)-5)
			names = names[:5]
		}
		found = append(found, notices.Finding{
			AgencyID: s.agency,
			Subject:  s.id,
			Detail: fmt.Sprintf("The local runner takes the runs of the scope %s and has no approved host key for %d of the %d "+
				"hosts it would connect to there (%s%s). A run it takes fails for those hosts (host_key_unverified). Until "+
				"2.3.0 the server captured a host's key the first time it connected; it now connects only to a host whose "+
				"key has been approved. A global administrator scans the scope and approves the keys under Runners → Local "+
				"runner → Host keys; a host reached through a bastion cannot be scanned, and its key is pasted there. Or "+
				"bind the scope to an agent, which has trusted keys of its own.",
				s.name, len(missing), dialled, strings.Join(names, ", "), more),
		})
	}
	return found, nil
}
