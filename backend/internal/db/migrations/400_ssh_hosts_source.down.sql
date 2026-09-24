-- Down 400 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate400RoundTrip).
DROP INDEX ux_ssh_hosts_inv;
ALTER TABLE ssh_hosts DROP COLUMN synced_at;
ALTER TABLE ssh_hosts DROP COLUMN scope_id;
ALTER TABLE ssh_hosts DROP COLUMN source;
