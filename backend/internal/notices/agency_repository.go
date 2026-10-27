package notices

import (
	"context"
	"database/sql"
	"strings"
	"sync"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// KindAgencyNoRepository — an agency has no repository connected (2.4.0
// Phase I3; §0a, IN-2; the owner's answer to question 39). A repository is
// where the agency's definitions live, and where its publishes, and from
// 2.5.0 the assignments it is given, are committed. Since 2.5.1 (SG-40) an
// agency connects it at creation or later, and a connected one is never
// disconnected; so this is an agency created without it, or one from before
// 2.4.0. Nothing else is refused while it is so: the agency cannot receive an
// assignment or be published to.
//
// The built-in agency is not asked (2.5.3, CA-6): it runs nothing and has no
// repository, so having none is not something to settle. Until 2.5.3 it was
// asked as "Global's repository is not connected".
//
// Subject: the agency's id. Filed under the agency. It clears when the
// agency's repository is connected.
const KindAgencyNoRepository = "agency_no_repository"

func checkAgencyNoRepository(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT a.id, a.name FROM agencies a
		 WHERE a.id <> ?
		   AND NOT EXISTS (SELECT 1 FROM git_repos g WHERE g.agency_id = a.id AND COALESCE(g.url, '') <> '')
		 ORDER BY a.name`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		found = append(found, Finding{
			AgencyID: id,
			Subject:  id,
			Detail: "The agency " + name + " has no repository connected. A repository is where an agency's definitions " +
				"live, and where its publishes and the assignments it is given are committed. Until it is connected the agency " +
				"works as it does now, and cannot receive an assignment or be published to. Its administrator (configureApp on " +
				name + ") connects it (a URL on the allowed hosts, and a token that can push) under Settings → Repositories; " +
				"this notice clears when it is done.",
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	return Reconcile(ctx, database, KindAgencyNoRepository, found)
}

// KindEnvironmentSettingsIgnored — the server was started with environment
// settings that 2.5.3 no longer reads (question 207): the CRONOMICON_GITLAB_*
// that configured Global's repository, which the built-in agency no longer
// has (CA-6), and CRONOMICON_RUNNER_BOOTSTRAP_TOKEN, which enrolled agents as
// Global's, which runs nothing (CA-1). They are named in a warning at start
// too.
//
// Subject: "environment". Filed under the built-in agency, for global
// administrators. It clears when the server starts without them.
const KindEnvironmentSettingsIgnored = "environment_settings_ignored"

var (
	ignoredEnvMu sync.Mutex
	ignoredEnv   []string
)

// SetIgnoredEnvironmentSettings tells the checks which of those settings the
// server was started with (settings.EnvSettingsIgnored; the server sets it at
// start): the checks are given the database only.
func SetIgnoredEnvironmentSettings(names []string) {
	ignoredEnvMu.Lock()
	defer ignoredEnvMu.Unlock()
	ignoredEnv = append([]string(nil), names...)
}

func checkEnvironmentSettingsIgnored(ctx context.Context, database *sql.DB) error {
	ignoredEnvMu.Lock()
	names := append([]string(nil), ignoredEnv...)
	ignoredEnvMu.Unlock()
	var found []Finding
	if len(names) > 0 {
		var gitlab, token bool
		for _, n := range names {
			if n == "CRONOMICON_RUNNER_BOOTSTRAP_TOKEN" {
				token = true
			} else {
				gitlab = true
			}
		}
		detail := "The server was started with " + strings.Join(names, ", ") + ", which this version no longer reads: " +
			"the built-in administration agency runs nothing and has no repository."
		if gitlab {
			detail += " Connect the repository to an ordinary agency under Settings → Repositories. If the built-in " +
				"agency still holds what it synced, a global administrator removes that leftover first (Settings → " +
				"Repositories, the built-in agency's), then connects the same URL to the agency."
		}
		if token {
			detail += " Enrol agents with a registration token minted in their agency (Runners). An agent enrolled with " +
				"the environment's token belongs to the built-in agency and takes no work: deregister it and enrol it " +
				"again with an agency's token."
		}
		detail += " Remove the settings from the server's environment; this notice clears at the next start without them."
		found = append(found, Finding{AgencyID: agencyid.Global, Subject: "environment", Detail: detail})
	}
	return Reconcile(ctx, database, KindEnvironmentSettingsIgnored, found)
}
