package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ResetSmith/cronomicon/internal/auth"
)

// The RB-4 pre-flight report (the rbac-update plan Phase 0).
//
// Departmental RBAC narrowed access across two releases (RB-2/RB-26 in v0.56.4,
// RB-15 in v0.56.5). This report existed to make that a decision rather than a
// discovery: it enumerated, against LIVE data, who would lose what.
//
// Both switches are now live and the two-axis model's tables are GONE (RB-19,
// v0.57.8), so the four conversion sections — LosesExecute, MultiRoleUsers,
// WideningRestrictions, OrphanScopes, GrantlessMappings — were removed with them.
// They each answered "what changes when we flip", and that question has no meaning
// once the flip is history and its inputs are dropped. Keeping them would have
// meant a report that reads a table it must not read, or reports a permanent zero
// that an operator would mistake for an all-clear.
//
// What remains is a CURRENT-STATE hygiene report — the questions that stay worth
// asking after the migration:
//
//	UngrantedGroups         — AD groups people actually present at login that no
//	                          grant names. Fail-closed means they hold nothing,
//	                          which reads as an outage, not as missing config.
//	UnscopedJobs / Share    — jobs a restricted actor must bind a scope for
//	                          (RB-26), with the denominator that says whether
//	                          this is an edge case or the norm.
//	UnscopedSchedules       — schedules on those jobs; deliberately general-pool
//	                          per RB-Q11(c), so a number to watch, not a blocker.
//	PendingUnbound/Revoked  — parked ad-hoc runs that fire with frozen
//	                          authorization (RB-Q12); the hand-sweep list.
//	EmptyMembershipEntities — secrets/variables with no agency membership, which
//	                          RB-Q14 makes unrestricted-only for writes/reveals.
//
// It mirrors AgencyPreflight (T2.12), which shipped one release ahead of the
// agency tightening for the same reason and is the precedent this follows.
//
// Everything here is read-only and computed on demand. Like AgencyPreflight it
// reads inputs into memory FIRST and resolves in Go — never a query inside an open
// cursor (the db.maxOpenConns pool deadlock).

// RbacJobFinding is one job or schedule affected by the RB-26 binding rule.
type RbacJobFinding struct {
	Kind   string `json:"kind"` // job | schedule | pendingRun
	Name   string `json:"name"`
	Source string `json:"source"`
	// Detail carries the kind-specific fact: recent triggerers for a job, the cron
	// and last fire for a schedule, the creator and run_at for a pending run.
	Detail string `json:"detail"`
	Reason string `json:"reason"`
}

// RbacEntityFinding is one secret or variable with no agency membership, which
// RB-Q14 makes unrestricted-only for writes and reveals.
type RbacEntityFinding struct {
	Kind           string `json:"kind"` // secret | var
	Key            string `json:"key"`
	Scope          string `json:"scope"` // "" = global
	LastModifiedBy string `json:"lastModifiedBy"`
}

// RbacPreflightReport is the whole answer, split by the switch that causes each
// finding so each release can be reviewed and accepted independently.
type RbacPreflightReport struct {
	// ── Grant hygiene ────────────────────────────────────────────────────────
	// UngrantedGroups are AD groups seen in recent_logins that no access grant
	// names. They confer nothing, which is usually a grant someone forgot: the
	// department is told to sign in, does, and holds nothing. Being fail-closed,
	// this reads to them as an outage rather than as missing configuration.
	//
	// Was UnmappedGroups until v0.57.8, when it stopped consulting the retired
	// ad_group_mappings table and started asking the question against the grants
	// that actually decide access.
	UngrantedGroups []string `json:"ungrantedGroups"`
	// EmptyMembershipEntities are secrets/variables with no agency membership,
	// which RB-Q14 makes unrestricted-only for writes and reveals. The
	// "assign memberships first" backlog to clear before RB-15.
	EmptyMembershipEntities []RbacEntityFinding `json:"emptyMembershipEntities"`

	// ── RB-26, v0.56.4 ───────────────────────────────────────────────────────
	// UnscopedJobs are jobs with no declared scope. A restricted actor must bind
	// one at trigger time after RB-26; today anyone may run them unbound.
	UnscopedJobs []RbacJobFinding `json:"unscopedJobs"`
	// UnscopedJobShare is UnscopedJobs as a percentage of the catalog, rounded.
	// A count alone does not say whether this is an edge case or the norm.
	UnscopedJobShare int `json:"unscopedJobShare"`
	// TotalJobs is the denominator for UnscopedJobShare.
	TotalJobs int `json:"totalJobs"`
	// UnscopedSchedules are schedules on unscoped jobs. RB-Q11(c) leaves these
	// deliberately unbound and general-pool, so this is NOT a blocker — it is the
	// number that would pull the deferred per-entry schedule scope (§10) forward.
	// git-source entries are the expensive population: a repo edit, not a UI one.
	UnscopedSchedules []RbacJobFinding `json:"unscopedSchedules"`
	// PendingUnbound are parked ad-hoc runs (RB-31) on unscoped jobs with no
	// frozen scope — created before RB-26 and promoted after it. RB-Q12 fires them
	// with frozen authorization, so these run unbound; listed so that is a choice.
	PendingUnbound []RbacJobFinding `json:"pendingUnbound"`
	// PendingRevoked are parked runs whose creator would lose the trigger verb
	// under RB-2. RB-Q12 fires them anyway (frozen authorization) — this is the
	// list an operator sweeps by hand if that is not what they want.
	PendingRevoked []RbacJobFinding `json:"pendingRevoked"`

	// ── Denominators ─────────────────────────────────────────────────────────
	// UsersEvaluated is how many recent_logins rows were examined, so an empty
	// findings list can be told apart from a report over no data.
	UsersEvaluated int `json:"usersEvaluated"`
	// GrantsEvaluated is the access_grants row count — the denominator that
	// replaced MappingsEvaluated/RestrictionsEvaluated when the two legacy tables
	// were dropped (v0.57.8). Zero here means the report had nothing to bite on.
	GrantsEvaluated int `json:"grantsEvaluated"`
}

// preflightWindowDays bounds "who has triggered this recently". An all-time count
// cannot distinguish a job run this morning from one last run by a departed
// employee in 2024, and that distinction is the whole point of the question.
const preflightWindowDays = 90

// RbacPreflight computes the report against live data.
func RbacPreflight(ctx context.Context, database *sql.DB) (*RbacPreflightReport, error) {
	rep := &RbacPreflightReport{
		UngrantedGroups:         []string{},
		EmptyMembershipEntities: []RbacEntityFinding{},
		UnscopedJobs:            []RbacJobFinding{},
		UnscopedSchedules:       []RbacJobFinding{},
		PendingUnbound:          []RbacJobFinding{},
		PendingRevoked:          []RbacJobFinding{},
	}

	// ── Inputs, all read up front ────────────────────────────────────────────

	// AD group → roles, matched EXACTLY as login matches it.
	//
	// Login is case-SENSITIVE: auth.ResolveGrants does `ad_group IN (…)` against a
	// plain TEXT column with no COLLATE NOCASE, and parseGroups trims but does not
	// fold case. Folding case here would make the report lie in both directions —
	// it would credit a case-mismatched grant with roles the user does not actually
	// get at login, and it would hide that grant from UngrantedGroups, which is the
	// one finding whose entire job is to surface "a grant someone got wrong and
	// that silently does nothing". The report must model the system as it is, not
	// as it should be.
	//
	// v0.57.8: reads access_grants. It read ad_group_mappings until RB-19 dropped
	// that table — which is also why the four conversion sections are gone. They
	// described a migration from the two-axis model that has since completed; a
	// report cannot go on describing a conversion whose inputs no longer exist.
	groupRoles := map[string][]string{}
	grantRows, err := database.QueryContext(ctx, `SELECT ad_group, role FROM access_grants`)
	if err != nil {
		return nil, err
	}
	for grantRows.Next() {
		var g, r string
		if err := grantRows.Scan(&g, &r); err != nil {
			grantRows.Close()
			return nil, err
		}
		groupRoles[g] = append(groupRoles[g], auth.CanonRole(r))
		rep.GrantsEvaluated++
	}
	if err := grantRows.Err(); err != nil {
		grantRows.Close()
		return nil, err
	}
	grantRows.Close()

	// scope name → agency names, and its inverse. This is the expansion RB-13
	// reuses; here it drives the conversion-hazard analysis.
	scopeAgencies, err := loadStringSets(ctx, database, `
		SELECT s.name, a.name FROM scope_agencies sa
		JOIN scopes s   ON s.id = sa.scope_id
		JOIN agencies a ON a.id = sa.agency_id`)
	if err != nil {
		return nil, err
	}
	agencyScopes := map[string][]string{}
	for scope, agencies := range scopeAgencies {
		for _, a := range agencies {
			agencyScopes[a] = append(agencyScopes[a], scope)
		}
	}

	// ── RB-2 / RB-15: per-user findings from recent_logins ───────────────────
	loginRows, err := database.QueryContext(ctx,
		`SELECT email, display_name, groups, last_login_at FROM recent_logins ORDER BY last_login_at DESC`)
	if err != nil {
		return nil, err
	}
	type loginRec struct {
		email, name, groupsJSON, lastLogin string
	}
	var logins []loginRec
	for loginRows.Next() {
		var lr loginRec
		var dn sql.NullString
		if err := loginRows.Scan(&lr.email, &dn, &lr.groupsJSON, &lr.lastLogin); err != nil {
			loginRows.Close()
			return nil, err
		}
		lr.name = dn.String
		logins = append(logins, lr)
	}
	if err := loginRows.Err(); err != nil {
		loginRows.Close()
		return nil, err
	}
	loginRows.Close()
	rep.UsersEvaluated = len(logins)

	// The only per-user question left: does this person's group set intersect any
	// grant at all? Everything else this loop used to compute — LosesExecute,
	// MultiRoleUsers — described the delta between the two-axis model and the grant
	// model. Both switches are live and the old model's tables are gone, so those
	// deltas are not merely zero, they are unanswerable.
	seenUngranted := map[string]bool{}
	for _, lr := range logins {
		var groups []string
		_ = json.Unmarshal([]byte(lr.groupsJSON), &groups)
		for _, g := range groups {
			// parseGroups trims; the DB comparison is then exact. Mirror both.
			k := strings.TrimSpace(g)
			if k == "" || seenUngranted[k] {
				continue
			}
			if _, granted := groupRoles[k]; !granted {
				seenUngranted[k] = true
				rep.UngrantedGroups = append(rep.UngrantedGroups, k)
			}
		}
	}
	sort.Strings(rep.UngrantedGroups)

	// ── RB-26: unscoped jobs, their schedules, and parked runs ───────────────
	// FX-A4: binned jobs are excluded from the denominator, the finding list and
	// the schedule section alike — a half-filtered report is worse than either
	// choice, because UnscopedJobShare and the findings it explains stop agreeing.
	if err := database.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE deleted_at IS NULL`).Scan(&rep.TotalJobs); err != nil {
		return nil, err
	}
	windowStart := time.Now().UTC().AddDate(0, 0, -preflightWindowDays).Format(time.RFC3339)
	// Recency matters: a job last run in 2024 by someone who has left is a very
	// different decision from one run this morning, and an all-time count cannot
	// tell them apart. The window is the report's own definition of "recently".
	//
	// runs.job_source is nullable (NULL ⇒ legacy/git), so the join must COALESCE
	// rather than compare directly — jobs are keyed (source, name), and matching on
	// name alone would merge a git and an amadeus job of the same name and report
	// their combined run count against both.
	// FX-D3 — EXECUTED runs only. A calendar veto, a Forbid refusal and a
	// missed-fire marker are all 'skipped' rows carrying trigger_kind='scheduled',
	// so counting them made a job that has been suppressed every day for a month
	// read as "run 30 times recently, most recently yesterday" — inverting the
	// exact judgement this section exists to support ("a schedule that has never
	// fired is a candidate for deletion, not for a scope column").
	jobRows, err := database.QueryContext(ctx, `
		SELECT j.name, j.source,
		       (SELECT COUNT(*) FROM runs r
		          WHERE r.job_name = j.name
		            AND COALESCE(r.job_source,'git') = j.source
		            AND r.status <> 'skipped'
		            AND r.created_at >= ?)                       AS recent_runs,
		       (SELECT GROUP_CONCAT(DISTINCT r.triggered_by) FROM runs r
		          WHERE r.job_name = j.name
		            AND COALESCE(r.job_source,'git') = j.source
		            AND r.status <> 'skipped'
		            AND r.created_at >= ?)                       AS triggerers,
		       (SELECT MAX(r.created_at) FROM runs r
		          WHERE r.job_name = j.name
		            AND COALESCE(r.job_source,'git') = j.source
		            AND r.status <> 'skipped')                   AS last_run
		FROM jobs j
		WHERE COALESCE(j.scope,'') = '' AND j.deleted_at IS NULL
		ORDER BY j.name`, windowStart, windowStart)
	if err != nil {
		return nil, err
	}
	for jobRows.Next() {
		var name, source string
		var recentRuns int
		var triggerers, lastRun sql.NullString
		if err := jobRows.Scan(&name, &source, &recentRuns, &triggerers, &lastRun); err != nil {
			jobRows.Close()
			return nil, err
		}
		detail := "never run"
		if lastRun.Valid && lastRun.String != "" {
			detail = "last run " + lastRun.String
			if recentRuns > 0 {
				detail += "; " + strconv.Itoa(recentRuns) + "× in the last " +
					strconv.Itoa(preflightWindowDays) + "d by " + orNone(triggerers.String)
			} else {
				detail += "; none in the last " + strconv.Itoa(preflightWindowDays) + "d"
			}
		}
		rep.UnscopedJobs = append(rep.UnscopedJobs, RbacJobFinding{
			Kind: "job", Name: name, Source: source, Detail: detail,
			Reason: "no declared scope; after RB-26 a restricted actor must bind one at " +
				"trigger time and only an unrestricted actor may run it unbound",
		})
	}
	if err := jobRows.Err(); err != nil {
		jobRows.Close()
		return nil, err
	}
	jobRows.Close()
	if rep.TotalJobs > 0 {
		rep.UnscopedJobShare = (len(rep.UnscopedJobs)*100 + rep.TotalJobs/2) / rep.TotalJobs
	}

	// Schedules on unscoped jobs. definition_schedules is what the scheduler reads
	// (the authored catalog is `schedules`), so it is the honest source for "what
	// would actually fire".
	// Last fire time is the load-bearing half of this section: RB-Q11(c) leaves
	// these unbound deliberately, so the number that would pull the deferred
	// per-entry schedule scope (§10) forward is not "how many exist" but "how many
	// actually fire". A schedule that has never fired is a candidate for deletion,
	// not for a scope column.
	schedRows, err := database.QueryContext(ctx, `
		SELECT ds.owner_name, ds.owner_source, ds.name, ds.cron,
		       (SELECT MAX(r.created_at) FROM runs r
		          WHERE r.job_name = ds.owner_name
		            AND COALESCE(r.job_source,'git') = ds.owner_source
		            AND r.trigger_kind = 'scheduled'
		            -- FX-D3: recordSkippedFire stamps trigger_kind='scheduled' on
		            -- every suppression, so without this "last fired" answered
		            -- "when did we last DECLINE to fire".
		            AND r.status <> 'skipped') AS last_fired
		FROM definition_schedules ds
		JOIN jobs j ON j.name = ds.owner_name AND j.source = ds.owner_source
		-- FX-A4: "what would actually fire" excludes the recycle bin — the
		-- scheduler's reload skips binned definitions, so counting one here
		-- inflated the very number this section exists to judge.
		WHERE ds.owner_kind = 'job' AND COALESCE(j.scope,'') = '' AND j.deleted_at IS NULL
		ORDER BY ds.owner_name, ds.name`)
	if err != nil {
		return nil, err
	}
	for schedRows.Next() {
		var owner, source, entry, cron string
		var lastFired sql.NullString
		if err := schedRows.Scan(&owner, &source, &entry, &cron, &lastFired); err != nil {
			schedRows.Close()
			return nil, err
		}
		detail := "cron " + cron
		if lastFired.Valid && lastFired.String != "" {
			detail += "; last fired " + lastFired.String
		} else {
			detail += "; never fired"
		}
		rep.UnscopedSchedules = append(rep.UnscopedSchedules, RbacJobFinding{
			Kind: "schedule", Name: owner + " / " + entry, Source: source,
			Detail: detail,
			Reason: "a scheduled fire has no actor, so RB-Q11(c) leaves it unbound and " +
				"general-pool. NOT a blocker — the number that would pull the deferred " +
				"per-entry schedule scope (§10) forward",
		})
	}
	if err := schedRows.Err(); err != nil {
		schedRows.Close()
		return nil, err
	}
	schedRows.Close()

	// Parked ad-hoc runs (RB-31). Two populations, reported separately because
	// they are caused by different switches.
	pendingRows, err := database.QueryContext(ctx, `
		SELECT p.id, p.kind, p.name, p.source, COALESCE(p.scope,''), p.run_at, p.scheduled_by
		FROM pending_runs p
		WHERE p.status = 'pending'
		ORDER BY p.run_at`)
	if err != nil {
		return nil, err
	}
	type pendingRec struct{ id, kind, name, source, scope, runAt, by string }
	var pendings []pendingRec
	for pendingRows.Next() {
		var p pendingRec
		if err := pendingRows.Scan(&p.id, &p.kind, &p.name, &p.source, &p.scope, &p.runAt, &p.by); err != nil {
			pendingRows.Close()
			return nil, err
		}
		pendings = append(pendings, p)
	}
	if err := pendingRows.Err(); err != nil {
		pendingRows.Close()
		return nil, err
	}
	pendingRows.Close()

	// Creator → roles, for the revoked-verb half. Resolved from recent_logins, the
	// only place a user's groups are recorded; a creator who never logged in (or
	// whose row aged out) cannot be resolved and is reported as unknown rather
	// than assumed safe.
	creatorRoles := map[string][]string{}
	for _, lr := range logins {
		var groups []string
		_ = json.Unmarshal([]byte(lr.groupsJSON), &groups)
		set := map[string]bool{}
		for _, g := range groups {
			for _, r := range groupRoles[strings.ToLower(strings.TrimSpace(g))] {
				set[r] = true
			}
		}
		creatorRoles[strings.ToLower(lr.email)] = sortedKeys(set)
	}

	for _, p := range pendings {
		if p.scope == "" {
			rep.PendingUnbound = append(rep.PendingUnbound, RbacJobFinding{
				Kind: "pendingRun", Name: p.kind + " " + p.name, Source: p.source,
				Detail: "fires " + p.runAt + ", scheduled by " + p.by,
				Reason: "parked with no frozen scope; RB-Q12 fires it with the authorization " +
					"it was created under, so it runs unbound even after RB-26",
			})
		}
		roles, known := creatorRoles[strings.ToLower(p.by)]
		if !known {
			rep.PendingRevoked = append(rep.PendingRevoked, RbacJobFinding{
				Kind: "pendingRun", Name: p.kind + " " + p.name, Source: p.source,
				Detail: "fires " + p.runAt + ", scheduled by " + p.by,
				Reason: "creator has no recent_logins row, so their current grants cannot be " +
					"resolved — verify by hand before RB-2",
			})
			continue
		}
		if !auth.PermsForRoles(roles).TriggerJobs {
			rep.PendingRevoked = append(rep.PendingRevoked, RbacJobFinding{
				Kind: "pendingRun", Name: p.kind + " " + p.name, Source: p.source,
				Detail: "fires " + p.runAt + ", scheduled by " + p.by,
				Reason: "creator's roles (" + strings.Join(roles, "+") + ") do not carry triggerJobs, " +
					"so RB-2 revokes their ability to create this — but RB-Q12 fires the parked " +
					"row anyway. Sweep by hand if that is not intended",
			})
		}
	}

	// ── RB-Q14: entities with no agency membership ───────────────────────────
	for _, spec := range []struct{ kind, table, joinTable, joinCol string }{
		{"secret", "secrets", "secret_agencies", "secret_id"},
		{"var", "env_vars", "env_var_agencies", "env_var_id"},
	} {
		rows, err := database.QueryContext(ctx, `
			SELECT e.key, COALESCE(e.scope,''), COALESCE(e.last_modified_by, COALESCE(e.created_by,''))
			FROM `+spec.table+` e
			WHERE NOT EXISTS (SELECT 1 FROM `+spec.joinTable+` m WHERE m.`+spec.joinCol+` = e.id)
			ORDER BY e.key`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f RbacEntityFinding
			f.Kind = spec.kind
			if err := rows.Scan(&f.Key, &f.Scope, &f.LastModifiedBy); err != nil {
				rows.Close()
				return nil, err
			}
			rep.EmptyMembershipEntities = append(rep.EmptyMembershipEntities, f)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return rep, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orNone(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
