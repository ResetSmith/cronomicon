-- 430_scopes_agency — bind a scope to the one agency its hosts live in
-- (agency-support.md M1, §3.2). Declarative FK to agencies(id); NULL = general /
-- unscoped. OPERATOR-OWNED, NEVER SYNCED: the GitLab sync (upsertScopes) MUST NOT
-- write this column, so an operator's agency assignment survives re-sync — the
-- same operator-owned model as jobs.tags. A scope's required agency is snapshotted
-- onto runs.agency at enqueue in M3 (the dispatch filter).
ALTER TABLE scopes ADD COLUMN agency_id TEXT REFERENCES agencies(id);
