-- 1310_git_repos — the repository an installation syncs from is a ROW, one of
-- several to come (2.4.0, GR-1, GR-3 and GR-12; Phase R2).
--
-- Until now there was exactly one repository and it was spread over two
-- singleton tables: `gitlab_config` (the connection: URL, branch, token,
-- webhook policy and secrets) and `git_sync_state` (what the last sync saw).
-- Both were `CHECK (id = 1)`, and every reader said `WHERE id=1`. 2.4.0 gives
-- each agency a repository of its own, so the two become one table with a row
-- per repository.
--
-- This migration changes nothing about how many repositories there are. It
-- writes exactly one row, Global's, from the two singletons, and stamps every
-- row that came from Git with it. The routes, the screens and the sync still
-- know one repository; the phases after this one add the rest.
--
--   * `git_repos.id`: Global's is the fixed id 'global' (repoid.Global), which
--     scripts (1290) and schedules (1300) already carry in `repo_id`.
--   * `git_repos.agency_id`: the agency the repository belongs to. UNIQUE and
--     never empty: an agency has at most one repository, and a repository's
--     agency never changes (GR-2). No foreign key and no cascade, like the
--     owner columns of 1230 and 1250: an agency that has a repository cannot be
--     deleted (GR-28), and the route that refuses is the control.
--   * The webhook flags DEFAULT 1. In `gitlab_config` they defaulted to 0 and
--     the reader failed open on a missing row, so the first write that created
--     the row without naming them (the first webhook-secret rotation on an
--     installation configured by environment alone) turned the webhook off.
--   * Global's row is written ALWAYS, also when neither singleton had a row
--     (an installation configured by environment, or not configured at all),
--     with the webhook on: what such an installation effectively ran.
--
-- Not carried: `gitlab_config`'s five original columns (`base_url`,
-- `project_path`, `pat_secret_id`, `webhook_secret`, `branch`). Since
-- migration 060 only the demo seed wrote them, and one activity line read two
-- of them. `webhook_secret` held a secret in clear; it goes with the table.

CREATE TABLE git_repos (
    id                       TEXT PRIMARY KEY,
    agency_id                TEXT NOT NULL UNIQUE,
    url                      TEXT NOT NULL DEFAULT '',
    branch                   TEXT NOT NULL DEFAULT 'main',
    token_enc                TEXT,
    bot_name                 TEXT NOT NULL DEFAULT 'cronomicon-bot',
    bot_email                TEXT NOT NULL DEFAULT 'cronomicon-bot@cronomicon.io',
    token_expiry_notify_days INTEGER NOT NULL DEFAULT 7,
    webhook_enabled          INTEGER NOT NULL DEFAULT 1 CHECK (webhook_enabled IN (0,1)),
    webhook_events_push      INTEGER NOT NULL DEFAULT 1 CHECK (webhook_events_push IN (0,1)),
    webhook_events_mr        INTEGER NOT NULL DEFAULT 1 CHECK (webhook_events_mr IN (0,1)),
    webhook_events_tag       INTEGER NOT NULL DEFAULT 1 CHECK (webhook_events_tag IN (0,1)),
    webhook_secret_enc       TEXT,
    webhook_secret_prev_enc  TEXT,
    webhook_overlap_until    TEXT,
    -- What the last sync of this repository saw (was git_sync_state).
    last_sha                 TEXT,
    last_synced_at           TEXT,
    last_status              TEXT,
    created_by               TEXT,
    created_at               TEXT,
    last_modified_by         TEXT,
    last_modified_at         TEXT
);

-- Global's row. The LEFT JOINs make it one row whether or not either singleton
-- has one. `gitlab_config` never recorded who created it, so created_* stay
-- NULL for this row.
INSERT INTO git_repos (
    id, agency_id, url, branch, token_enc, bot_name, bot_email, token_expiry_notify_days,
    webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
    webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until,
    last_sha, last_synced_at, last_status, last_modified_by, last_modified_at)
SELECT 'global', 'global',
       COALESCE(c.repo_url, ''),
       COALESCE(NULLIF(c.write_branch, ''), 'main'),
       c.pat_enc,
       COALESCE(NULLIF(c.bot_name, ''), 'cronomicon-bot'),
       COALESCE(NULLIF(c.bot_email, ''), 'cronomicon-bot@cronomicon.io'),
       COALESCE(c.token_expiry_notify_days, 7),
       COALESCE(c.webhook_enabled, 1),
       COALESCE(c.webhook_events_push, 1),
       COALESCE(c.webhook_events_mr, 1),
       COALESCE(c.webhook_events_tag, 1),
       c.webhook_secret_enc, c.webhook_secret_prev_enc, c.webhook_overlap_until,
       st.last_sha, st.last_synced_at, st.last_status,
       c.last_modified_by, c.last_modified_at
  FROM (SELECT 1) one
  LEFT JOIN gitlab_config  c  ON c.id = 1
  LEFT JOIN git_sync_state st ON st.id = 1;

-- ── Every row that came from Git records its repository (GR-3) ──────────────
--
-- Scripts and schedules have had the column since 1290 and 1300. A row built
-- in the app has no repository: NULL.
ALTER TABLE jobs      ADD COLUMN repo_id TEXT;
ALTER TABLE workflows ADD COLUMN repo_id TEXT;
ALTER TABLE scopes    ADD COLUMN repo_id TEXT;
UPDATE jobs      SET repo_id = 'global' WHERE source = 'git';
UPDATE workflows SET repo_id = 'global' WHERE source = 'git';
UPDATE scopes    SET repo_id = 'global' WHERE source = 'git';

-- The two histories: every sync and every publish so far was Global's.
ALTER TABLE git_sync_events ADD COLUMN repo_id TEXT NOT NULL DEFAULT 'global';
ALTER TABLE schedule_pushes ADD COLUMN repo_id TEXT NOT NULL DEFAULT 'global';

-- The repository a checkout run's commit was pinned from. NULL for every run
-- so far, which reads as Global's; stamped at enqueue from Phase R5 on.
ALTER TABLE runs ADD COLUMN checkout_repo TEXT;

DROP TABLE gitlab_config;
DROP TABLE git_sync_state;
