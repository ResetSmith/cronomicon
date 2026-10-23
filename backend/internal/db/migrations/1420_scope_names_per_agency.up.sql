-- 1420_scope_names_per_agency — a scope's name is unique within its agency
-- (2.4.0, §0a, IN-5; Phase I2).
--
-- Until now `scopes.name` was UNIQUE across the installation (003), so one
-- agency's scope `web` kept every other agency from having one, and an
-- agency's administrator could be refused a name for a reason in another
-- agency they may not see (GR-19, scope_name_in_use). The key is now
-- (owner_agency, name): two agencies can each have `web`, and one agency
-- cannot have two. owner_agency is the scope's one agency, or Global's for a
-- scope several share (1410).
--
-- A column constraint cannot be dropped, so the table is rebuilt. Foreign keys
-- are enforced while migrations run, and DROP TABLE performs an implicit
-- DELETE that fires every ON DELETE CASCADE pointing at the table (the 830
-- lesson). Eight tables hang off `scopes` (six directly, two through
-- scope_groups), so their rows are copied aside first and put back after the
-- rename. The triggers whose bodies name `scopes` are dropped and created
-- again around it, and scope_agencies_repo_fixed only after the rows are back,
-- so it does not judge rows that are already there.
--
-- A scope is now born into the agency it is created for (its owner_agency,
-- which the writer gives), or its repository's for a Git scope; until now it
-- was born into Global and moved by a second write, which a name Global
-- already holds would refuse.

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
DROP INDEX IF EXISTS idx_scopes_owner_name;

CREATE TABLE scopes_new (
    id                TEXT PRIMARY KEY,              -- UUIDv7
    name              TEXT NOT NULL,
    source            TEXT NOT NULL CHECK (source IN ('git','cronomicon')),
    description       TEXT,
    created_by        TEXT,
    created_at        TEXT NOT NULL,
    last_modified_by  TEXT,                           -- S4
    last_modified_at  TEXT,                           -- S4
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
    owner_agency      TEXT NOT NULL DEFAULT 'global', -- 1410
    UNIQUE (owner_agency, name)
);

INSERT INTO scopes_new (id, name, source, description, created_by, created_at, last_modified_by, last_modified_at,
                        source_path, git_meta_json, synced_at, raw_inventory, inventory_format, projection_status,
                        projection_json, tags, repo_id, owner_agency)
SELECT id, name, source, description, created_by, created_at, last_modified_by, last_modified_at,
       source_path, git_meta_json, synced_at, raw_inventory, inventory_format, projection_status,
       projection_json, tags, repo_id, owner_agency
  FROM scopes;

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
        NEW.owner_agency,
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

-- 1400's, reading the scope's owner: a job written with no owner is born its
-- repository's (a Git job) or the agency of the one scope of its scope's name
-- (Global's when there is none or several, as for a scope several share).
CREATE TRIGGER jobs_born_owned AFTER INSERT ON jobs WHEN NEW.owner_agency IS NULL
BEGIN
    UPDATE jobs SET owner_agency = CASE WHEN NEW.source = 'git' AND NEW.repo_id IS NOT NULL
        THEN COALESCE((SELECT g.agency_id FROM git_repos g WHERE g.id = NEW.repo_id), 'global')
        ELSE COALESCE((SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sc.owner_agency) END
                         FROM scopes sc WHERE sc.name = NEW.scope), 'global') END
     WHERE rowid = NEW.rowid;
END;

CREATE TRIGGER scope_agencies_repo_fixed BEFORE INSERT ON scope_agencies
WHEN EXISTS (SELECT 1 FROM scopes sc JOIN git_repos g ON g.id = sc.repo_id
              WHERE sc.id = NEW.scope_id AND sc.source = 'git'
                AND g.id <> 'global' AND g.agency_id <> NEW.agency_id)
BEGIN
    SELECT RAISE(ABORT, 'scope_agency_fixed: a scope that comes from an agency''s repository belongs to that agency');
END;
