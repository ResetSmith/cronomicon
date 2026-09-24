-- Reverse 640 (SQLite DROP COLUMN = table rebuild; data-present round-trip test
-- in migrate_test.go TestMigrate640RoundTrip).
ALTER TABLE bastions DROP COLUMN host_key;
