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
// The reaction's upstream is the definition it is pinned to (on_uid), else
// the one definition that holds its name, as the reactor tells it (2.4.2,
// present defect 60: a reaction that pinned none was taken to be its own
// agency's, and was refused at fire time with nothing said). A Git upstream
// named only in Global's repository is Global's: the reaction, written when a
// name resolved there next, no longer fires (GR-16 went with §0a).
//
// Subject: `<owner uid>:<reaction name>` (`<kind>/<name>` for an owner with
// no identity). Filed under the owner's agency. It clears when the reaction
// is removed or watches its own agency's definition.
const KindReactionOtherAgency = "reaction_other_agency"

func checkReactionOtherAgency(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT COALESCE(r.owner_uid, ''), r.owner_source, r.owner_kind, r.owner_name, r.name,
		       COALESCE(r.on_uid, ''), r.on_source, r.on_kind, r.on_name
		  FROM reactions r`)
	if err != nil {
		return err
	}
	type pair struct{ ownerUID, ownerSource, ownerKind, ownerName, name, onUID, onSource, onKind, onName string }
	var all []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.ownerUID, &p.ownerSource, &p.ownerKind, &p.ownerName, &p.name,
			&p.onUID, &p.onSource, &p.onKind, &p.onName); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	table := func(kind string) string {
		if kind == "workflow" {
			return "workflows"
		}
		return "jobs"
	}
	// agencyOf tells a definition's agency by its uid, else by its name when
	// one definition of the kind and source holds it.
	agencyOf := func(uid, kind, source, name string) (sql.NullString, error) {
		var a sql.NullString
		if uid != "" {
			if err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, uid, kind).Scan(&a); err != nil || a.Valid {
				return a, err
			}
		}
		var byName sql.NullString
		if err := database.QueryRowContext(ctx, `SELECT CASE WHEN COUNT(*) = 1 THEN MAX(uid) END FROM `+table(kind)+
			` WHERE source = ? AND name = ? AND deleted_at IS NULL`, source, name).Scan(&byName); err != nil || !byName.Valid {
			return sql.NullString{}, err
		}
		err := database.QueryRowContext(ctx, `SELECT `+agencyid.DefinitionSQL, byName.String, kind).Scan(&a)
		return a, err
	}
	inRepoOf := func(agency, kind, name, repo string) (bool, error) {
		var n int
		err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table(kind)+
			` WHERE source = 'git' AND name = ? AND deleted_at IS NULL AND COALESCE(repo_id, 'global') = `+repo,
			name, agency).Scan(&n)
		return n > 0, err
	}
	var found []Finding
	for _, p := range all {
		owner, err := agencyOf(p.ownerUID, p.ownerKind, p.ownerSource, p.ownerName)
		if err != nil {
			return err
		}
		if !owner.Valid {
			continue
		}
		var upstream sql.NullString
		if p.onUID == "" && p.onSource == "git" {
			// As the reactor matches it: the owner's agency's repository first.
			own, err := inRepoOf(owner.String, p.onKind, p.onName, `(SELECT id FROM git_repos WHERE agency_id = ?)`)
			if err != nil {
				return err
			}
			if own {
				continue
			}
			global, err := inRepoOf(agencyid.Global, p.onKind, p.onName, `?`)
			if err != nil {
				return err
			}
			if global && owner.String != agencyid.Global {
				upstream = sql.NullString{String: agencyid.Global, Valid: true}
			}
		}
		if !upstream.Valid {
			if upstream, err = agencyOf(p.onUID, p.onKind, p.onSource, p.onName); err != nil {
				return err
			}
		}
		if !upstream.Valid || owner.String == upstream.String {
			continue
		}
		subject := p.ownerUID + ":" + p.name
		if p.ownerUID == "" {
			subject = p.ownerKind + "/" + p.ownerName + ":" + p.name
		}
		found = append(found, Finding{
			AgencyID: owner.String,
			Subject:  subject,
			Detail: fmt.Sprintf("The reaction %s of the %s %s watches the %s %s, which is another agency's. A reaction "+
				"watches its own agency's definitions only: it does not fire, and each run of that %s is recorded as "+
				"refused. Copy what it watches into this agency and point the reaction at it, or remove the reaction. "+
				"This notice clears when it is done.",
				p.name, p.ownerKind, p.ownerName, p.onKind, p.onName, p.onKind),
		})
	}
	return Reconcile(ctx, database, KindReactionOtherAgency, found)
}
