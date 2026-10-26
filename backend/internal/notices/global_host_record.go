package notices

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalHostRecord — a host that an agency's scope reaches has a host
// record that Global wrote by hand, and none of the agency's own (2.4.0, §0a,
// IN-6; the owner's answer to question 33). Until §0a a record Global wrote by
// hand answered to every scope, and won over the agency's own (LR-70). Now it
// is Global's runs' only: the agency's runs on that host find no record, and
// fail on that host ("no SSH host record").
//
// What is reported is the host as it stands: a scope of the agency lists it,
// Global has a hand-written record of that hostname, and neither a record
// imported for that scope nor one the agency wrote by hand has it.
//
// Subject: `<agency id>:<hostname>`. Filed under the agency. It clears when
// the agency has a record for the host, or no scope of its lists it.
const KindGlobalHostRecord = "global_host_record"

func checkGlobalHostRecord(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT sa.agency_id, sh.host, sc.name
		  FROM scope_hosts sh
		  JOIN scopes sc ON sc.id = sh.scope_id
		  JOIN scope_agencies sa ON sa.scope_id = sc.id
		 WHERE sa.agency_id <> ?1
		   AND EXISTS (SELECT 1 FROM ssh_hosts g
		                WHERE g.hostname = sh.host AND g.scope_id IS NULL AND g.owner_agency = ?1)
		   AND NOT EXISTS (SELECT 1 FROM ssh_hosts o
		                    WHERE o.hostname = sh.host
		                      AND (o.scope_id = sc.id OR (o.scope_id IS NULL AND o.owner_agency = sa.agency_id)))
		 ORDER BY sa.agency_id, sh.host, sc.name`, agencyid.Global)
	if err != nil {
		return err
	}
	type key struct{ agency, host string }
	var order []key
	scopes := map[key][]string{}
	for rows.Next() {
		var k key
		var scope string
		if err := rows.Scan(&k.agency, &k.host, &scope); err != nil {
			rows.Close()
			return err
		}
		if _, seen := scopes[k]; !seen {
			order = append(order, k)
		}
		scopes[k] = append(scopes[k], scope)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	central := agencyid.CurrentGlobalName(ctx, database) // the built-in agency's name now (2.5.2 GR)
	var found []Finding
	for _, k := range order {
		noun := "scope"
		if len(scopes[k]) > 1 {
			noun = "scopes"
		}
		found = append(found, Finding{
			AgencyID: k.agency,
			Subject:  k.agency + ":" + k.host,
			Detail: fmt.Sprintf("The host %s, which this agency's %s %s reach, has a host record only %s has. %s runs "+
				"nothing since 2.5.3, so no run uses its records: this agency's runs on %s find no record, and fail on that host. "+
				"Give this agency a record for %s (by hand, or imported with the scope's inventory). This notice clears "+
				"when it is done.",
				k.host, noun, strings.Join(scopes[k], ", "), central, central, k.host, k.host),
		})
	}
	return Reconcile(ctx, database, KindGlobalHostRecord, found)
}
