-- Down 390 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate390RoundTrip).
ALTER TABLE runners DROP COLUMN protocol_version;
