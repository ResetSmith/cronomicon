package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindReactionOtherAgency — a reaction watches a definition of another agency
// than its own (2.4.0, §0a, IN-8). A reaction watches its own agency's
// definitions only; one that watches another's does not fire, and each
// delivery it is refused says why (§0a, open point 1). It was written before
// §0a, or a scope has since been given to another agency.
//
// The reaction's upstream is the definition it is pinned to (on_uid). One
// that pins none is matched by name, in its own agency, and so cannot reach
// another's.
//
// Subject: `<owner uid>:<reaction name>`. Filed under the owner's agency. It
// clears when the reaction is removed or watches its own agency's definition.
const KindReactionOtherAgency = "reaction_other_agency"

func checkReactionOtherAgency(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT r.owner_uid, r.owner_kind, r.owner_name, r.name, r.on_uid, r.on_kind, r.on_name
		  FROM reactions r
		 WHERE COALESCE(r.owner_uid, '') <> '' AND COALESCE(r.on_uid, '') <> ''`)
	if err != nil {
		return err
	}
	type pair struct{ ownerUID, ownerKind, ownerName, name, onUID, onKind, onName string }
	var all []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ownerUID, &p.ownerKind, &p.ownerName, &p.name, &p.onUID, &p.onKind, &p.onName); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	agencyOf := func(uid, kind string) (sql.NullString, error) {
		var a sql.NullString
		err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, uid, kind).Scan(&a)
		return a, err
	}
	var found []Finding
	for _, p := range all {
		owner, err := agencyOf(p.ownerUID, p.ownerKind)
		if err != nil {
			return err
		}
		upstream, err := agencyOf(p.onUID, p.onKind)
		if err != nil {
			return err
		}
		if !owner.Valid || !upstream.Valid || owner.String == upstream.String {
			continue
		}
		found = append(found, Finding{
			AgencyID: owner.String,
			Subject:  p.ownerUID + ":" + p.name,
			Detail: fmt.Sprintf("The reaction %s of the %s %s watches the %s %s, which is another agency's. A reaction "+
				"watches its own agency's definitions only: it does not fire, and each run of that %s is recorded as "+
				"refused. Copy what it watches into this agency and point the reaction at it, or remove the reaction. "+
				"This notice clears when it is done.",
				p.name, p.ownerKind, p.ownerName, p.onKind, p.onName, p.onKind),
		})
	}
	return Reconcile(ctx, database, KindReactionOtherAgency, found)
}
