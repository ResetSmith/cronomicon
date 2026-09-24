-- 120 runs kind discriminator (V1.1-2). Distinguishes ordinary job runs from
-- synthetic "SSH Test connection" runs so History can badge them and the
-- Dashboard/job-failure metrics can exclude diagnostics. SQLite permits a
-- column-level CHECK plus a constant DEFAULT in ADD COLUMN.
ALTER TABLE runs ADD COLUMN kind TEXT NOT NULL DEFAULT 'job'
    CHECK (kind IN ('job','ssh-test'));
