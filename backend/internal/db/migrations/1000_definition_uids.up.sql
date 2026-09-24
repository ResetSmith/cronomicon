-- 1000_definition_uids — AF-4a: jobs, workflows and schedules gain a stable
-- surrogate identity (the agency-first-rbac plan, phase AF-4).
--
-- WHY. These three catalogs are keyed PRIMARY KEY (source, name), and the name
-- is load-bearing across ~21 referencing columns, five cascade triggers, the
-- concurrency-gate key, the entity-code allocator, the runner wire protocol and
-- alert targeting (the AF-4 inventory ranks ten of those edges as failing
-- SILENTLY if a name could repeat). The product owner's direction is per-agency name
-- uniqueness — which therefore requires identity to move OFF the name before
-- any uniqueness rule can relax. This migration is that first, deliberately
-- behavior-neutral step: every definition gets a uid that is assigned once and
-- never changes — across syncs, edits, renames-via-recreate, and restores.
--
-- WHAT THIS IS NOT. The PK does not change here, no referencing edge moves, and
-- (source, name) stays globally unique — so every textual join in the codebase
-- remains exactly as correct as yesterday. ADD COLUMN + UNIQUE INDEX, no table
-- rebuild: rebuilding these three would have to re-create five jobs triggers and
-- two workflows triggers (the 940 header documents how a forgotten one silently
-- stops cascading), a risk the foundation step has no reason to take. The PK
-- swap happens in AF-4b, where the rebuild is unavoidable anyway.
--
-- Backfill uses randomblob() hex — opaque and unique, which is all an id must
-- be. Rows created after this migration get UUIDv7 from db.NewID() like every
-- other backend-assigned id; the two formats coexist harmlessly because nothing
-- may ever parse one. NOT NULL is enforced by the writers (and the unique index
-- makes a forgotten writer collide loudly on '' rather than drift silently):
-- ALTER TABLE ADD COLUMN cannot add NOT NULL without a constant default, and a
-- constant default is exactly what a unique id must not have.

ALTER TABLE jobs      ADD COLUMN uid TEXT;
ALTER TABLE workflows ADD COLUMN uid TEXT;
ALTER TABLE schedules ADD COLUMN uid TEXT;

UPDATE jobs      SET uid = lower(hex(randomblob(16)));
UPDATE workflows SET uid = lower(hex(randomblob(16)));
UPDATE schedules SET uid = lower(hex(randomblob(16)));

CREATE UNIQUE INDEX idx_jobs_uid      ON jobs(uid);
CREATE UNIQUE INDEX idx_workflows_uid ON workflows(uid);
CREATE UNIQUE INDEX idx_schedules_uid ON schedules(uid);
