-- 1310_git_repos (down) — one repository again, in its two singleton tables.
--
-- Lossy by nature: only Global's repository goes back. The connection of any
-- other repository is dropped (its token and webhook secrets with it); the
-- rows it supplied stay, and read as Global's, as every Git row did. The five
-- original columns of gitlab_config come back empty: nothing read them.

CREATE TABLE gitlab_config (
    id               INTEGER PRIMARY KEY CHECK (id = 1), -- singleton row
    base_url         TEXT,
    project_path     TEXT,
    pat_secret_id    TEXT REFERENCES secrets(id) ON DELETE SET NULL,
    webhook_secret   TEXT,
    branch           TEXT NOT NULL DEFAULT 'main',
    last_modified_by TEXT,
    last_modified_at TEXT,
    pat_enc TEXT,
    bot_name TEXT NOT NULL DEFAULT 'cronomicon-bot',
    bot_email TEXT NOT NULL DEFAULT 'cronomicon-bot@cronomicon.io',
    write_branch TEXT NOT NULL DEFAULT 'main',
    repo_url TEXT NOT NULL DEFAULT '',
    token_expiry_notify_days INTEGER NOT NULL DEFAULT 7,
    webhook_enabled INTEGER NOT NULL DEFAULT 0 CHECK (webhook_enabled IN (0,1)),
    webhook_events_push INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_push IN (0,1)),
    webhook_events_mr INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_mr IN (0,1)),
    webhook_events_tag INTEGER NOT NULL DEFAULT 0 CHECK (webhook_events_tag IN (0,1)),
    webhook_secret_enc TEXT,
    webhook_secret_prev_enc TEXT,
    webhook_overlap_until TEXT
);

CREATE TABLE git_sync_state (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_sha    TEXT,
    last_synced_at TEXT,
    last_status TEXT
);

INSERT INTO gitlab_config (
    id, pat_enc, bot_name, bot_email, write_branch, repo_url, token_expiry_notify_days,
    webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
    webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until,
    last_modified_by, last_modified_at)
SELECT 1, token_enc, bot_name, bot_email, branch, url, token_expiry_notify_days,
       webhook_enabled, webhook_events_push, webhook_events_mr, webhook_events_tag,
       webhook_secret_enc, webhook_secret_prev_enc, webhook_overlap_until,
       last_modified_by, last_modified_at
  FROM git_repos WHERE id = 'global';

-- A sync state row only if a sync ever recorded one, as before.
INSERT INTO git_sync_state (id, last_sha, last_synced_at, last_status)
SELECT 1, last_sha, last_synced_at, last_status
  FROM git_repos
 WHERE id = 'global' AND (last_sha IS NOT NULL OR last_synced_at IS NOT NULL OR last_status IS NOT NULL);

ALTER TABLE runs            DROP COLUMN checkout_repo;
ALTER TABLE schedule_pushes DROP COLUMN repo_id;
ALTER TABLE git_sync_events DROP COLUMN repo_id;
ALTER TABLE scopes          DROP COLUMN repo_id;
ALTER TABLE workflows       DROP COLUMN repo_id;
ALTER TABLE jobs            DROP COLUMN repo_id;

DROP TABLE git_repos;
