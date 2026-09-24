-- 010 Git sync history table (B3).
-- Records every sync attempt (webhook or poll) with outcome and stats.
-- Queried by GET /api/v1/git/history.

CREATE TABLE git_sync_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    triggered_by TEXT NOT NULL CHECK (triggered_by IN ('poll','webhook','manual')),
    sha         TEXT,           -- HEAD SHA after sync (null on error)
    status      TEXT NOT NULL CHECK (status IN ('success','failed','partial')),
    jobs_synced   INTEGER NOT NULL DEFAULT 0,
    wfs_synced    INTEGER NOT NULL DEFAULT 0,
    scopes_synced INTEGER NOT NULL DEFAULT 0,
    error_message TEXT,
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL
);
CREATE INDEX idx_git_sync_events_started ON git_sync_events(started_at);
