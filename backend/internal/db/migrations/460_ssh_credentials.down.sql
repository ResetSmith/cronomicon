-- Down 460 (SQLite DROP COLUMN = table rebuild; data-present round-trip test in
-- migrate_test.go TestMigrate460RoundTrip). Drop the referencing FK columns BEFORE
-- the table they point at so the rebuild and table drop are clean under
-- _foreign_keys=on.
ALTER TABLE ssh_hosts DROP COLUMN auth_credential_id;
ALTER TABLE bastions  DROP COLUMN auth_credential_id;
DROP INDEX ux_ssh_credentials_label;
DROP TABLE ssh_credentials;
