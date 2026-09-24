-- 110 down: remove git scope columns using DROP COLUMN.
ALTER TABLE scopes DROP COLUMN source_path;
ALTER TABLE scopes DROP COLUMN capability_types;
ALTER TABLE scopes DROP COLUMN capability_json;
ALTER TABLE scopes DROP COLUMN synced_at;
