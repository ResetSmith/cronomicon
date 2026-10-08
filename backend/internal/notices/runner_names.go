package notices

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// KindRunnerNameShared — two or more runner agents of ONE agency are registered
// under the same name and are both alive (2.3.2). A name is self-declared and
// not unique, and nothing that decides anything reads it: claims, bindings,
// keys and kills go by the runner's id. But it is what History, Activity and the
// host-key ledger SHOW, so two such agents cannot be told apart there, and the
// offer to restore a lost runner's scope bindings is made by name, so it would
// be made to the other. Subject: "<agency id>:<name>". Filed under the agency
// that owns the runners.
const KindRunnerNameShared = "runner_name_shared"

// checkRunnerNameShared reports each name that two live agents of one agency
// both carry.
//
// "Both alive" is what keeps a re-enrolment out of it. An agent that loses its
// identity registers again under the same name with a new id, and its old row
// stays (and stays `online` until the reaper notices) beside the new one: that
// is one agent, and Restore placement is the tool for it. Two rows are two
// agents only when each was seen after the other registered, which a row whose
// process is gone never is. Agents of different agencies are not compared: a
// name is each agency's own business, and a notice must not tell one agency
// what another calls its runners.
func checkRunnerNameShared(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT a.owner_agency, COALESCE(ag.name, a.owner_agency), a.name, a.id, COALESCE(a.last_client_ip, '')
		  FROM runners a LEFT JOIN agencies ag ON ag.id = a.owner_agency
		 WHERE a.kind <> 'server' AND a.status <> 'offline' AND a.last_seen_at IS NOT NULL
		   AND EXISTS (SELECT 1 FROM runners b
		                WHERE b.id <> a.id AND b.kind <> 'server' AND b.status <> 'offline'
		                  AND b.owner_agency = a.owner_agency AND b.name = a.name
		                  AND b.last_seen_at IS NOT NULL
		                  AND b.last_seen_at >= a.registered_at
		                  AND a.last_seen_at >= b.registered_at)
		 ORDER BY a.owner_agency, a.name, a.registered_at, a.id`)
	if err != nil {
		return err
	}
	type group struct {
		agencyID, agencyName, name string
		members                    []string
	}
	var groups []*group
	for rows.Next() {
		var agencyID, agencyName, name, id, ip string
		if err := rows.Scan(&agencyID, &agencyName, &name, &id, &ip); err != nil {
			rows.Close()
			return err
		}
		if len(groups) == 0 || groups[len(groups)-1].agencyID != agencyID || groups[len(groups)-1].name != name {
			groups = append(groups, &group{agencyID: agencyID, agencyName: agencyName, name: name})
		}
		member := "id " + id
		if ip != "" {
			member += ", last seen from " + ip
		}
		g := groups[len(groups)-1]
		g.members = append(g.members, member)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	found := make([]Finding, 0, len(groups))
	for _, g := range groups {
		found = append(found, Finding{
			AgencyID: g.agencyID,
			Subject:  g.agencyID + ":" + g.name,
			Detail: fmt.Sprintf("%d runner agents owned by %s are registered under the name %s and are all polling (%s). "+
				"Each runs its jobs correctly: a runner is identified by its id, never by its name. But History, Activity "+
				"and the host-key ledger show the name, so the agents cannot be told apart there; Activity's runner filter "+
				"shows them together; and if one of them is deregistered or lost, the offer to restore its scope bindings "+
				"is made to the other, because that offer goes by name. Give all but one a name of its own: set "+
				"CRONOMICON_RUNNER_NAME (or -name) in that agent's configuration and restart it. It keeps its identity, "+
				"its keys and its bindings; its row is renamed when it next polls, and this notice clears. If these are "+
				"one machine copied (a cloned VM, two containers on one volume), see the install guide on copied "+
				"identity files first: such copies usually show as ONE runner, not two, and that is a different and "+
				"worse problem.",
				len(g.members), g.agencyName, g.name, strings.Join(g.members, "; ")),
		})
	}
	return Reconcile(ctx, database, KindRunnerNameShared, found)
}
