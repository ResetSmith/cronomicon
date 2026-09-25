-- 990_role_compose — Compose becomes a grantable permission (AF-2,
-- the af2-compose-permission plan).
--
-- Authoring cronomicon-source jobs and workflows was gated by RequireRole("admin")
-- — the only role check in the codebase — because v20 decision Q-5 deferred a
-- real permission until the scope enforcement behind it existed. AF-1 built the
-- missing half: every job now states its agency, so "may this actor author HERE"
-- is finally answerable, and the verb can become data like the other six.
--
-- DEFAULT 0 is the safe direction: an existing custom role gains nothing on
-- upgrade, and admin is granted explicitly below. The 800 seed's four built-ins
-- are updated in place rather than re-inserted, so an operator's edits to those
-- rows (description, rank, other permissions) survive this migration.
--
-- Note what this column does NOT open. The old admin gate also covered
-- schedule-defs, calendars, reactions and the revisions/recycle-bin — objects
-- with no scope of their own, where "agency-bound" has no meaning yet. Those
-- stay admin-only (requireComposeAdmin), and so does any All-scoped job: that
-- is what keeps RB-30's unscoped-scheduling bypass unreachable.
ALTER TABLE roles ADD COLUMN compose INTEGER NOT NULL DEFAULT 0 CHECK (compose IN (0,1));

UPDATE roles SET compose = 1 WHERE name = 'admin';
