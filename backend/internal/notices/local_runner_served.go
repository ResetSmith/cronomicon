package notices

import (
	"context"
	"database/sql"
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

// Retired in 2.5.3 (CA-11): the local runner belongs to whichever agency a
// global administrator gives it (CA-8), one of these included, so "it serves
// Global only now" no longer holds. Its notices are cleared; the check stays
// one release so that they are, and goes after.
func checkLocalRunnerServed(ctx context.Context, database *sql.DB) error {
	return Reconcile(ctx, database, KindLocalRunnerServed, nil)
}

// KindLocalRunnerUnassigned — the local runner's switch is stored on, and it
// belongs to no agency, so it is off (2.5.3, CA-9): the state an installation
// from before 2.5.3 arrives in when its local runner, which was Global's, was
// on. Its runs were Global's, which runs nothing since (CA-1).
//
// Subject: "local-runner". Filed under the built-in agency, for global
// administrators. It clears when the local runner is given to an agency, or
// turned off.
const KindLocalRunnerUnassigned = "local_runner_unassigned"

// localRunnerUnassignedOn is settings.LocalRunnerUnassignedButSwitchedOn,
// given by the server at start (the notices package does not import settings).
var localRunnerUnassignedOn func(context.Context, *sql.DB) (bool, error)

// SetLocalRunnerUnassignedCheck gives the checks the reader of that state.
func SetLocalRunnerUnassignedCheck(f func(context.Context, *sql.DB) (bool, error)) {
	localRunnerUnassignedOn = f
}

func checkLocalRunnerUnassigned(ctx context.Context, database *sql.DB) error {
	if localRunnerUnassignedOn == nil {
		return nil
	}
	on, err := localRunnerUnassignedOn(ctx, database)
	if err != nil {
		return err
	}
	var found []Finding
	if on {
		found = append(found, Finding{
			AgencyID: "global",
			Subject:  "local-runner",
			Detail: "The local runner was on. Since 2.5.3 it belongs to the one agency a global administrator gives it, " +
				"or to none, and with none it is off: it was the built-in agency's, which runs nothing now. Give it to the " +
				"agency whose shell jobs it is to run (Settings → Local runner), or turn it off. Given, it is off until that " +
				"agency has approved its own hosts' keys for it and a global administrator turns it on. This notice clears " +
				"when either is done.",
		})
	}
	return Reconcile(ctx, database, KindLocalRunnerUnassigned, found)
}
