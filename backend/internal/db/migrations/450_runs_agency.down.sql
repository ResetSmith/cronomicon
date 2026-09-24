-- Down 450 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate450RoundTrip).
ALTER TABLE runs DROP COLUMN agency;
