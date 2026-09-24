-- 580 runner operator tags. Runners are self-registered agents, NOT a Git-synced
-- catalog, so unlike jobs/scripts there is no sync-preservation ripple — tags are
-- plain operator-owned SQLite metadata, edited from a runner's expanded row in the
-- Runners view and full-replaced via PUT /runner-tags/{runnerId}. Mirrors the
-- operator-authored tag pattern of the other tagged entities (tags-support.md).
ALTER TABLE runners ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
