-- 110 git scopes columns. Add missing columns to the scopes table to support
-- the git-source scope synchronization implemented in internal/gitlab/sync.go.
ALTER TABLE scopes ADD COLUMN source_path        TEXT;
ALTER TABLE scopes ADD COLUMN capability_types   TEXT; -- JSON array of strings
ALTER TABLE scopes ADD COLUMN capability_json    TEXT; -- JSON object for Capability metadata (origin, owner, sidecarPath, errors)
ALTER TABLE scopes ADD COLUMN synced_at          TEXT;
