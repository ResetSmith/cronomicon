-- 800_roles — the role catalog becomes DATA (the rbac-update plan RB-6).
--
-- Until now the permission matrix was a compiled-in Go literal, so "our Tax agency
-- needs an Operator and a Viewer scoped to just Tax" could not be expressed without
-- a recompile. Modelling departments as roles instead would mean 24 agencies × 4
-- levels = 96 roles, and four more for every department added — which is why roles
-- stay pure, reusable PERMISSION TEMPLATES here and the departmental axis attaches
-- to the grant (Phase 2, access_grants) rather than to the role.
--
-- Columns rather than a role_permissions(role, perm) join table, deliberately: the
-- Go side is a fixed struct that the OpenAPI Role schema pins, so columns keep the
-- union compile-checked and keep the wire shape typed. Operator-DEFINED permissions
-- (RB-Q5) are explicitly not proposed — new permissions come from new routes, which
-- is a code change regardless.
--
-- SIX permission columns, not the eight the matrix carried. viewDashboard and
-- editSchedules are deleted (RB-Q9/RB-Q13): neither had a predicate anywhere and
-- neither could get one — viewDashboard gates nothing a session does not already
-- reach, and the only schedule-authoring surface is admin-gated by requireCompose,
-- which RB-30's invariant requires to stay that way. They are omitted here rather
-- than created-and-dropped because removing a column in SQLite is a table rebuild.
--
-- `rank` replaces auth.rolePrecedence. approver is seeded at 2 DELIBERATELY: it was
-- absent from that map and therefore ranked 0, which rendered a blank role in the
-- Honest View for an approver-only user. Reproducing that omission in data would
-- carry the bug forward.
--
-- No FK from ad_group_mappings.role or scope_restrictions.role to roles(name).
-- Those columns are plain TEXT today and adding a constraint would require a table
-- rebuild of both, on a schema that Phase 2 replaces wholesale with access_grants.
-- The role name is validated at the API boundary instead (RB-8).
CREATE TABLE roles (
    name             TEXT PRIMARY KEY,             -- lowercase canonical
    description      TEXT,
    builtin          INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0,1)),
    rank             INTEGER NOT NULL DEFAULT 0,   -- precedence; higher wins
    trigger_jobs     INTEGER NOT NULL DEFAULT 0 CHECK (trigger_jobs IN (0,1)),
    kill_jobs        INTEGER NOT NULL DEFAULT 0 CHECK (kill_jobs IN (0,1)),
    manage_env_vars  INTEGER NOT NULL DEFAULT 0 CHECK (manage_env_vars IN (0,1)),
    publish_schedule INTEGER NOT NULL DEFAULT 0 CHECK (publish_schedule IN (0,1)),
    configure_app    INTEGER NOT NULL DEFAULT 0 CHECK (configure_app IN (0,1)),
    manage_roles     INTEGER NOT NULL DEFAULT 0 CHECK (manage_roles IN (0,1)),
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT
);

-- The four built-ins, carrying EXACTLY the matrix that was compiled in, minus the
-- two deleted permissions. viewer holds no permissions at all — correct, not an
-- oversight: its only entry was viewDashboard. A viewer's access is visibility
-- (its resolved scopes), and visibility is not a verb.
INSERT INTO roles (name, description, builtin, rank,
                   trigger_jobs, kill_jobs, manage_env_vars,
                   publish_schedule, configure_app, manage_roles)
VALUES
    ('admin',    'Full control, including roles and application configuration.', 1, 3, 1, 1, 1, 1, 1, 1),
    ('approver', 'Runs jobs and publishes definitions to GitLab.',               1, 2, 1, 1, 0, 1, 0, 0),
    ('operator', 'Runs and stops jobs within the scopes granted to the role.',   1, 2, 1, 1, 0, 0, 0, 0),
    ('viewer',   'Read-only. Sees the scopes granted to the role and holds no verbs.', 1, 1, 0, 0, 0, 0, 0, 0);
