// Package preflight reports what an upgrade will change for THIS installation,
// before it is made (GC-19).
//
// v2.2.2 takes abilities away from an administrator of one agency: the
// install-wide routes they used to reach are now a global administrator's. The
// project's upgrade posture is a warning, not machinery — so the warning has to
// arrive before the upgrade and name the groups it concerns, or it is a
// changelog entry nobody connects to their own grants until a screen refuses
// them.
//
// A report is read-only and reads only tables that exist in the PREVIOUS
// release: it is run with the new binary against the old database, before any
// migration. Each release that changes behavior on upgrade adds its own
// section; this file holds the one for 2.2.2.
package preflight

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// perms is one role's permission set, read from the roles table.
type perms struct {
	configureApp, manageRoles, manageEnvVars, publishSchedule, compose, builtinAdmin bool
}

// AgencyGrant is a departmental grant that loses an ability in 2.2.2.
type AgencyGrant struct {
	ADGroup, Role, Agency string
	Loses                 []string
}

// ServiceAccount is an active account its creator could not mint under the
// 2.2.2 rules.
type ServiceAccount struct {
	Name, Role, Where, CreatedBy, Why string
}

// VaultSecret is a vault-source secret owned by an agency.
type VaultSecret struct {
	Key, Scope string
	Agencies   []string
}

// Report is the 2.2.2 section.
type Report struct {
	// GlobalAdminGroups are the AD groups holding an all-agencies grant whose
	// role carries configureApp and manageRoles — who can still change the
	// installation and repair access after the upgrade.
	GlobalAdminGroups []string
	AgencyGrants      []AgencyGrant
	ServiceAccounts   []ServiceAccount
	VaultSecrets      []VaultSecret
}

// HasGlobalAdmin reports whether anyone can administer the installation.
func (r Report) HasGlobalAdmin() bool { return len(r.GlobalAdminGroups) > 0 }

// Quiet reports whether the upgrade changes nothing for this installation.
func (r Report) Quiet() bool {
	return r.HasGlobalAdmin() && len(r.AgencyGrants) == 0 && len(r.ServiceAccounts) == 0 && len(r.VaultSecrets) == 0
}

func loadRoles(ctx context.Context, db *sql.DB) (map[string]perms, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lower(name), configure_app, manage_roles, manage_env_vars, publish_schedule, compose, builtin
		  FROM roles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]perms{}
	for rows.Next() {
		var name string
		var ca, mr, me, ps, co, bi int
		if err := rows.Scan(&name, &ca, &mr, &me, &ps, &co, &bi); err != nil {
			return nil, err
		}
		out[name] = perms{ca == 1, mr == 1, me == 1, ps == 1, co == 1, bi == 1 && name == "admin"}
	}
	return out, rows.Err()
}

type grant struct {
	group, role, agencyID, agencyName string
	all                               bool
}

func loadGrants(ctx context.Context, db *sql.DB) ([]grant, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT g.ad_group, lower(g.role), COALESCE(g.agency_id,''), COALESCE(a.name,''), g.all_scopes
		  FROM access_grants g LEFT JOIN agencies a ON a.id = g.agency_id
		 ORDER BY g.ad_group, a.name, g.role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []grant
	for rows.Next() {
		var g grant
		var all int
		if err := rows.Scan(&g.group, &g.role, &g.agencyID, &g.agencyName, &all); err != nil {
			return nil, err
		}
		g.all = all == 1
		out = append(out, g)
	}
	return out, rows.Err()
}

// losses lists what a DEPARTMENTAL grant of this role can no longer do.
func losses(p perms) []string {
	var out []string
	if p.configureApp {
		out = append(out,
			"change install-wide settings (General, Notifications, GitLab, Vault, Log Storage, Observability, Audit & Compliance) or run a Git sync",
			"export the audit history",
			"create, rename or delete agencies, or move a scope between agencies",
			"edit, delete or re-bind a scope that belongs to another agency or to none",
			"create or edit alert rules",
			"create or edit bastions and manually authored SSH host records")
	}
	if p.builtinAdmin {
		out = append(out, "author reusable schedules, calendars or reactions, or use revisions and the recycle bin")
	}
	if p.manageEnvVars {
		out = append(out, "create, edit or migrate Vault-backed secrets")
	}
	if p.manageRoles {
		out = append(out, "mint or revoke a service account outside this agency, for every agency, or with a role above their own")
	}
	if p.publishSchedule {
		out = append(out, "publish schedule or workflow files, unscoped jobs, or jobs in another agency's scope")
	}
	return out
}

// Build reads the 2.2.2 report.
func Build(ctx context.Context, db *sql.DB) (Report, error) {
	var rep Report
	roles, err := loadRoles(ctx, db)
	if err != nil {
		return rep, fmt.Errorf("read roles: %w", err)
	}
	grants, err := loadGrants(ctx, db)
	if err != nil {
		return rep, fmt.Errorf("read access grants: %w", err)
	}
	seenAdmin := map[string]bool{}
	for _, g := range grants {
		p := roles[g.role]
		if g.all {
			if p.configureApp && p.manageRoles && !seenAdmin[g.group] {
				seenAdmin[g.group] = true
				rep.GlobalAdminGroups = append(rep.GlobalAdminGroups, g.group)
			}
			continue
		}
		if l := losses(p); len(l) > 0 {
			rep.AgencyGrants = append(rep.AgencyGrants, AgencyGrant{ADGroup: g.group, Role: g.role, Agency: g.agencyName, Loses: l})
		}
	}
	sort.Strings(rep.GlobalAdminGroups)

	if rep.ServiceAccounts, err = serviceAccounts(ctx, db, roles, grants); err != nil {
		return rep, fmt.Errorf("read service accounts: %w", err)
	}
	if rep.VaultSecrets, err = vaultSecrets(ctx, db); err != nil {
		return rep, fmt.Errorf("read secrets: %w", err)
	}
	return rep, nil
}

// serviceAccounts lists the active accounts whose creator, judged by the AD
// groups they last signed in with, could not mint them under GC-5: an account
// for every agency, for an agency the creator does not administer, or with a
// role carrying a permission the creator does not hold there.
func serviceAccounts(ctx context.Context, db *sql.DB, roles map[string]perms, grants []grant) ([]ServiceAccount, error) {
	byGroup := map[string][]grant{}
	for _, g := range grants {
		byGroup[g.group] = append(byGroup[g.group], g)
	}
	creatorGrants := func(email string) ([]grant, bool) {
		var groupsJSON string
		if err := db.QueryRowContext(ctx,
			`SELECT groups FROM recent_logins WHERE email = ? COLLATE NOCASE`, email).Scan(&groupsJSON); err != nil {
			return nil, false
		}
		var groups []string
		_ = json.Unmarshal([]byte(groupsJSON), &groups)
		var out []grant
		for _, grp := range groups {
			out = append(out, byGroup[grp]...)
		}
		return out, true
	}
	rows, err := db.QueryContext(ctx, `
		SELECT sa.name, lower(sa.role), COALESCE(sa.agency_id,''), COALESCE(a.name,''), sa.all_scopes, sa.created_by
		  FROM service_accounts sa LEFT JOIN agencies a ON a.id = sa.agency_id
		 WHERE sa.revoked_at IS NULL
		 ORDER BY sa.name`)
	if err != nil {
		return nil, err
	}
	type acct struct {
		name, role, agencyID, agencyName, createdBy string
		all                                         bool
	}
	var accts []acct
	for rows.Next() {
		var a acct
		var all int
		if err := rows.Scan(&a.name, &a.role, &a.agencyID, &a.agencyName, &all, &a.createdBy); err != nil {
			rows.Close()
			return nil, err
		}
		a.all = all == 1
		accts = append(accts, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	var out []ServiceAccount
	for _, a := range accts {
		where := a.agencyName
		if a.all {
			where = "every agency"
		}
		held, known := creatorGrants(a.createdBy)
		flag := func(why string) {
			out = append(out, ServiceAccount{Name: a.name, Role: a.role, Where: where, CreatedBy: a.createdBy, Why: why})
		}
		if !known {
			if a.all {
				flag("grants every agency, and its creator has no recorded sign-in to check against")
			}
			continue
		}
		global := false
		var here perms // the union of what the creator holds IN the account's agency
		for _, g := range held {
			p := roles[g.role]
			if g.all && p.manageRoles {
				global = true
			}
			if g.all || g.agencyID == a.agencyID {
				here.configureApp = here.configureApp || p.configureApp
				here.manageRoles = here.manageRoles || p.manageRoles
				here.manageEnvVars = here.manageEnvVars || p.manageEnvVars
				here.publishSchedule = here.publishSchedule || p.publishSchedule
				here.compose = here.compose || p.compose
			}
		}
		if global {
			continue
		}
		want := roles[a.role]
		switch {
		case a.all:
			flag("grants every agency; its creator administers access for one agency only")
		case !here.manageRoles:
			flag("its creator does not administer access for " + where)
		case (want.configureApp && !here.configureApp) || (want.manageEnvVars && !here.manageEnvVars) ||
			(want.publishSchedule && !here.publishSchedule) || (want.compose && !here.compose):
			flag("its role carries a permission its creator does not hold in " + where)
		}
	}
	return out, nil
}

func vaultSecrets(ctx context.Context, db *sql.DB) ([]VaultSecret, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.key, COALESCE(s.scope,''), COALESCE(a.name,'')
		  FROM secrets s
		  JOIN secret_agencies sa ON sa.secret_id = s.id
		  JOIN agencies a ON a.id = sa.agency_id
		 WHERE s.source = 'vault'
		 ORDER BY s.key, s.scope, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VaultSecret
	for rows.Next() {
		var key, scope, agency string
		if err := rows.Scan(&key, &scope, &agency); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].Key == key && out[n-1].Scope == scope {
			out[n-1].Agencies = append(out[n-1].Agencies, agency)
			continue
		}
		out = append(out, VaultSecret{Key: key, Scope: scope, Agencies: []string{agency}})
	}
	return out, rows.Err()
}

// Write renders the report as plain text.
func (r Report) Write(w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	p("Cronomicon 2.2.2 — what this upgrade changes for this installation")
	p("")
	p("From 2.2.2, a change to the INSTALLATION needs a global administrator: a role")
	p("granted on every agency that itself carries the permission. Holding the")
	p("permission on one agency is no longer enough.")
	p("")
	if r.HasGlobalAdmin() {
		p("Global administrators (configureApp and manageRoles on every agency):")
		for _, g := range r.GlobalAdminGroups {
			p("  %s", g)
		}
	} else {
		p("!! NO GLOBAL ADMINISTRATOR EXISTS.")
		p("   No access grant gives configureApp and manageRoles on every agency, so after")
		p("   the upgrade nobody can change install-wide settings or repair access.")
		p("   Before upgrading, either add an all-agencies admin grant in Settings →")
		p("   Users & Access, or run (with the server stopped):")
		p("       cronomicon grant-admin <ad-group>")
		p("   Trusted-header deployments may also set CRONOMICON_BOOTSTRAP_ADMIN_GROUP.")
	}
	p("")
	if len(r.AgencyGrants) == 0 {
		p("No agency-scoped grant loses an ability.")
	} else {
		p("Agency-scoped grants that lose abilities (%d):", len(r.AgencyGrants))
		for _, g := range r.AgencyGrants {
			p("")
			p("  %s — role %q on %s — can no longer:", g.ADGroup, g.Role, g.Agency)
			for _, l := range g.Loses {
				p("    - %s", l)
			}
		}
		p("")
		p("  Within their own agency these groups keep everything else: their scopes,")
		p("  runners, stored secrets, variables, SSH keys, jobs, workflows and access grants.")
		p("  A user who is ALSO a viewer of every agency was treated as unrestricted by two")
		p("  access checks; that no longer holds.")
	}
	p("")
	if len(r.ServiceAccounts) == 0 {
		p("No active service account exceeds what its creator could grant.")
	} else {
		p("Active service accounts their creator could not mint under the new rules (%d) —", len(r.ServiceAccounts))
		p("they keep working; review each and revoke any that should not exist:")
		for _, s := range r.ServiceAccounts {
			p("  %s — role %q on %s, created by %s: %s", s.Name, s.Role, s.Where, s.CreatedBy, s.Why)
		}
	}
	p("")
	if len(r.VaultSecrets) == 0 {
		p("No agency owns a Vault-backed secret.")
	} else {
		p("Vault-backed secrets owned by an agency (%d) — they keep resolving, and their", len(r.VaultSecrets))
		p("agency can still use and reveal them, but only a global administrator may edit them:")
		for _, s := range r.VaultSecrets {
			scope := s.Scope
			if scope == "" {
				scope = "(global scope)"
			}
			p("  %s in %s — %s", s.Key, scope, strings.Join(s.Agencies, ", "))
		}
	}
}
