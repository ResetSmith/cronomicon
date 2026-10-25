package notices

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindCentralHoldsWork — the built-in agency still owns something that runs,
// or something runs with (2.5.3, CA-17): what an installation from before
// 2.5.3 held as Global's. Since 2.5.3 Central is the installation's
// administration and runs nothing (CA-1): none of it runs, and nothing new is
// made Central's (CA-4). Nothing is moved automatically (the owner's
// preference); this lists it, with the way out for each kind.
//
// One finding per kind, naming the rows (the first ten, and how many more).
// Subject: `central:<kind>`. Filed under the built-in agency, for global
// administrators. A finding clears when Central holds none of its kind.
//
// What an AGENCY's definitions depend on of Central's is told to that agency
// by its own notices (global_credential_bound, global_host_record,
// global_schedule_bound, global_script_job); a Central agent that still
// serves agencies, by legacy_placement; a Central secret, variable or key
// listed for an agency, to that agency by shared_ownership. Those stay.
const KindCentralHoldsWork = "central_holds_work"

// centralLeftoverRepo is settings.CentralLeftoverRepo, given by the server at
// start (the notices package does not import settings).
var centralLeftoverRepo func(context.Context, interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (bool, error)

// SetCentralLeftoverRepoCheck gives the checks the reader of a leftover
// repository of the built-in agency's.
func SetCentralLeftoverRepoCheck(f func(context.Context, interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) (bool, error)) {
	centralLeftoverRepo = f
}

// centralHeld is one kind of row the built-in agency may hold: the query
// lists its names, the remedy says what a global administrator does. A job,
// workflow or schedule in the recycle bin is not held (deleting it is the
// way out). A host record imported for a scope is the scope's, whatever its
// owner column says (migration 1230): only a hand-written one is listed, and
// a scope of Central's is listed as a scope.
var centralHeld = []struct {
	kind, noun, query, remedy string
}{
	{"job", "jobs built in the app", `SELECT name FROM jobs WHERE source <> 'git' AND owner_agency = 'global' AND deleted_at IS NULL ORDER BY name`,
		"A job's name is the installation's, and this one keeps it here and in the recycle bin: note its settings, delete it " +
			"here and then for good (Settings → Recycle Bin → Delete forever), then make it again in the agency whose work it is " +
			"(Jobs, working in that agency) or publish it into that agency's repository. Jobs built in the app are not moved: " +
			"2.6.0 keeps definitions in Git only."},
	{"workflow", "workflows built in the app", `SELECT name FROM workflows WHERE source <> 'git' AND owner_agency = 'global' AND deleted_at IS NULL ORDER BY name`,
		"A workflow's name is the installation's, and this one keeps it here and in the recycle bin: note its steps, delete " +
			"it here and then for good (Settings → Recycle Bin → Delete forever), then make it again in its agency or publish " +
			"it into that agency's repository."},
	{"schedule", "schedules built in the app", `SELECT name FROM schedules WHERE source <> 'git' AND owner_agency = 'global' AND deleted_at IS NULL ORDER BY name`,
		"Make each again in the agency whose definitions use it, then delete it here."},
	{"scope", "scopes built in the app", `SELECT name FROM scopes WHERE source <> 'git' AND owner_agency = 'global' ORDER BY name`,
		"Give each to the agency whose hosts they are (Scopes → the scope's agency: a repair, with a reason), or clone it into each agency that needs it."},
	{"secret", "secrets", `SELECT key FROM secrets WHERE owner_agency = 'global' ORDER BY key`,
		"Give each to the agency whose runs use it (Env Vars → Secrets → the row → Repair: move to another agency…, with a reason). A secret two agencies " +
			"need is shared through a common Vault path, with a row in each agency."},
	{"env-var", "variables", `SELECT key FROM env_vars WHERE owner_agency = 'global' ORDER BY key`,
		"Give each to the agency whose runs use it (Env Vars → Variables → the row → Repair: move to another agency…, with a reason), or make it again in each agency that needs it."},
	{"ssh-credential", "SSH keys", `SELECT label FROM ssh_credentials WHERE owner_agency = 'global' ORDER BY label`,
		"Give each to the agency whose hosts it opens (Env Vars → SSH Keys → the row → Repair: move to another agency…, with a reason)."},
	{"host", "host records", `SELECT hostname FROM ssh_hosts WHERE scope_id IS NULL AND owner_agency = 'global' ORDER BY hostname`,
		"Give each to the agency whose scope reaches the host (a repair, with a reason), or make it again there."},
	{"bastion", "bastions", `SELECT COALESCE(NULLIF(name, ''), hostname) AS n FROM bastions WHERE owner_agency = 'global' ORDER BY n`,
		"Give each to the agency whose hosts it reaches (a repair, with a reason), or make it again there."},
	{"runner", "agents serving it alone", `
		SELECT rn.name FROM runners rn
		 WHERE rn.kind <> 'server' AND rn.owner_agency = 'global'
		   AND NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = rn.id AND ra.agency_id <> 'global')
		 ORDER BY rn.name`,
		"An agent serving the built-in agency alone takes no work (one enrolled with the environment's bootstrap token " +
			"before 2.5.3 is one). Deregister each and enrol it again with a registration token minted in its agency " +
			"(Runners → + Add Runner, working in that agency)."},
}

func checkCentralHoldsWork(ctx context.Context, database *sql.DB) error {
	var found []Finding
	if centralLeftoverRepo == nil {
		// Not asked (a caller that gave no reader): what is open stays open,
		// as it was, rather than being resolved by a question nobody asked.
		var f Finding
		err := database.QueryRowContext(ctx, `SELECT agency_id, subject, detail FROM notices
		         WHERE kind = ? AND subject = 'central:repository' AND resolved_at IS NULL`, KindCentralHoldsWork).
			Scan(&f.AgencyID, &f.Subject, &f.Detail)
		switch {
		case err == nil:
			found = append(found, f)
		case err != sql.ErrNoRows:
			return err
		}
	} else {
		left, err := centralLeftoverRepo(ctx, database)
		if err != nil {
			return err
		}
		if left {
			var jobs, workflows, schedules, scripts, scopes int
			if err := database.QueryRowContext(ctx, `
				SELECT (SELECT COUNT(*) FROM jobs WHERE source = 'git' AND repo_id = 'global'),
				       (SELECT COUNT(*) FROM workflows WHERE source = 'git' AND repo_id = 'global'),
				       (SELECT COUNT(*) FROM schedules WHERE source = 'git' AND repo_id = 'global'),
				       (SELECT COUNT(*) FROM scripts WHERE repo_id = 'global'),
				       (SELECT COUNT(*) FROM scopes WHERE source = 'git' AND repo_id = 'global')`).Scan(&jobs, &workflows, &schedules, &scripts, &scopes); err != nil {
				return err
			}
			found = append(found, Finding{
				AgencyID: agencyid.Global,
				Subject:  "central:repository",
				Detail: fmt.Sprintf("The built-in administration agency still holds the repository it synced before 2.5.3 "+
					"(%d jobs, %d workflows, %d schedules, %d scripts and %d scopes from it). It no longer syncs, and nothing of it runs. "+
					"To keep those definitions, remove the leftover (Settings → Repositories, the built-in agency's: a "+
					"repair, with a reason; it is refused while one of its scopes has work queued), then connect the same "+
					"URL and branch to the agency whose work they are: its sync brings them in as that agency's, except a job "+
					"with no scope, which an agency's sync refuses: give each one of that agency's scopes first (cronomicon "+
					"validate -agency on a checkout lists them). Their run history stays.", jobs, workflows, schedules, scripts, scopes),
			})
		}
	}
	for _, k := range centralHeld {
		rows, err := database.QueryContext(ctx, k.query)
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(names) == 0 {
			continue
		}
		list := names
		more := ""
		if len(list) > 10 {
			more = fmt.Sprintf(", and %d more", len(list)-10)
			list = list[:10]
		}
		found = append(found, Finding{
			AgencyID: agencyid.Global,
			Subject:  "central:" + k.kind,
			Detail: fmt.Sprintf("The built-in administration agency still holds %d %s from before 2.5.3: %s%s. Since 2.5.3 "+
				"it runs nothing, so none of it is used. %s This notice clears when it holds none.",
				len(names), k.noun, strings.Join(list, ", "), more, k.remedy),
		})
	}
	return Reconcile(ctx, database, KindCentralHoldsWork, found)
}
