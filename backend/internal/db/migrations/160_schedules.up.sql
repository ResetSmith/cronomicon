-- 160 First-class Schedules (amadeus-v20.md — A10a).
--
-- A Schedule becomes a standalone, browsable, REFERENCEABLE primitive (cron +
-- optional env), the way A8 promoted Scripts. A job/workflow may reference one or
-- more named schedules via `scheduleRefs` (resolved into the runtime
-- definition_schedules expansion at sync time) OR keep an inline schedule (A10a:
-- ref-or-inline, mutually compatible). The owner-scoped definition_schedules table
-- (migration 090) is retained as the runtime "what fires when" expansion cache.
--
-- Born DUAL-SOURCE aware (PK (source,name)) so Phase 2's dual-source work needs no
-- rebuild of this table. GitLab is the source of truth for source='git' rows;
-- source='amadeus' rows are operator-authored in-app (Phase 3+). Like
-- scripts/jobs/definition_schedules, the git rows are a Git-derived read-model cache.
CREATE TABLE schedules (
    name             TEXT NOT NULL,
    source           TEXT NOT NULL DEFAULT 'git' CHECK (source IN ('git','amadeus')),
    description      TEXT,
    cron             TEXT NOT NULL,
    env              TEXT,                  -- optional JSON map[string]string; plaintext if git (§3.7)
    -- 'sha256:'-prefixed digest of cron+env, for run-snapshot reproducibility (§5.5).
    content_hash     TEXT NOT NULL,
    source_path      TEXT,                  -- schedules/<name>.yaml in git (NULL for amadeus)
    synced_at        TEXT,                  -- git only
    -- S4 provenance for operator-authored (source='amadeus') rows.
    created_by       TEXT,
    created_at       TEXT,
    last_modified_by TEXT,
    last_modified_at TEXT,
    PRIMARY KEY (source, name)
);

-- Sync bookkeeping: count schedules synced alongside jobs/scripts/workflows/scopes.
ALTER TABLE git_sync_events ADD COLUMN schedules_synced INTEGER NOT NULL DEFAULT 0;
