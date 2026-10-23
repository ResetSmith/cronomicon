package preflight

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"sort"
	"strings"
)

// The 2.3.0 section.
//
// 2.3.0 changes more on upgrade than any release before it, and almost all of
// it is decided by what the installation holds: which rows sit in several
// agencies, which runners serve more than one, where shell jobs ran from, and
// which hosts the server has a key for. 2.3.0 converted nothing by force
// (LR-16); 2.4.0 and 2.5.0 do (§0a): a repository and a row are one agency's,
// and what an agency had through Global's stops. Since 2.5.0 an installation
// goes from 2.2.3 to 2.5.0 in one step, so this section says, before the
// upgrade, what that one step changes, and the last block of it what stops at
// the first start (2.5.0 C11, present defect 81; until then it described 2.3.0
// alone and said that what it found "keeps working").
//
// Like the rest of the package it is read-only and reads the PREVIOUS
// release's schema (2.2.x, v1210): no Global agency row, "no agency" as the
// absence of membership rows, `ssh_hosts.host_key`, `jobs.executor` as a live
// choice. The command does not run it against a database already on the 2.3.0
// schema — there the findings are the app's Notices.

// shellTypes are the run types the server can run itself.
var shellTypes = []string{"bash", "perl", "powershell", "python"}

// SharedRow is a row that sits in several agencies today.
type SharedRow struct {
	Kind     string // scope | secret | variable | SSH key
	Name     string
	Agencies []string
}

// LegacyRunner is a runner that serves more than one agency today (MA-29).
type LegacyRunner struct {
	Name     string
	Agencies []string
}

// ShellScope is an unbound scope that has shell jobs, with where those jobs ran
// under 2.2 and who may take them under 2.3.0.
type ShellScope struct {
	Scope    string // "" for the jobs with no scope
	Agencies []string
	// FromServer and OnAgents say what the scope's shell jobs resolved to under
	// the 2.2 precedence (job executor, else the default setting, else ssh).
	FromServer, OnAgents bool
	Jobs                 int
	// Agents are the registered agents that serve one of the scope's agencies
	// and can run a shell type: under 2.3.0 any of them may take these jobs.
	Agents []string
}

// NamedJob is a job with one fact about it.
type NamedJob struct{ Job, Scope, Detail string }

// KeyConflict is one address that two records hold different host keys for.
type KeyConflict struct {
	Address string
	Records []string
}

// Report230 is what the upgrade to 2.3.0 changes for this installation.
type Report230 struct {
	Shared        []SharedRow
	LegacyRunners []LegacyRunner
	// PoolRunners are the runners in no agency: they become the Global
	// agency's, as do runs with no scope.
	PoolRunners []string
	ShellScopes []ShellScope
	// Unowned counts the secrets, variables and SSH keys with no owner that
	// sit in exactly one agency: the upgrade makes that agency their owner
	// where that changes nothing a run resolves.
	UnownedSecrets, UnownedVariables, UnownedKeys int
	RequiresJobs                                  []NamedJob
	TargetsOutsideScope                           []NamedJob
	HostsWithoutKey, BastionsWithoutKey           []string
	KeyConflicts                                  []KeyConflict
	ExecutorJobs, ExecutorScripts                 int
	VaultSecrets                                  []VaultSecret
	VaultKeys                                     []SharedRow
	// NamedGlobal are the agencies already called "Global" (in any letter
	// case): migration 1220 renames each to "<name> (renamed)". RenameBlocked
	// are those whose new name is already taken, for which the migration
	// refuses to run.
	NamedGlobal, RenameBlocked []string

	// From 2.4.0 and 2.5.0, read from the 2.2 schema (C11, present defect 81):
	// what stops or moves at the first start.
	//
	// GitJobsOnAgencyScopes are the Git jobs whose scope is one agency's: every
	// Git job is Global's repository's, and from 2.4.0 a job of Global's
	// repository runs on Global's scopes only, so each of their runs is refused
	// (runref.RepoScopeMismatch; the notice global_job_on_agency_scope).
	GitJobsOnAgencyScopes []NamedJob
	// AgencyGitScopes are the Git scopes of one agency: 1470 makes each that
	// agency's own, in the app, with its hosts, and its file in Global's
	// repository makes no scope any more.
	AgencyGitScopes []SharedRow
	// GlobalHostRecords are the hand-written host records (Global's since
	// 1230) whose host a scope of one agency lists: from 2.4.0 a record of
	// Global's is used by Global's runs only (the notice global_host_record).
	GlobalHostRecords []SharedRow
	// GlobalCalendars are the calendars that froze every agency: 1430 gives
	// each agency a copy, which freezes that agency; Global's then freezes
	// Global only.
	GlobalCalendars []string
	// QueuedSSHRuns are the runs still queued for the SSH executor: 1590
	// closes them as skipped (present defect 80).
	QueuedSSHRuns int
	// AgencyGitRuns count, per agency, the runs of Git jobs recorded for that
	// agency alone: from 2.4.0 a run is its job's agency's (1480), and every
	// 2.2 Git job is Global's, so they become Global's history (question 104).
	AgencyGitRuns []AgencyCount
	// Agencies and AgencyGrants: each agency needs a repository of its own
	// (agency_no_repository), and each grant of an agency names a group that
	// no agency's group list holds yet (grant_group_unlisted).
	Agencies, AgencyGrants int
}

// AgencyCount is a number of rows of one agency.
type AgencyCount struct {
	Agency string
	N      int
}

func names(ctx context.Context, db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func count(ctx context.Context, db *sql.DB, query string, args ...any) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// grouped runs a query of (name, agency name) rows ordered by name and folds
// each name's agencies together.
func grouped(ctx context.Context, db *sql.DB, kind, query string) ([]SharedRow, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedRow
	for rows.Next() {
		var name, agency string
		if err := rows.Scan(&name, &agency); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Name == name {
			out[n-1].Agencies = append(out[n-1].Agencies, agency)
			continue
		}
		out = append(out, SharedRow{Kind: kind, Name: name, Agencies: []string{agency}})
	}
	return out, rows.Err()
}

// severalSQL lists the rows of one kind held by more than one agency, as
// (display name, agency name), by the membership table's own columns.
func severalSQL(display, table, id, members, fk string) string {
	return `SELECT ` + display + `, COALESCE(a.name, m.agency_id)
	          FROM ` + table + ` t
	          JOIN ` + members + ` m ON m.` + fk + ` = t.` + id + `
	          LEFT JOIN agencies a ON a.id = m.agency_id
	         WHERE (SELECT COUNT(*) FROM ` + members + ` m2 WHERE m2.` + fk + ` = t.` + id + `) > 1
	         ORDER BY 1, 2`
}

const scopedName = `t.key || CASE WHEN COALESCE(t.scope, '') = '' THEN '' ELSE ' (scope ' || t.scope || ')' END`

// Build230 reads the 2.3.0 section from a 2.2.x database.
func Build230(ctx context.Context, db *sql.DB) (Report230, error) {
	var rep Report230
	fail := func(what string, err error) (Report230, error) {
		return rep, fmt.Errorf("read %s: %w", what, err)
	}

	for _, k := range []struct{ kind, sql string }{
		{"scope", severalSQL(`t.name`, "scopes", "id", "scope_agencies", "scope_id")},
		{"secret", severalSQL(scopedName, "secrets", "id", "secret_agencies", "secret_id")},
		{"variable", severalSQL(scopedName, "env_vars", "id", "env_var_agencies", "env_var_id")},
		{"SSH key", severalSQL(`t.label`, "ssh_credentials", "id", "ssh_credential_agencies", "credential_id")},
	} {
		got, err := grouped(ctx, db, k.kind, k.sql)
		if err != nil {
			return fail(k.kind+"s in several agencies", err)
		}
		rep.Shared = append(rep.Shared, got...)
	}

	legacy, err := grouped(ctx, db, "runner", severalSQL(`t.name`, "runners", "id", "runner_agencies", "runner_id"))
	if err != nil {
		return fail("runners", err)
	}
	for _, l := range legacy {
		rep.LegacyRunners = append(rep.LegacyRunners, LegacyRunner{Name: l.Name, Agencies: l.Agencies})
	}
	if rep.PoolRunners, err = names(ctx, db, `
		SELECT name FROM runners r
		 WHERE NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = r.id)
		 ORDER BY name`); err != nil {
		return fail("runners", err)
	}

	if rep.ShellScopes, err = shellScopes(ctx, db); err != nil {
		return fail("shell jobs", err)
	}

	for _, u := range []struct {
		into                 *int
		table, members, fkey string
	}{
		{&rep.UnownedSecrets, "secrets", "secret_agencies", "secret_id"},
		{&rep.UnownedVariables, "env_vars", "env_var_agencies", "env_var_id"},
		{&rep.UnownedKeys, "ssh_credentials", "ssh_credential_agencies", "credential_id"},
	} {
		if *u.into, err = count(ctx, db, `
			SELECT COUNT(*) FROM `+u.table+` t
			 WHERE COALESCE(t.owner_agency, '') = ''
			   AND (SELECT COUNT(*) FROM `+u.members+` m WHERE m.`+u.fkey+` = t.id) = 1`); err != nil {
			return fail(u.table, err)
		}
	}

	// A job's `requires` was already applied to every claim an AGENT made. The
	// SSH executor ignored it, so what changes is a shell job that ran from the
	// server: by the 2.2 precedence, its own executor, else the default setting,
	// else ssh, and never on a scope bound to runners.
	fallback, err := defaultExecutor22(ctx, db)
	if err != nil {
		return fail("settings", err)
	}
	in := "'" + strings.Join(shellTypes, "','") + "'"
	//nolint:gosec // `in` is the package's own list of shell run types, not input
	if rep.RequiresJobs, err = jobsArgs(ctx, db, `
		SELECT j.name, COALESCE(j.scope, ''), j.requires_json
		  FROM jobs j LEFT JOIN scopes sc ON sc.name = j.scope
		 WHERE j.deleted_at IS NULL AND j.run_type IN (`+in+`)
		   AND TRIM(COALESCE(j.requires_json, '')) NOT IN ('', '[]', 'null')
		   AND NOT EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id)
		   AND NOT (j.source = 'git' AND sc.id IS NOT NULL AND `+oneAgencyScope+`) -- runs nowhere from 2.4.0 (build24)
		   AND CASE WHEN COALESCE(j.executor, '') IN ('ssh', 'runner') THEN j.executor ELSE ? END = 'ssh'
		 ORDER BY j.name`, fallback); err != nil {
		return fail("job requirements", err)
	}
	// A fixed target host is checked against its scope only when the scope
	// lists hosts: a scope whose inventory is the agent's own lists none.
	if rep.TargetsOutsideScope, err = jobs(ctx, db, `
		SELECT j.name, j.scope, j.target_host
		  FROM jobs j JOIN scopes sc ON sc.name = j.scope
		 WHERE j.deleted_at IS NULL AND TRIM(COALESCE(j.target_host, '')) <> ''
		   AND EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id)
		   AND NOT EXISTS (SELECT 1 FROM scope_hosts sh WHERE sh.scope_id = sc.id AND sh.host = j.target_host)
		 ORDER BY j.name`); err != nil {
		return fail("job targets", err)
	}

	// The address a record is dialled at, as the server will trust it: one key
	// per address and key type, and a port of 0 is 22. A record with no key of
	// its own is not listed when another record's key already covers its
	// address — the carried key will serve both.
	const at = `COALESCE(NULLIF(TRIM(address), ''), hostname) || ':' || CASE WHEN COALESCE(port, 0) = 0 THEN 22 ELSE port END`
	const keyed = `(SELECT ` + at + ` AS addr FROM ssh_hosts WHERE TRIM(COALESCE(host_key, '')) <> ''
	                UNION SELECT ` + at + ` FROM bastions WHERE TRIM(COALESCE(host_key, '')) <> '')`
	if rep.HostsWithoutKey, err = names(ctx, db, `
		SELECT hostname || ' (`+"' || "+at+" || '"+`)' FROM ssh_hosts
		 WHERE TRIM(COALESCE(host_key, '')) = ''
		   AND `+at+` NOT IN (SELECT addr FROM `+keyed+`)
		 ORDER BY hostname`); err != nil {
		return fail("host records", err)
	}
	if rep.BastionsWithoutKey, err = names(ctx, db, `
		SELECT COALESCE(NULLIF(name, ''), hostname) || ' (`+"' || "+at+" || '"+`)' FROM bastions
		 WHERE TRIM(COALESCE(host_key, '')) = ''
		   AND `+at+` NOT IN (SELECT addr FROM `+keyed+`)
		 ORDER BY 1`); err != nil {
		return fail("bastions", err)
	}
	if rep.KeyConflicts, err = keyConflicts(ctx, db, at); err != nil {
		return fail("host keys", err)
	}

	if rep.ExecutorJobs, err = count(ctx, db, `
		SELECT COUNT(*) FROM jobs
		 WHERE source = 'git' AND deleted_at IS NULL AND COALESCE(executor, '') <> ''`); err != nil {
		return fail("jobs", err)
	}
	if rep.ExecutorScripts, err = count(ctx, db,
		`SELECT COUNT(*) FROM scripts WHERE COALESCE(executor, '') <> ''`); err != nil {
		return fail("scripts", err)
	}

	if rep.NamedGlobal, err = names(ctx, db,
		`SELECT name FROM agencies WHERE LOWER(name) = 'global' ORDER BY name`); err != nil {
		return fail("agencies", err)
	}
	if rep.RenameBlocked, err = names(ctx, db, `
		SELECT a.name FROM agencies a
		 WHERE LOWER(a.name) = 'global'
		   AND EXISTS (SELECT 1 FROM agencies b WHERE b.name = a.name || ' (renamed)')
		 ORDER BY a.name`); err != nil {
		return fail("agencies", err)
	}

	if rep.VaultSecrets, err = vaultSecrets(ctx, db); err != nil {
		return fail("secrets", err)
	}
	if rep.VaultKeys, err = grouped(ctx, db, "SSH key", `
		SELECT t.label, COALESCE(a.name, m.agency_id)
		  FROM ssh_credentials t
		  JOIN ssh_credential_agencies m ON m.credential_id = t.id
		  LEFT JOIN agencies a ON a.id = m.agency_id
		 WHERE t.source = 'vault'
		 ORDER BY 1, 2`); err != nil {
		return fail("SSH keys", err)
	}
	if err := build24(ctx, db, &rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// oneAgencyScope is the condition that the scope `sc` is in exactly one
// agency (2.2 schema): the scopes 2.3.0 gives that agency.
const oneAgencyScope = `(SELECT COUNT(*) FROM scope_agencies s1 WHERE s1.scope_id = sc.id) = 1`

// build24 reads what 2.4.0 and 2.5.0 stop or move at the first start.
func build24(ctx context.Context, db *sql.DB, rep *Report230) error {
	fail := func(what string, err error) error { return fmt.Errorf("read %s: %w", what, err) }
	var err error
	if rep.GitJobsOnAgencyScopes, err = jobs(ctx, db, `
		SELECT j.name, j.scope, COALESCE(a.name, sa.agency_id)
		  FROM jobs j JOIN scopes sc ON sc.name = j.scope
		  JOIN scope_agencies sa ON sa.scope_id = sc.id
		  LEFT JOIN agencies a ON a.id = sa.agency_id
		 WHERE j.source = 'git' AND j.deleted_at IS NULL AND `+oneAgencyScope+`
		 ORDER BY 3, 1`); err != nil {
		return fail("Git jobs", err)
	}
	if rep.AgencyGitScopes, err = grouped(ctx, db, "scope", `
		SELECT sc.name, COALESCE(a.name, sa.agency_id)
		  FROM scopes sc JOIN scope_agencies sa ON sa.scope_id = sc.id
		  LEFT JOIN agencies a ON a.id = sa.agency_id
		 WHERE sc.source = 'git' AND `+oneAgencyScope+`
		 ORDER BY 1, 2`); err != nil {
		return fail("Git scopes", err)
	}
	if rep.GlobalHostRecords, err = grouped(ctx, db, "host", `
		SELECT DISTINCT h.hostname, COALESCE(a.name, sa.agency_id)
		  FROM ssh_hosts h
		  JOIN scope_hosts sh ON sh.host = h.hostname
		  JOIN scopes sc ON sc.id = sh.scope_id
		  JOIN scope_agencies sa ON sa.scope_id = sc.id
		  LEFT JOIN agencies a ON a.id = sa.agency_id
		 WHERE COALESCE(h.scope_id, '') = '' AND `+oneAgencyScope+`
		 ORDER BY 1, 2`); err != nil {
		return fail("host records", err)
	}
	if rep.GlobalCalendars, err = names(ctx, db, `SELECT name FROM calendars WHERE global = 1 ORDER BY name`); err != nil {
		return fail("calendars", err)
	}
	if rep.QueuedSSHRuns, err = count(ctx, db, `SELECT COUNT(*) FROM runs WHERE status = 'queued' AND executor = 'ssh'`); err != nil {
		return fail("queued runs", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT json_extract(agencies_json, '$[0]'), COUNT(*) FROM runs
		 WHERE COALESCE(job_source, 'git') = 'git'
		   AND json_valid(agencies_json) AND json_array_length(agencies_json) = 1
		 GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return fail("runs", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c AgencyCount
		if err := rows.Scan(&c.Agency, &c.N); err != nil {
			return fail("runs", err)
		}
		rep.AgencyGitRuns = append(rep.AgencyGitRuns, c)
	}
	if err := rows.Err(); err != nil {
		return fail("runs", err)
	}
	if rep.Agencies, err = count(ctx, db, `SELECT COUNT(*) FROM agencies`); err != nil {
		return fail("agencies", err)
	}
	if rep.AgencyGrants, err = count(ctx, db, `SELECT COUNT(*) FROM access_grants WHERE agency_id IS NOT NULL`); err != nil {
		return fail("grants", err)
	}
	return nil
}

func jobs(ctx context.Context, db *sql.DB, query string) ([]NamedJob, error) {
	return jobsArgs(ctx, db, query)
}

func jobsArgs(ctx context.Context, db *sql.DB, query string, args ...any) ([]NamedJob, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamedJob
	for rows.Next() {
		var j NamedJob
		if err := rows.Scan(&j.Job, &j.Scope, &j.Detail); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// keyConflicts lists the addresses for which host records and bastions hold
// different keys. 2.3.0's local runner trusts one key per address and key
// type; the upgrade carries one and reports the other.
func keyConflicts(ctx context.Context, db *sql.DB, at string) ([]KeyConflict, error) {
	//nolint:gosec // `at` is a constant expression of this package, not input
	rows, err := db.QueryContext(ctx, `
		WITH raw AS (
			SELECT `+at+` AS addr, 'host ' || hostname AS record, TRIM(host_key) AS k
			  FROM ssh_hosts WHERE TRIM(COALESCE(host_key, '')) <> ''
			UNION ALL
			SELECT `+at+`, 'bastion ' || COALESCE(NULLIF(name, ''), hostname), TRIM(host_key)
			  FROM bastions WHERE TRIM(COALESCE(host_key, '')) <> ''
		),
		-- a stored key is "<type> <base64> [comment]": two records conflict
		-- when they hold different keys of the SAME type for one address. Keys
		-- of different types are both carried.
		held AS (
			SELECT addr, record, k,
			       CASE WHEN INSTR(k, ' ') > 0 THEN SUBSTR(k, 1, INSTR(k, ' ') - 1) ELSE k END AS ktype
			  FROM raw
		)
		SELECT DISTINCT h.addr, h.record FROM held h
		 WHERE (SELECT COUNT(DISTINCT h2.k) FROM held h2 WHERE h2.addr = h.addr AND h2.ktype = h.ktype) > 1
		 ORDER BY h.addr, h.record`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyConflict
	for rows.Next() {
		var addr, record string
		if err := rows.Scan(&addr, &record); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Address == addr {
			out[n-1].Records = append(out[n-1].Records, record)
			continue
		}
		out = append(out, KeyConflict{Address: addr, Records: []string{record}})
	}
	return out, rows.Err()
}

// defaultExecutor22 is what a shell job with no executor of its own resolved
// to under 2.2: the `defaultExecutor` setting when it names one, else ssh. It
// reads the stored value as the upgrade pass does.
func defaultExecutor22(ctx context.Context, db *sql.DB) (string, error) {
	var def sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'defaultExecutor'`).Scan(&def); err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if def.String == "ssh" || def.String == "runner" {
		return def.String, nil
	}
	return "ssh", nil
}

// shellScopes classifies every unbound scope that has shell jobs, and the jobs
// with no scope, by where those jobs ran under the 2.2 precedence, and names
// the agents that may take them under 2.3.0. It is the pre-upgrade twin of the
// classification the upgrade pass makes at first boot.
func shellScopes(ctx context.Context, db *sql.DB) ([]ShellScope, error) {
	fallback, err := defaultExecutor22(ctx, db)
	if err != nil {
		return nil, err
	}

	in := "'" + strings.Join(shellTypes, "','") + "'"
	//nolint:gosec // `in` is the package's own list of shell run types, not input
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(sc.id, ''), COALESCE(j.scope, ''), COALESCE(j.executor, '')
		  FROM jobs j LEFT JOIN scopes sc ON sc.name = j.scope
		 WHERE j.deleted_at IS NULL AND j.run_type IN (`+in+`)
		   AND NOT EXISTS (SELECT 1 FROM scope_runners sr WHERE sr.scope_id = sc.id)
		   -- A Git job on one agency's scope runs nowhere from 2.4.0 (build24).
		   AND NOT (j.source = 'git' AND sc.id IS NOT NULL AND `+oneAgencyScope+`)
		 ORDER BY 2`)
	if err != nil {
		return nil, err
	}
	type key struct{ id, name string }
	byKey := map[key]*ShellScope{}
	var order []key
	for rows.Next() {
		var id, name, executor string
		if err := rows.Scan(&id, &name, &executor); err != nil {
			rows.Close()
			return nil, err
		}
		if name != "" && id == "" {
			continue // a job naming a scope that does not exist runs nowhere
		}
		k := key{id, name}
		s := byKey[k]
		if s == nil {
			s = &ShellScope{Scope: name}
			byKey[k] = s
			order = append(order, k)
		}
		s.Jobs++
		if executor != "ssh" && executor != "runner" {
			executor = fallback
		}
		if executor == "ssh" {
			s.FromServer = true
		} else {
			s.OnAgents = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A runner "can run a shell type" by what it declared; the operator's
	// capability mask is not read here, so this may name an agent that has
	// been masked off shell work.
	shellCap := `EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(r.capabilities) THEN r.capabilities ELSE '[]' END) c
	                      WHERE c.value IN (` + in + `))`
	out := make([]ShellScope, 0, len(order))
	for _, k := range order {
		s := byKey[k]
		if k.id == "" {
			// No scope: the general pool under 2.2, Global under 2.3.0.
			s.Agencies = []string{"Global"}
			if s.Agents, err = names(ctx, db, `
				SELECT r.name FROM runners r
				 WHERE NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = r.id) AND `+shellCap+`
				 ORDER BY r.name`); err != nil {
				return nil, err
			}
			out = append(out, *s)
			continue
		}
		if s.Agencies, err = names(ctx, db, `
			SELECT CASE WHEN LOWER(a.name) = 'global' THEN a.name || ' (renamed)' ELSE COALESCE(a.name, sa.agency_id) END
			  FROM scope_agencies sa LEFT JOIN agencies a ON a.id = sa.agency_id
			 WHERE sa.scope_id = ? ORDER BY 1`, k.id); err != nil {
			return nil, err
		}
		switch {
		case len(s.Agencies) == 0:
			s.Agencies = []string{"Global"}
			s.Agents, err = names(ctx, db, `
				SELECT r.name FROM runners r
				 WHERE NOT EXISTS (SELECT 1 FROM runner_agencies ra WHERE ra.runner_id = r.id) AND `+shellCap+`
				 ORDER BY r.name`)
		case len(s.Agencies) == 1:
			// One agency's scope: its own agents, those that serve it alone
			// (from 2.4.0 a runner that served several is Global's, and takes
			// Global's runs only).
			s.Agents, err = names(ctx, db, `
				SELECT DISTINCT r.name FROM runners r
				  JOIN runner_agencies ra ON ra.runner_id = r.id
				  JOIN scope_agencies sa ON sa.agency_id = ra.agency_id AND sa.scope_id = ?
				 WHERE `+shellCap+`
				   AND (SELECT COUNT(*) FROM runner_agencies r2 WHERE r2.runner_id = r.id) = 1
				 ORDER BY r.name`, k.id)
		default:
			// A scope of several agencies is Global's (1410): Global's runners,
			// those of no agency and those that served several.
			s.Agents, err = names(ctx, db, `
				SELECT r.name FROM runners r
				 WHERE (SELECT COUNT(*) FROM runner_agencies ra WHERE ra.runner_id = r.id) <> 1 AND `+shellCap+`
				 ORDER BY r.name`)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

// Placed are the agencies the local runner serves after the upgrade: Global
// alone. Since 2.4.0 the local runner is Global's and serves no other agency
// (§0a, IN-3); until then the 2.3.0 upgrade pass placed it in every agency
// whose shell jobs had run from the server, and that pass was retired in 2.5.0.
func (r Report230) Placed() []string { return []string{"Global"} }

// NeedAgents are the agencies other than Global whose shell jobs ran from the
// server: after the upgrade the local runner does not serve them, so their runs
// wait for a runner of their own.
func (r Report230) NeedAgents() []string {
	seen := map[string]bool{}
	for _, s := range r.ShellScopes {
		if !s.FromServer {
			continue
		}
		for _, a := range s.Agencies {
			if a != "Global" {
				seen[a] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func scopeLabel(s string) string {
	if s == "" {
		return "jobs with no scope"
	}
	return "scope " + s
}

// listed prints up to max items, one per line, and counts the rest.
func listed(p func(string, ...any), items []string, max int) {
	for i, it := range items {
		if i == max {
			p("    … and %d more", len(items)-max)
			return
		}
		p("    %s", it)
	}
}

// Write renders the section as plain text. sshExecutor is what
// CRONOMICON_SSH_EXECUTOR_ENABLED says in the environment the command ran in,
// or nil when that could not be read: the upgrade pass acts on it, and this
// command may be run somewhere other than where the server runs.
func (r Report230) Write(w io.Writer, sshExecutor *bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	p("Cronomicon 2.3.0 to 2.5.0 — what this upgrade changes for this installation")
	p("")
	p("Take a backup immediately before the first start of the new release (the `VACUUM")
	p("INTO` snapshot or the S3 backup). Rolling back is restoring it under the 2.2.x")
	p("binary; runs made in between are lost. The first start signs every user out, once:")
	p("from 2.3.0 a change to access grants takes effect at once, not at the next sign-in.")
	p("")
	p("The upgrade is one step to 2.5.0. The sections below say what 2.3.0 changes, and")
	p("the last one what 2.4.0 and 2.5.0 stop or move at the first start. Lines marked !!")
	p("need action before the upgrade. After it, the app lists each item under Notices.")

	p("")
	p("Agencies")
	p("")
	p("  \"No agency\" becomes a real agency, Global. From 2.3.0 a scope, secret,")
	p("  variable, SSH key, host record or runner is created in exactly one agency; a row")
	p("  in Global is every agency's to use and a global administrator's to change.")
	p("")
	for _, n := range r.RenameBlocked {
		p("  !! The agency %q cannot be renamed: \"%s (renamed)\" already exists, and the", n, n)
		p("     upgrade REFUSES TO RUN until one of the two has another name. Rename it first.")
	}
	if len(r.NamedGlobal) > 0 {
		p("  !! An agency is already called Global: %s. The upgrade renames it", strings.Join(r.NamedGlobal, ", "))
		p("     \"<name> (renamed)\", in the catalog and on its waiting runs, and a notice says")
		p("     so. Rename it yourself beforehand to choose the name. (Below, it is shown under")
		p("     its new name; \"Global\" means the built-in agency.)")
		p("")
	}
	if len(r.Shared) == 0 {
		p("  No scope, secret, variable or SSH key is in several agencies.")
	} else {
		p("  Rows in several agencies (%d). Each becomes Global's and keeps serving the agencies", len(r.Shared))
		p("  it is in until it is given to one (a scope's runs are then Global's, taken by")
		p("  Global's runners, and refuse a secret, variable or key name two of its agencies")
		p("  both hold). Give each to one agency afterwards; if several agencies run against")
		p("  the same hosts, give each a scope of its own.")
		for _, s := range r.Shared {
			p("    %s %s — %s", s.Kind, s.Name, strings.Join(s.Agencies, ", "))
		}
	}
	if n := r.UnownedSecrets + r.UnownedVariables + r.UnownedKeys; n > 0 {
		p("")
		p("  %d secret(s), %d variable(s) and %d SSH key(s) have no owner and are in exactly", r.UnownedSecrets, r.UnownedVariables, r.UnownedKeys)
		p("  one agency. The upgrade makes that agency their owner, except where doing so")
		p("  would change which row a run resolves; those stay Global's and resolve as before.")
	}

	p("")
	p("Runners")
	p("")
	p("  From 2.3.0 an agent serves exactly one agency: its owner. An agency's")
	p("  administrators enrol and manage their own agents.")
	p("")
	if len(r.LegacyRunners) == 0 {
		p("  No runner serves more than one agency.")
	} else {
		p("  !! Runners that serve several agencies (%d). Each becomes Global's and, from 2.4.0,", len(r.LegacyRunners))
		p("  takes Global's runs only: the runs of the other agencies on its list wait for an")
		p("  agent of their own. It can be narrowed but never widened. To settle one, in order:")
		p("  enrol an agent for each agency; re-bind each of that agency's scopes to its new")
		p("  agent; copy the approved host keys for that agency's hosts to it; then narrow or")
		p("  deregister the old runner. A second agent can run on the same machine, under an")
		p("  OS user of its own: fill in Instance name in the install helper (--instance).")
		for _, l := range r.LegacyRunners {
			p("    %s — %s", l.Name, strings.Join(l.Agencies, ", "))
		}
	}
	if len(r.PoolRunners) > 0 {
		p("")
		p("  Runners in no agency (%d) become Global's and take Global's runs: jobs with no", len(r.PoolRunners))
		p("  scope, and scopes in no agency.")
		listed(p, r.PoolRunners, 20)
	}

	p("")
	p("Where shell jobs run")
	p("")
	p("  The SSH executor becomes the local runner: the server, listed with the other")
	p("  runners. Nothing chooses an executor any more — not a job, a script, a setting")
	p("  or a run. A run is taken by whichever runner asks first that serves its scope's")
	p("  agency, is bound to the scope if the scope names runners, and can run its type.")
	p("  The local runner runs bash, perl, powershell and python only.")
	p("")
	switch {
	case sshExecutor == nil:
		p("  CRONOMICON_SSH_EXECUTOR_ENABLED could not be read here. If it is true where the")
		p("  server runs, read the lines below as \"the local runner WILL be turned on\".")
	case *sshExecutor:
		p("  CRONOMICON_SSH_EXECUTOR_ENABLED is true here: at its first start 2.3.0 turns the")
		p("  local runner on. After that the switch is Settings → Local runner, not the")
		p("  environment.")
	default:
		p("  CRONOMICON_SSH_EXECUTOR_ENABLED is not true here: the local runner starts off,")
		p("  and is turned on under Settings → Local runner by a global administrator.")
	}
	if sshExecutor == nil || *sshExecutor {
		p("  It will serve: %s. The local runner is Global's and serves no other agency", strings.Join(r.Placed(), ", "))
		p("  (from 2.4.0).")
		if need := r.NeedAgents(); len(need) > 0 {
			p("  The shell jobs of %s ran from the server: after the upgrade they wait", strings.Join(need, ", "))
			p("  for a runner of their own agency. Enrol an agent for each before upgrading, or")
			p("  their runs queue.")
		}
	}
	var toAgent, toServer []ShellScope
	placed := map[string]bool{}
	for _, a := range r.Placed() {
		placed[a] = true
	}
	for _, s := range r.ShellScopes {
		if s.FromServer && len(s.Agents) > 0 {
			toAgent = append(toAgent, s)
		}
		if s.OnAgents && (sshExecutor == nil || *sshExecutor) {
			for _, a := range s.Agencies {
				if placed[a] {
					toServer = append(toServer, s)
					break
				}
			}
		}
	}
	p("")
	if len(toAgent) == 0 {
		p("  No shell job that ran from the server can be taken by an agent.")
	} else {
		p("  Shell jobs that ran from the server and may now be taken by an AGENT (%d", len(toAgent))
		p("  place(s)). This holds whether or not the SSH executor was on: with it off these")
		p("  jobs were not running, and an agent may now run them. To keep a scope's jobs on")
		p("  the server, bind the scope to the local runner after the upgrade; to keep them")
		p("  on particular agents, bind it to those (Scopes → Runners).")
		for _, s := range toAgent {
			p("    %s (%s), %d shell job(s) — %s", scopeLabel(s.Scope), strings.Join(s.Agencies, ", "), s.Jobs, strings.Join(s.Agents, ", "))
		}
	}
	if len(toServer) > 0 {
		p("")
		p("  Shell jobs that ran on agents and may now be taken by the SERVER (%d place(s)),", len(toServer))
		p("  because the local runner will serve their agency. Bind the scope to its agents")
		p("  to keep them there.")
		for _, s := range toServer {
			p("    %s (%s), %d shell job(s)", scopeLabel(s.Scope), strings.Join(s.Agencies, ", "), s.Jobs)
		}
	}
	if len(r.RequiresJobs) > 0 {
		p("")
		p("  Shell jobs that ran from the server and declare `requires` (%d). The SSH executor", len(r.RequiresJobs))
		p("  ignored requirements; from 2.3.0 they are read for every run, and the local")
		p("  runner satisfies none. Each of these waits for an agent that advertises what it")
		p("  asks:")
		for _, j := range r.RequiresJobs {
			p("    %s (%s) requires %s", j.Job, scopeLabel(j.Scope), j.Detail)
		}
	}
	if r.ExecutorJobs+r.ExecutorScripts > 0 {
		p("")
		p("  %d job(s) and %d script(s) synced from Git declare `executor`. The key is ignored", r.ExecutorJobs, r.ExecutorScripts)
		p("  from 2.3.0, with a warning from `cronomicon validate` and the sync log; a notice")
		p("  lists the jobs. Remove the line when convenient.")
	}

	p("")
	p("Host keys")
	p("")
	p("  From 2.3.0 the server trusts a host only by a key an operator approved, as agents")
	p("  always have. It no longer captures a key the first time it connects, in a run or")
	p("  in Test connection. The keys it already holds are carried over as approved.")
	p("")
	if len(r.HostsWithoutKey)+len(r.BastionsWithoutKey) == 0 {
		p("  Every host record and bastion has a stored key.")
	} else {
		p("  Records with no stored key (%d host(s), %d bastion(s)). If the local runner", len(r.HostsWithoutKey), len(r.BastionsWithoutKey))
		p("  takes runs for them, those runs fail (host_key_unverified) until a key is")
		p("  approved: Runners → Local runner → Trusted host keys → Scan keys. A host behind")
		p("  a bastion is not scanned; paste its key.")
		listed(p, r.HostsWithoutKey, 30)
		for _, b := range r.BastionsWithoutKey {
			p("    bastion %s", b)
		}
	}
	if len(r.KeyConflicts) > 0 {
		p("")
		p("  Addresses with two different stored keys of one type (%d). The server will trust", len(r.KeyConflicts))
		p("  ONE key per address and key type; the other record's connections fail as a")
		p("  mismatch until its key is the approved one. Two machines that share an address")
		p("  behind different bastions cannot both be reached from the server: use an agent")
		p("  for one.")
		for _, c := range r.KeyConflicts {
			p("    %s — %s", c.Address, strings.Join(c.Records, "; "))
		}
	}

	p("")
	p("Vault and targets")
	p("")
	if len(r.VaultSecrets)+len(r.VaultKeys) == 0 {
		p("  No agency holds a Vault-backed secret or SSH key.")
	} else {
		p("  Vault-backed rows held by an agency (%d secret(s), %d SSH key(s)). From 2.3.0 an", len(r.VaultSecrets), len(r.VaultKeys))
		p("  agency's administrators manage their own Vault references, inside the Vault")
		p("  path prefixes a global administrator gives the agency (Scopes → Agencies).")
		p("  Assign each of these agencies a prefix after the upgrade; a reference outside")
		p("  its agency's prefixes keeps resolving and is listed by a notice.")
		for _, s := range r.VaultSecrets {
			p("    secret %s — %s", s.Key, strings.Join(s.Agencies, ", "))
		}
		for _, k := range r.VaultKeys {
			p("    SSH key %s — %s", k.Name, strings.Join(k.Agencies, ", "))
		}
	}
	if len(r.TargetsOutsideScope) > 0 {
		p("")
		p("  !! Jobs whose fixed target host is not in their scope (%d). From 2.3.0 a job runs", len(r.TargetsOutsideScope))
		p("     only against hosts of its own scope: such a job cannot be started by hand or")
		p("     by a token, and a scheduled run fails for that host. Fix the job's target, or")
		p("     add the host to the scope, BEFORE upgrading. A notice lists them afterwards.")
		for _, j := range r.TargetsOutsideScope {
			p("    %s — %s is not a host of scope %s", j.Job, j.Detail, j.Scope)
		}
	}
	r.write24(p)
}

// write24 is the last block: what 2.4.0 and 2.5.0 stop or move at the first
// start (C11, present defect 81).
func (r Report230) write24(p func(string, ...any)) {
	p("")
	p("What stops or moves at the first start (2.4.0 and 2.5.0)")
	p("")
	p("  From 2.4.0 every agency has a repository of its own, and a row is one agency's:")
	p("  what an agency had through Global's (its Git jobs, Git scopes, hand-written host")
	p("  records) is Global's, and Global's serves Global's runs only.")
	if len(r.GitJobsOnAgencyScopes) > 0 {
		p("")
		p("  !! Git jobs on an agency's scope (%d) STOP RUNNING: every Git job is Global's", len(r.GitJobsOnAgencyScopes))
		p("     repository's, and from 2.4.0 a job of Global's repository runs on Global's")
		p("     scopes only, so each of their runs (scheduled, by hand, by a token) is refused,")
		p("     with the reason. Give each agency its repository and move these files there")
		p("     (or, after the upgrade, Give to an agency… on the job). The notice")
		p("     global_job_on_agency_scope lists them.")
		for _, j := range r.GitJobsOnAgencyScopes {
			p("    %s — scope %s (%s)", j.Job, j.Scope, j.Detail)
		}
	}
	if len(r.AgencyGitScopes) > 0 {
		p("")
		p("  Git scopes of one agency (%d) become that agency's own, built in the app, with", len(r.AgencyGitScopes))
		p("  their hosts, groups, variables and runner bindings. Their files in Global's")
		p("  repository make no scope any more: remove them, or move them to the agency's")
		p("  repository (which then defines the scope again).")
		for _, sc := range r.AgencyGitScopes {
			p("    scope %s — %s", sc.Name, strings.Join(sc.Agencies, ", "))
		}
	}
	if len(r.GlobalHostRecords) > 0 {
		p("")
		p("  !! Hand-written host records that an agency's scope uses (%d) are Global's, and", len(r.GlobalHostRecords))
		p("     from 2.4.0 a record of Global's is used by Global's runs only: that agency's")
		p("     runs on the host find no record and fail there. Give the agency a record of")
		p("     its own (by hand, or imported with the scope's inventory). The notice")
		p("     global_host_record lists them.")
		for _, h := range r.GlobalHostRecords {
			p("    %s — %s", h.Name, strings.Join(h.Agencies, ", "))
		}
	}
	if len(r.GlobalCalendars) > 0 {
		p("")
		p("  Calendars that froze every agency (%d): each agency gets a copy of its own, which", len(r.GlobalCalendars))
		p("  freezes that agency; Global's then freezes Global only. A change to Global's")
		p("  calendar no longer reaches the agencies: edit each copy, or use the")
		p("  installation-wide freeze in Settings.")
		listed(p, r.GlobalCalendars, 20)
	}
	if r.QueuedSSHRuns > 0 {
		p("")
		p("  !! Runs queued for the SSH executor (%d) are closed as skipped, with the reason:", r.QueuedSSHRuns)
		p("  nothing has claimed them since 2.3.0. Let them run, or cancel them, before you")
		p("  stop the old server, or run their jobs again after the upgrade.")
	}
	if len(r.AgencyGitRuns) > 0 {
		p("")
		p("  The run history of Git jobs becomes Global's, with the activity of those runs:")
		p("  a run is its job's agency's from 2.4.0, and every 2.2 Git job is Global's. An")
		p("  agency's people no longer see their own history of these runs (a global")
		p("  administrator and a viewer of every agency do). Runs of jobs built in the app")
		p("  stay their agency's.")
		for _, c := range r.AgencyGitRuns {
			p("    %s — %d run(s)", c.Agency, c.N)
		}
	}
	if r.Agencies > 0 {
		p("")
		p("  Each agency (%d) needs a repository of its own (Settings → Repositories); until", r.Agencies)
		p("  it has one it works as before and cannot be given a definition.")
	}
	if r.AgencyGrants > 0 {
		p("")
		p("  Grants of an agency (%d) name groups that no agency's group list holds yet: they", r.AgencyGrants)
		p("  work as before, and an agency's administrators can change them only once a")
		p("  global administrator lists the groups (Scopes → Agencies).")
	}
}
