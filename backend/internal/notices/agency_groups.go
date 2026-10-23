package notices

import (
	"context"
	"database/sql"
	"fmt"
)

// KindGrantGroupUnlisted — an agency's access grant names a directory group
// that is not on the agency's group list (2.5.0 C5, GA-2). Since 2.5.0 a
// global administrator lists the groups each agency's grants may name, and a
// delegate grants only to those (GA-1). The lists started empty (question 83),
// so on the day of the upgrade every agency grant is one; later, a global
// administrator's grant to a group not listed is one too (question 84).
// Nothing is refused while it is so: the grant works as before, and its
// agency's delegates may delete it but not change it.
//
// Subject: the grant's id. Filed under the grant's agency. It clears when the
// group is listed or the grant is gone.
const KindGrantGroupUnlisted = "grant_group_unlisted"

func checkGrantGroupUnlisted(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT g.id, g.ad_group, g.role, a.id, a.name
		  FROM access_grants g JOIN agencies a ON a.id = g.agency_id
		 WHERE g.all_scopes = 0
		   AND NOT EXISTS (SELECT 1 FROM agency_groups l WHERE l.agency_id = g.agency_id AND l.ad_group = g.ad_group)
		 ORDER BY a.name, g.ad_group, g.role`)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, group, role, agency, name string
		if err := rows.Scan(&id, &group, &role, &agency, &name); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  id,
			Detail: fmt.Sprintf("The grant giving %s the %s role in %s names a group that is not on %s's group list. "+
				"Since 2.5.0 a global administrator lists the groups an agency's grants may name, and its administrators "+
				"grant only to those. The grant works as before. A global administrator can list the group (Scopes → "+
				"Agencies), or an administrator of %s can delete the grant; until the group is listed they cannot change it. "+
				"This notice clears when either is done.", group, role, name, name, name),
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return Reconcile(ctx, database, KindGrantGroupUnlisted, found)
}
