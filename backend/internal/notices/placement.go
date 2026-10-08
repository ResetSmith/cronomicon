package notices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
)

// Where a shell job runs changed in 2.3.0 (LR-40, LR-42). Until then a shell
// job ran from the server unless it or a default said "runner"; now every run
// is written for the runner executor and whichever eligible runner asks first
// takes it — an agent, or the server as the local runner. Nothing is moved or
// bound automatically (LR-16: a warning only). The upgrade records, once, how
// each unbound scope's shell jobs were being served, and the checks below say
// where that can now differ, for as long as it can.
const (
	// KindMayRunOnAgent — a scope whose shell jobs ran from the server, where an
	// agent can now take them too. Subject: the scope's id ("no-scope" for
	// jobs with none). Filed under the scope's agency.
	KindMayRunOnAgent = "may_run_on_agent"
	// KindMixedScope — a scope some of whose shell jobs ran from the server and
	// some on agents; either can take any of them now.
	KindMixedScope = "mixed_scope"
	// KindMayRunOnServer — a scope whose shell jobs ran on agents, where the
	// local runner can now take them too.
	KindMayRunOnServer = "may_run_on_server"
	// KindAgencyPlaced — the upgrade put the local runner on an agency's serve
	// list, because the server was running that agency's shell jobs. Subject:
	// the agency's id. Filed under Global: the serve list is a global
	// administrator's.
	KindAgencyPlaced = "agency_placed"
	// KindShellJobRequires — a shell job that declares requirement tokens no
	// registered agent serving its scope advertises. The local runner
	// advertises none, and the SSH executor used to ignore them, so the job ran
	// until 2.3.0 and would now wait for ever. Subject: the job's uid.
	KindShellJobRequires = "shell_job_requires"
	// KindNoRunnerForShellJobs — an agency (Global among them) has shell jobs
	// and no registered runner that serves it can run a shell job. Until 2.3.0
	// the server ran every agency's shell jobs; it now runs those of the
	// agencies on the local runner's list. The upgrade places it where the
	// server was serving on that day; this is for everything after — a first
	// shell job in an agency, a job restored from the bin, an agent removed.
	// Subject: the agency's id. Filed under that agency.
	KindNoRunnerForShellJobs = "no_runner_for_shell_jobs"
)

// NoScopeSubject stands for "the jobs that have no scope" where a scope id is
// expected. Such a job is Global's.
const NoScopeSubject = "no-scope"

// LocalRunnerUpgradeKey is the settings row holding what the 2.3.0 upgrade
// found, as a LocalRunnerUpgrade. Its presence is also the marker that the
// pass has run.
const LocalRunnerUpgradeKey = "upgrade.2.3.0.localRunner"

// How a scope's shell jobs were served before 2.3.0.
const (
	ServedByServer = "server" // every one resolved to the SSH executor
	ServedByAgents = "agents" // every one resolved to the runner executor
	ServedByBoth   = "both"
)

// LocalRunnerUpgrade is the record the upgrade pass leaves behind.
type LocalRunnerUpgrade struct {
	// WasOn: the SSH executor was turned on when 2.3.0 first started.
	WasOn bool `json:"wasOn"`
	// Scopes: scope id (or NoScopeSubject) → how its shell jobs were served.
	// Unbound scopes with at least one shell job, and only when WasOn.
	Scopes map[string]string `json:"scopes"`
	// Placed: the agencies the pass added to the local runner's serve list.
	Placed []string `json:"placed"`
}

// ReadLocalRunnerUpgrade returns the record, or nil when the pass has not run.
func ReadLocalRunnerUpgrade(ctx context.Context, database *sql.DB) (*LocalRunnerUpgrade, error) {
	var raw string
	err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, LocalRunnerUpgradeKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var u LocalRunnerUpgrade
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		return nil, fmt.Errorf("read the local runner upgrade record: %w", err)
	}
	return &u, nil
}

// shellTypesSQL lists the run types the local runner takes (LR-41).
const shellTypesSQL = `('bash','perl','powershell','python')`

// engineDifference is what an operator needs to know when a job can land on
// either engine: it is the same job, and it is not the same environment.
const engineDifference = "The two are not the same place to run: an agent connects with the keys in its own key directory " +
	"(or one delivered to it) and trusts the hosts in its own reviewed known_hosts; the server connects with the keys " +
	"stored in Cronomicon and the host keys kept with the SSH targets. A run across several hosts where some fail ends " +
	"as a warning on the server and as a failure on an agent. "

const bindRemedy = "To decide where they run, bind the scope to the runners that should serve it (Scopes → the scope → Bind runners)."

// checkRunPlacement reconciles the three "may run somewhere else" kinds and
// agency_placed from the upgrade's record and the present state. A condition
// holds only while the scope is still unbound and both kinds of runner can
// still reach it; binding the scope, or removing the other runner, resolves it.
func checkRunPlacement(ctx context.Context, database *sql.DB) error {
	u, err := ReadLocalRunnerUpgrade(ctx, database)
	if err != nil {
		return err
	}
	found := map[string][]Finding{KindMayRunOnAgent: nil, KindMixedScope: nil, KindMayRunOnServer: nil, KindAgencyPlaced: nil}
	if u != nil && u.WasOn {
		ids := make([]string, 0, len(u.Scopes))
		for id := range u.Scopes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			st, err := scopeServing(ctx, database, id)
			if err != nil {
				return err
			}
			if st == nil || st.bound {
				continue // gone, or the operator has decided where it runs
			}
			jobs := "its shell jobs"
			if len(st.jobs) > 0 {
				jobs = "its shell jobs (" + strings.Join(st.jobs, ", ") + ")"
			}
			switch served := u.Scopes[id]; {
			case served == ServedByServer && st.agent && st.local:
				found[KindMayRunOnAgent] = append(found[KindMayRunOnAgent], Finding{AgencyID: st.agency, Subject: id,
					Detail: fmt.Sprintf("Until 2.3.0 %s's shell jobs ran from the server. They are now taken by whichever runner "+
						"that serves %s asks first, and an agent serves it (%s) as well as the local runner, so %s may run on "+
						"that agent. %s%s", st.label, st.agencyName, st.agentNames, jobs, engineDifference, bindRemedy)})
			case served == ServedByBoth && st.agent && st.local:
				found[KindMixedScope] = append(found[KindMixedScope], Finding{AgencyID: st.agency, Subject: id,
					Detail: fmt.Sprintf("Until 2.3.0 some of %s's shell jobs ran from the server and some on agents, each as its "+
						"own executor setting said. That setting is no longer read: any of %s may now be taken by the local "+
						"runner or by an agent that serves %s (%s). %s%s", st.label, jobs, st.agencyName, st.agentNames,
						engineDifference, bindRemedy)})
			case served == ServedByAgents && st.local:
				found[KindMayRunOnServer] = append(found[KindMayRunOnServer], Finding{AgencyID: st.agency, Subject: id,
					Detail: fmt.Sprintf("Until 2.3.0 %s's shell jobs ran on agents. The local runner serves %s too, so when it "+
						"is turned on %s may run from the server instead. %s%s", st.label, st.agencyName, jobs,
						engineDifference, bindRemedy)})
			}
		}
		for _, agency := range u.Placed {
			var name string
			var serves bool
			err := database.QueryRowContext(ctx, `
				SELECT a.name, EXISTS (SELECT 1 FROM runner_agencies ra JOIN runners rn ON rn.id = ra.runner_id
				                        WHERE rn.kind = 'server' AND ra.agency_id = a.id)
				  FROM agencies a WHERE a.id = ?`, agency).Scan(&name, &serves)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && !serves) {
				continue
			}
			if err != nil {
				return err
			}
			found[KindAgencyPlaced] = append(found[KindAgencyPlaced], Finding{AgencyID: agencyid.Global, Subject: agency,
				Detail: fmt.Sprintf("The upgrade to 2.3.0 added %s to the agencies the local runner serves, because the server "+
					"was running that agency's shell jobs and would otherwise have stopped. The local runner takes the runs of "+
					"the agencies on its list and no others (Settings → Local runner). Leave %s there to keep those jobs "+
					"running from the server, or take it off once an agent of its own serves it.", name, name)})
		}
	}
	for _, kind := range []string{KindMayRunOnAgent, KindMixedScope, KindMayRunOnServer, KindAgencyPlaced} {
		if err := Reconcile(ctx, database, kind, found[kind]); err != nil {
			return err
		}
	}
	return nil
}

// servingState is who can take a scope's shell runs right now.
type servingState struct {
	label      string   // "the scope dmz-web", or "the jobs with no scope"
	agency     string   // where the notice is filed
	agencyName string   // the agency whose runners serve it
	bound      bool     // the scope names its runners
	agent      bool     // an agent with a shell capability serves its agency
	agentNames string   // up to three of them
	local      bool     // the local runner serves its agency
	jobs       []string // up to five of its shell jobs, by name
}

// scopeServing reads the state for a scope id, or for NoScopeSubject. It
// returns nil for a scope that no longer exists.
func scopeServing(ctx context.Context, database *sql.DB, scopeID string) (*servingState, error) {
	st := &servingState{}
	var scopeName string
	var agencies []string
	if scopeID == NoScopeSubject {
		st.label = "the jobs with no scope"
		agencies = []string{agencyid.Global}
	} else {
		err := database.QueryRowContext(ctx, `
			SELECT sc.name, EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id)
			  FROM scopes sc WHERE sc.id = ?`, scopeID).Scan(&scopeName, &st.bound)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		st.label = "the scope " + scopeName
		rows, err := database.QueryContext(ctx,
			`SELECT agency_id FROM scope_agencies WHERE scope_id = ? ORDER BY agency_id`, scopeID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a string
			if err := rows.Scan(&a); err != nil {
				rows.Close()
				return nil, err
			}
			agencies = append(agencies, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	if len(agencies) == 0 {
		return nil, nil // a scope in no agency is damage; `orphaned` reports it
	}
	// One agency is the rule (LR-7); a scope still in several is a global
	// administrator's to settle, and so is this.
	st.agency = agencies[0]
	if len(agencies) > 1 {
		st.agency = agencyid.Global
	}
	agencyJSON, _ := json.Marshal(agencies)
	var names sql.NullString
	if err := database.QueryRowContext(ctx, `
		SELECT group_concat(name, ', ') FROM (SELECT name FROM agencies
		 WHERE id IN (SELECT value FROM json_each(?)) ORDER BY name)`, string(agencyJSON)).Scan(&names); err != nil {
		return nil, err
	}
	st.agencyName = names.String

	rows, err := database.QueryContext(ctx, `
		SELECT rn.name, rn.kind = 'server'
		  FROM runners rn
		 WHERE EXISTS (SELECT 1 FROM runner_agencies ra
		                WHERE ra.runner_id = rn.id AND ra.agency_id IN (SELECT value FROM json_each(?)))
		   AND EXISTS (SELECT 1 FROM json_each(COALESCE(rn.capabilities, '[]')) c WHERE c.value IN `+shellTypesSQL+`)
		 ORDER BY rn.name, rn.id`, string(agencyJSON))
	if err != nil {
		return nil, err
	}
	var agents []string
	for rows.Next() {
		var name string
		var local bool
		if err := rows.Scan(&name, &local); err != nil {
			rows.Close()
			return nil, err
		}
		if local {
			st.local = true
		} else {
			st.agent = true
			if len(agents) < 3 {
				agents = append(agents, name)
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	st.agentNames = strings.Join(agents, ", ")

	jrows, err := database.QueryContext(ctx, `
		SELECT name FROM jobs
		 WHERE COALESCE(scope, '') = ? AND deleted_at IS NULL AND run_type IN `+shellTypesSQL+`
		 ORDER BY name LIMIT 5`, scopeName)
	if err != nil {
		return nil, err
	}
	defer jrows.Close()
	for jrows.Next() {
		var n string
		if err := jrows.Scan(&n); err != nil {
			return nil, err
		}
		st.jobs = append(st.jobs, n)
	}
	return st, jrows.Err()
}

// checkShellJobRequires finds shell jobs whose requirement tokens no registered
// agent serving the job's scope advertises. Whether the agent is online does
// not matter here: an offline agent is an ordinary wait, and this is about a
// job that has nobody to wait for.
func checkShellJobRequires(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(j.uid, ''), j.source || ':' || j.name), j.name, j.source, COALESCE(j.scope, ''), j.requires_json,
		       COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
		                   FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
		                  WHERE sc.name = j.scope), ?)
		  FROM jobs j
		 WHERE j.deleted_at IS NULL
		   AND j.run_type IN `+shellTypesSQL+`
		   AND json_array_length(CASE WHEN json_valid(j.requires_json) THEN j.requires_json ELSE '[]' END) > 0
		   AND NOT EXISTS (
		       SELECT 1 FROM runners rn
		        WHERE rn.kind <> 'server'
		          AND EXISTS (
		              SELECT 1 FROM runner_agencies ra
		               WHERE ra.runner_id = rn.id
		                 AND ((COALESCE(j.scope, '') = '' AND ra.agency_id = ?)
		                      OR ra.agency_id IN (SELECT sa.agency_id FROM scope_agencies sa
		                                            JOIN scopes sc ON sc.id = sa.scope_id
		                                           WHERE sc.name = j.scope)))
		          AND NOT EXISTS (
		              SELECT 1 FROM json_each(CASE WHEN json_valid(j.requires_json) THEN j.requires_json ELSE '[]' END) need
		               WHERE need.value NOT IN (SELECT have.value FROM json_each(COALESCE(rn.capabilities, '[]')) have)))
		 ORDER BY j.name, j.source`, agencyid.Global, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var subject, name, source, scope, requires, agency string
		if err := rows.Scan(&subject, &name, &source, &scope, &requires, &agency); err != nil {
			rows.Close()
			return err
		}
		var tokens []string
		_ = json.Unmarshal([]byte(requires), &tokens)
		where := "edit the job"
		if source == "git" {
			where = "change the job's file in Git"
		}
		on := "no scope (it is Global's)"
		if scope != "" {
			on = "the scope " + scope
		}
		found = append(found, Finding{
			AgencyID: agency,
			Subject:  subject,
			Detail: fmt.Sprintf("The shell job %s (on %s) requires %s, and no registered agent that serves it advertises "+
				"that. Until 2.3.0 a shell job run from the server was started without its requirements being read; "+
				"they are read for every run now, the local runner advertises none, and so a run of this job waits "+
				"for a runner that does not exist. Either %s to drop the requirement, or enrol an agent that has it.",
				name, on, strings.Join(tokens, ", "), where),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindShellJobRequires, found)
}

// checkNoRunnerForShellJobs finds agencies whose shell jobs nobody can run: no
// registered runner — agent or local — serves the agency with a shell
// capability. Registered, not online: an agent that is merely offline is an
// ordinary wait, and its runs say so.
func checkNoRunnerForShellJobs(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		WITH shell_jobs AS (
			SELECT j.name AS job,
			       CASE WHEN COALESCE(j.scope, '') = '' THEN ? ELSE sa.agency_id END AS agency
			  FROM jobs j
			  LEFT JOIN scopes sc ON sc.name = j.scope
			  LEFT JOIN scope_agencies sa ON sa.scope_id = sc.id
			 WHERE j.deleted_at IS NULL AND j.run_type IN `+shellTypesSQL+`
		)
		SELECT a.id, a.name, COUNT(DISTINCT sj.job),
		       (SELECT group_concat(job, ', ') FROM (
		            SELECT DISTINCT s2.job AS job FROM shell_jobs s2 WHERE s2.agency = a.id ORDER BY s2.job LIMIT 5))
		  FROM agencies a JOIN shell_jobs sj ON sj.agency = a.id
		 WHERE NOT EXISTS (
		       SELECT 1 FROM runners rn JOIN runner_agencies ra ON ra.runner_id = rn.id
		        WHERE ra.agency_id = a.id
		          AND EXISTS (SELECT 1 FROM json_each(COALESCE(rn.capabilities, '[]')) c
		                       WHERE c.value IN `+shellTypesSQL+`))
		 GROUP BY a.id, a.name
		 ORDER BY a.name`, agencyid.Global)
	if err != nil {
		return err
	}
	var found []Finding
	for rows.Next() {
		var id, name string
		var n int
		var jobs sql.NullString
		if err := rows.Scan(&id, &name, &n, &jobs); err != nil {
			rows.Close()
			return err
		}
		list := jobs.String
		if n > 5 {
			list += fmt.Sprintf(", and %d more", n-5)
		}
		found = append(found, Finding{
			AgencyID: id,
			Subject:  id,
			Detail: fmt.Sprintf("%s has shell jobs (%s) and no runner that can run them: no agent of its own, and the "+
				"local runner does not serve it. Until 2.3.0 the server ran every agency's shell jobs; it now runs "+
				"those of the agencies on the local runner's list, so a run of these jobs waits. Either enrol an "+
				"agent for %s (Runners → Add Runner), or have a global administrator add %s to the agencies the "+
				"local runner serves (Settings → Local runner).", name, list, name, name),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	return Reconcile(ctx, database, KindNoRunnerForShellJobs, found)
}
