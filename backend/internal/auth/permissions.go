package auth

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"sync"
)

// The role→permission registry (RB-1 / RB-6 / RB-7, the rbac-update plan).
//
// This lived in internal/api until v0.56.0 and was a hardcoded Go matrix until
// v0.56.1. It is now a `roles` TABLE read into a process cache, which is what lets
// an operator define "tax-operator" without a recompile.
//
// Why a cache rather than a query: PermsForRoles is called from requirePerm, which
// is mount-time middleware wrapping ~75 routes and has no context.Context and no
// *sql.DB in scope. Threading both through every gate to re-read four rows on every
// request would be a large, invasive change to buy nothing — the table changes only
// when an admin edits a role, and every such edit already revokes all other sessions
// (SU-5). So: load once at boot, swap the map on write.
//
// The cache is seeded with the built-in matrix at init, so a process that has not
// yet called Refresh (or one whose Refresh failed) authorizes exactly as v0.56.0
// did rather than denying everything or granting everything.

// Permissions is the fixed permission set carried by a role. The JSON tags are the
// wire contract: they are what GET /roles emits and what the OpenAPI Role schema
// pins, so renaming a field is a contract change.
//
// SEVEN permissions since AF-2 (six before it). viewDashboard and editSchedules were deleted in
// v0.56.1 (RB-Q9/RB-Q13): neither had a predicate anywhere, and neither could get
// one. viewDashboard gates nothing a session does not already reach, and the only
// schedule-authoring surface is admin-gated by requireCompose — which RB-30's
// invariant requires to STAY admin-gated, so a grantable schedule verb behind it
// would be decorative by construction. A permission that cannot be enforced must
// not be displayed as if it were; that is the disease this plan treats.
//
// All six are enforced. triggerJobs and killJobs were checked nowhere until RB-2
// (FR-M1) wired them at every execution route in v0.56.4; RF-6 closed the two it
// missed in v0.56.7.
type Permissions struct {
	TriggerJobs     bool `json:"triggerJobs"`
	KillJobs        bool `json:"killJobs"`
	ManageEnvVars   bool `json:"manageEnvVars"`
	PublishSchedule bool `json:"publishSchedule"`
	ConfigureApp    bool `json:"configureApp"`
	ManageRoles     bool `json:"manageRoles"`
	// Compose (AF-2) — authoring amadeus-source JOBS and WORKFLOWS. Grantable and
	// AGENCY-BOUND: holding it means you may author within the scopes your grant
	// reaches, never everywhere. The four other surfaces the old admin-only gate
	// covered (schedule-defs, calendars, reactions, revisions/recycle-bin) act on
	// objects with no scope of their own and stay admin-only — see
	// requireComposeAdmin. An All-scoped (unscoped) job is also admin-only, which
	// is what keeps RB-30's unscoped-scheduling bypass unreachable.
	Compose bool `json:"compose"`
}

// Permission names, as they appear in JSON, in requirePerm's audit rows, and as
// the `perm` argument to Identity.Can. Using the constants keeps a typo in a Can
// call from silently evaluating to "permission nobody has" — Has reports false for
// an unknown name, which fails closed but fails silently.
const (
	PermTriggerJobs     = "triggerJobs"
	PermKillJobs        = "killJobs"
	PermManageEnvVars   = "manageEnvVars"
	PermPublishSchedule = "publishSchedule"
	PermConfigureApp    = "configureApp"
	PermManageRoles     = "manageRoles"
	PermCompose         = "compose"
)

// PermissionNames is every permission, in wire order. Exported so the roles UI and
// the CRUD validator enumerate the same list the struct declares.
var PermissionNames = []string{
	PermTriggerJobs, PermKillJobs, PermManageEnvVars,
	PermPublishSchedule, PermConfigureApp, PermManageRoles, PermCompose,
}

// Has reports whether the set carries the named permission. An unrecognized name
// reports false — Can fails closed on a typo rather than granting.
func (p Permissions) Has(perm string) bool {
	switch perm {
	case PermTriggerJobs:
		return p.TriggerJobs
	case PermKillJobs:
		return p.KillJobs
	case PermManageEnvVars:
		return p.ManageEnvVars
	case PermPublishSchedule:
		return p.PublishSchedule
	case PermConfigureApp:
		return p.ConfigureApp
	case PermManageRoles:
		return p.ManageRoles
	case PermCompose:
		return p.Compose
	}
	return false
}

// With returns a copy with the named permission set. Unknown names are ignored,
// mirroring Has — the CRUD boundary validates names before calling this.
func (p Permissions) With(perm string, v bool) Permissions {
	switch perm {
	case PermTriggerJobs:
		p.TriggerJobs = v
	case PermKillJobs:
		p.KillJobs = v
	case PermManageEnvVars:
		p.ManageEnvVars = v
	case PermPublishSchedule:
		p.PublishSchedule = v
	case PermConfigureApp:
		p.ConfigureApp = v
	case PermManageRoles:
		p.ManageRoles = v
	case PermCompose:
		p.Compose = v
	}
	return p
}

// Or returns the union of two permission sets. Extracted so the OR-union has ONE
// definition — PermsForRoles and any future grant merge both use it, and adding a
// permission field means updating one function, not three.
func (p Permissions) Or(q Permissions) Permissions {
	return Permissions{
		TriggerJobs:     p.TriggerJobs || q.TriggerJobs,
		KillJobs:        p.KillJobs || q.KillJobs,
		ManageEnvVars:   p.ManageEnvVars || q.ManageEnvVars,
		PublishSchedule: p.PublishSchedule || q.PublishSchedule,
		ConfigureApp:    p.ConfigureApp || q.ConfigureApp,
		ManageRoles:     p.ManageRoles || q.ManageRoles,
		Compose:         p.Compose || q.Compose,
	}
}

// Role is one row of the permission matrix.
type Role struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Builtin     bool        `json:"builtin"`
	Rank        int         `json:"rank"`
	Permissions Permissions `json:"permissions"`
}

// AdminRole is the role that must always retain ManageRoles. Named so the lockout
// guards read as intent rather than as a magic string.
const AdminRole = "admin"

// CanonRole normalizes a role name to its lowercase canonical form.
func CanonRole(role string) string { return strings.ToLower(strings.TrimSpace(role)) }

// builtinRoles is the compiled-in matrix. It is the migration-800 seed, and it is
// the cache's contents before Refresh runs — so a process that cannot read the
// table still authorizes exactly as v0.56.0 did.
//
// Ranks mirror the old auth.rolePrecedence (admin 3, operator 2, viewer 1) with
// approver seeded DELIBERATELY at 2. approver was absent from that map and so
// ranked 0, which rendered a blank role in the Honest View for an approver-only
// user; reproducing the omission in the seed would carry a bug forward into data.
//
// viewer holds NO permissions. That is correct, not an oversight: its only entry
// was viewDashboard, which v0.56.1 deletes. A viewer's access is visibility —
// resolved scopes — and visibility is not a verb.
func builtinRoles() []Role {
	return []Role{
		{Name: "admin", Builtin: true, Rank: 3,
			Description: "Full control, including roles and application configuration.",
			Permissions: Permissions{
				TriggerJobs: true, KillJobs: true, ManageEnvVars: true,
				PublishSchedule: true, ConfigureApp: true, ManageRoles: true,
			}},
		{Name: "approver", Builtin: true, Rank: 2,
			Description: "Runs jobs and publishes definitions to GitLab.",
			Permissions: Permissions{
				TriggerJobs: true, KillJobs: true, PublishSchedule: true,
			}},
		{Name: "operator", Builtin: true, Rank: 2,
			Description: "Runs and stops jobs within the scopes granted to the role.",
			Permissions: Permissions{
				TriggerJobs: true, KillJobs: true,
			}},
		{Name: "viewer", Builtin: true, Rank: 1,
			Description: "Read-only. Sees the scopes granted to the role and holds no verbs.",
			Permissions: Permissions{}},
	}
}

// ── The process cache ────────────────────────────────────────────────────────

var registry = struct {
	mu     sync.RWMutex
	byName map[string]Role
}{byName: indexRoles(builtinRoles())}

func indexRoles(rs []Role) map[string]Role {
	m := make(map[string]Role, len(rs))
	for _, r := range rs {
		m[r.Name] = r
	}
	return m
}

// RefreshRoles reloads the role registry from the database. Call it once at boot
// (after migrations) and after every write to the roles table — those are the only
// moments the answer can change, and each write also revokes every other session.
//
// On error the previous contents are kept: a transient DB failure must not silently
// de-privilege every operator.
func RefreshRoles(ctx context.Context, database *sql.DB) error {
	rows, err := database.QueryContext(ctx, `
		SELECT name, COALESCE(description,''), builtin, rank,
		       trigger_jobs, kill_jobs, manage_env_vars,
		       publish_schedule, configure_app, manage_roles, compose
		FROM roles`)
	if err != nil {
		return err
	}
	defer rows.Close()

	loaded := map[string]Role{}
	for rows.Next() {
		var r Role
		var builtin int
		if err := rows.Scan(&r.Name, &r.Description, &builtin, &r.Rank,
			&r.Permissions.TriggerJobs, &r.Permissions.KillJobs, &r.Permissions.ManageEnvVars,
			&r.Permissions.PublishSchedule, &r.Permissions.ConfigureApp, &r.Permissions.ManageRoles,
			&r.Permissions.Compose); err != nil {
			return err
		}
		r.Builtin = builtin != 0
		r.Name = CanonRole(r.Name)
		loaded[r.Name] = r
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// An empty table would mean "nobody can do anything" — including repair the
	// table. Treat it as a failed read and keep the built-ins.
	if len(loaded) == 0 {
		return nil
	}

	registry.mu.Lock()
	registry.byName = loaded
	registry.mu.Unlock()
	return nil
}

// AllRoles returns every known role, ordered by descending rank then name, so the
// list is stable for display and for tests.
func AllRoles() []Role {
	registry.mu.RLock()
	out := make([]Role, 0, len(registry.byName))
	for _, r := range registry.byName {
		out = append(out, r)
	}
	registry.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank > out[j].Rank
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// LookupRole returns the named role, canonicalized.
func LookupRole(name string) (Role, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	r, ok := registry.byName[CanonRole(name)]
	return r, ok
}

// RoleValid reports whether the name resolves to a known role. Custom roles
// validate too, which is the entire point of RB-6.
func RoleValid(role string) bool {
	_, ok := LookupRole(role)
	return ok
}

// PermsForRoles returns the OR-union of the matrix rows for the caller's resolved
// roles (PP-B1). Unknown roles contribute nothing, so a user with no mapped role
// gets the zero Permissions (all false → denied).
//
// NOTE the union here is over ROLES, discarding which role supplied which
// permission — that is exactly half of the cross-product leak this plan closes
// (§0). Identity.Can is the seam that keeps the association; this function stays
// for the global, scope-less permissions where the union is the correct answer.
func PermsForRoles(roles []string) Permissions {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	var out Permissions
	for _, rn := range roles {
		if r, ok := registry.byName[CanonRole(rn)]; ok {
			out = out.Or(r.Permissions)
		}
	}
	return out
}
