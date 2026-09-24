-- Down 430 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate430RoundTrip).
ALTER TABLE scopes DROP COLUMN agency_id;
