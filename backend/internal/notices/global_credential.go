package notices

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindGlobalCredentialBound — a job of an agency binds a secret, a variable
// or an SSH key by a name that only Global holds (2.4.0, §0a, IN-6, IN-14).
// Until §0a a row of Global's was usable by every agency's runs (LR-23), and
// a row Global owned that was shared into one agency by its membership was
// usable by that agency's. Now a row of Global's is Global's runs' only: the
// job's run does not find the name, and fails, naming it.
//
// What is reported is the binding as it stands: the job's agency has no row
// of that kind and name of its own, and Global has one. A secret two agencies
// need is shared through a common Vault path (IN-6): each agency keeps its own
// row pointing at it.
//
// Subject: `<job uid>:<kind>:<name>`. Filed under the job's agency. It clears
// when the agency has a row of the name, or the binding goes.
const KindGlobalCredentialBound = "global_credential_bound"

func checkGlobalCredentialBound(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT owner_uid, owner_name, ref_kind, ref_name
		  FROM reference_bindings
		 WHERE owner_kind = 'job' AND COALESCE(owner_uid, '') <> ''`)
	if err != nil {
		return err
	}
	type binding struct{ uid, job, kind, name, agency string }
	var bound []binding
	for rows.Next() {
		var b binding
		if err := rows.Scan(&b.uid, &b.job, &b.kind, &b.name); err != nil {
			rows.Close()
			return err
		}
		bound = append(bound, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var all []binding
	for _, b := range bound {
		var agency sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, b.uid, "job").Scan(&agency); err != nil {
			return err
		}
		if !agency.Valid || agency.String == agencyid.Global {
			continue
		}
		b.agency = agency.String
		all = append(all, b)
	}
	// Whether an agency holds a row of a kind and name.
	holds := func(kind, name, agency string) (bool, error) {
		q := `SELECT EXISTS (SELECT 1 FROM secrets WHERE key = ?1 AND owner_agency = ?2)`
		switch kind {
		case "var":
			q = `SELECT EXISTS (SELECT 1 FROM env_vars WHERE key = ?1 AND owner_agency = ?2)`
		case "key":
			q = `SELECT EXISTS (SELECT 1 FROM ssh_credentials WHERE label = ?1 AND owner_agency = ?2)`
		}
		var yes bool
		err := database.QueryRowContext(ctx, q, name, agency).Scan(&yes)
		return yes, err
	}
	what := map[string]string{"secret": "secret", "var": "variable", "key": "SSH key"}
	var found []Finding
	for _, b := range all {
		own, err := holds(b.kind, b.name, b.agency)
		if err != nil {
			return err
		}
		if own {
			continue
		}
		global, err := holds(b.kind, b.name, agencyid.Global)
		if err != nil {
			return err
		}
		if !global {
			continue
		}
		found = append(found, Finding{
			AgencyID: b.agency,
			Subject:  b.uid + ":" + b.kind + ":" + b.name,
			Detail: fmt.Sprintf("The job %s binds the %s %s, which only Global has. A %s of Global's is used by "+
				"Global's runs only: this job's runs do not find it, and fail, naming it. Give this agency a %s of that "+
				"name (for a secret two agencies need, each can point at the same Vault path), or change the binding. "+
				"This notice clears when it is done.",
				b.job, what[b.kind], b.name, what[b.kind], what[b.kind]),
		})
	}
	return Reconcile(ctx, database, KindGlobalCredentialBound, found)
}
