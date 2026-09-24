-- 290 SQLite-only, sync-preserved user-authored tags on schedules and workflows
-- (tags-support.md). Same model as scripts.tags (280): authored via the dedicated
-- PUT tag endpoints, NEVER parsed from Git, omitted from every sync/compose upsert
-- so they survive. Additive ALTERs — no table rebuild. (jobs.tags already exists
-- from migration 005/170; it becomes operator-owned here too via the upsertJobs
-- D6 change, but needs no DDL.)
ALTER TABLE schedules ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
ALTER TABLE workflows ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
