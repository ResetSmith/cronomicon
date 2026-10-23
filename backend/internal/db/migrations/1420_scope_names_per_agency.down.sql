-- 1420_scope_names_per_agency (down) — a scope's name is unique across the
-- installation again, and a scope is born into Global. Fails, and changes
-- nothing, if two agencies have scopes of one name by then.

CREATE TABLE _1420_scope_groups         AS SELECT * FROM scope_groups;
CREATE TABLE _1420_scope_hosts          AS SELECT * FROM scope_hosts;
CREATE TABLE _1420_scope_host_vars      AS SELECT * FROM scope_host_vars;
CREATE TABLE _1420_scope_group_vars     AS SELECT * FROM scope_group_vars;
CREATE TABLE _1420_scope_group_hosts    AS SELECT * FROM scope_group_hosts;
CREATE TABLE _1420_scope_group_children AS SELECT * FROM scope_group_children;
CREATE TABLE _1420_scope_agencies       AS SELECT * FROM scope_agencies;
CREATE TABLE _1420_scope_runners        AS SELECT * FROM scope_runners;

DROP TRIGGER IF EXISTS scope_agencies_repo_fixed;
DROP TRIGGER IF EXISTS scope_owner_on_join;
DROP TRIGGER IF EXISTS scope_owner_on_leave;
DROP TRIGGER IF EXISTS scopes_born_global;
DROP TRIGGER IF EXISTS jobs_born_owned;

CREATE TABLE scopes_new (
    id                TEXT PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    source            TEXT NOT NULL CHECK (source IN ('git','cronomicon')),
    description       TEXT,
    created_by        TEXT,
    created_at        TEXT NOT NULL,
    last_modified_by  TEXT,
    last_modified_at  TEXT,
    source_path       TEXT,
    git_meta_json     TEXT,
    synced_at         TEXT,
    raw_inventory     TEXT,
    inventory_format  TEXT,
    projection_status TEXT NOT NULL DEFAULT 'ok'
        CHECK (projection_status IN ('ok','degraded','unavailable')),
    projection_json   TEXT,
    tags              TEXT NOT NULL DEFAULT '[]',
    repo_id           TEXT,
    owner_agency      TEXT NOT NULL DEFAULT 'global'
);
INSERT INTO scopes_new SELECT id, name, source, description, created_by, created_at, last_modified_by, last_modified_at,
       source_path, git_meta_json, synced_at, raw_inventory, inventory_format, projection_status,
       projection_json, tags, repo_id, owner_agency FROM scopes;
DROP TABLE scopes;
ALTER TABLE scopes_new RENAME TO scopes;

INSERT INTO scope_groups         SELECT * FROM _1420_scope_groups;
INSERT INTO scope_hosts          SELECT * FROM _1420_scope_hosts;
INSERT INTO scope_host_vars      SELECT * FROM _1420_scope_host_vars;
INSERT INTO scope_group_vars     SELECT * FROM _1420_scope_group_vars;
INSERT INTO scope_group_hosts    SELECT * FROM _1420_scope_group_hosts;
INSERT INTO scope_group_children SELECT * FROM _1420_scope_group_children;
INSERT INTO scope_agencies       SELECT * FROM _1420_scope_agencies;
INSERT INTO scope_runners        SELECT * FROM _1420_scope_runners;
DROP TABLE _1420_scope_groups;
DROP TABLE _1420_scope_hosts;
DROP TABLE _1420_scope_host_vars;
DROP TABLE _1420_scope_group_vars;
DROP TABLE _1420_scope_group_hosts;
DROP TABLE _1420_scope_group_children;
DROP TABLE _1420_scope_agencies;
DROP TABLE _1420_scope_runners;

CREATE TRIGGER scopes_born_global AFTER INSERT ON scopes
BEGIN
    INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id)
    VALUES (NEW.id, COALESCE(
        (SELECT g.agency_id FROM git_repos g WHERE NEW.source = 'git' AND g.id = NEW.repo_id),
        'global'));
END;
CREATE TRIGGER scope_owner_on_join AFTER INSERT ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = NEW.scope_id),
        'global')
     WHERE id = NEW.scope_id;
END;
CREATE TRIGGER scope_owner_on_leave AFTER DELETE ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = OLD.scope_id),
        'global')
     WHERE id = OLD.scope_id;
END;
CREATE INDEX idx_scopes_owner_name ON scopes (owner_agency, name);
CREATE TRIGGER jobs_born_owned AFTER INSERT ON jobs WHEN NEW.owner_agency IS NULL
BEGIN
    UPDATE jobs SET owner_agency = CASE WHEN NEW.source = 'git' AND NEW.repo_id IS NOT NULL
        THEN COALESCE((SELECT g.agency_id FROM git_repos g WHERE g.id = NEW.repo_id), 'global')
        ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END
                         FROM scope_agencies sa JOIN scopes sc ON sc.id = sa.scope_id
                        WHERE sc.name = NEW.scope), 'global') END
     WHERE rowid = NEW.rowid;
END;
CREATE TRIGGER scope_agencies_repo_fixed BEFORE INSERT ON scope_agencies
WHEN EXISTS (SELECT 1 FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
              WHERE sc.id = NEW.scope_id AND sc.source = 'git'
                AND g.id <> 'global' AND g.agency_id <> NEW.agency_id)
BEGIN
    SELECT RAISE(ABORT, 'scope_agency_fixed: a scope that comes from an agency''s repository belongs to that agency');
END;
