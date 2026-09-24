-- Down 350 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate350RoundTrip).
ALTER TABLE scopes DROP COLUMN projection_json;
ALTER TABLE scopes DROP COLUMN projection_status;
ALTER TABLE scopes DROP COLUMN inventory_format;
ALTER TABLE scopes DROP COLUMN raw_inventory;
