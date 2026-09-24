-- 1130 Log archive (SL-1, the s3-logging plan).
--
-- Run logs live on local disk and are NOT in the database, so a database-only
-- restore keeps every runs row and loses every log those rows point at
-- (20260826-ha-disaster-recovery.md §6 item 5). The `s3` log-storage backend --
-- a settings shell since migration 060, refused with 422 on save -- becomes a
-- post-terminal ARCHIVE TIER: local stays the only write target during a run
-- (live tailing depends on an append-only file with stable byte offsets), and a
-- scheduled sweep copies sealed logs to the bucket afterwards.
--
-- The sweep's whole design is one query -- terminal runs with no archive marker,
-- oldest first -- so the marker lives on the run row and the pending set is a
-- partial index over exactly that predicate.

-- When the run's log was verified present in the bucket (RFC3339). NULL = not
-- archived. Stamped only after the upload's reported size matches the local
-- file's size read BEFORE the upload began (SL-Q11).
ALTER TABLE runs ADD COLUMN log_archived_at TEXT;

-- The "stop trying" answer (SL-Q6). `missing`: the local file was gone before it
-- was ever archived (reaped, repointed away, or a `skipped` run that never had a
-- process) -- without this a misconfigured bucket makes the pending count grow
-- forever and every tick re-stats files that do not exist. `failed`: the retry
-- budget was exhausted on a NON-transient error (AccessDenied, NoSuchBucket);
-- a network error leaves the row pending. `expired`: the archived object was
-- deleted by the archivedLogFiles window (SL-4) -- without it, clearing the
-- marker would put the run back in the pending set and, while its local file
-- outlived the archive window, the next tick would re-upload what the window
-- had just deleted. NULL otherwise.
ALTER TABLE runs ADD COLUMN log_archive_state TEXT
    CHECK (log_archive_state IS NULL OR log_archive_state IN ('missing','failed','expired'));

-- The sweep's query, precomputed. Ordered by completed_at because the sweep
-- archives oldest-first and gates on a grace period after completion (SL-Q12).
CREATE INDEX idx_runs_log_pending
    ON runs(completed_at) WHERE log_archived_at IS NULL AND log_archive_state IS NULL;

-- Settings: the connection gains the one field 060 forgot (backups carry
-- BackupS3UseSSL; the log tier had no way to say http://), the schedule
-- (SL-Q3: interval mode with a 60s floor, or daily at HH:MM UTC), and the
-- archive status the Settings card reads. archived_count/bytes are maintained
-- at upload time, never by listing the bucket on GET (SL-Q13).
ALTER TABLE log_storage_config ADD COLUMN s3_use_ssl            INTEGER NOT NULL DEFAULT 1;
-- A PEM CA bundle for a private S3 node whose certificate the process trust
-- store does not know (an internal MinIO/Ceph). Stored, not a file path: the
-- container's trust store is not the operator's to edit, and a pasted bundle
-- takes effect on save with no restart. Not a secret; returned on read.
ALTER TABLE log_storage_config ADD COLUMN s3_ca_pem             TEXT;
ALTER TABLE log_storage_config ADD COLUMN sync_mode             TEXT NOT NULL DEFAULT 'interval'
    CHECK (sync_mode IN ('interval','daily'));
ALTER TABLE log_storage_config ADD COLUMN sync_interval_sec     INTEGER NOT NULL DEFAULT 900;
ALTER TABLE log_storage_config ADD COLUMN sync_at               TEXT;
ALTER TABLE log_storage_config ADD COLUMN archived_count        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE log_storage_config ADD COLUMN archived_bytes        INTEGER NOT NULL DEFAULT 0;
ALTER TABLE log_storage_config ADD COLUMN last_sync_started_at  TEXT;
ALTER TABLE log_storage_config ADD COLUMN last_sync_finished_at TEXT;
ALTER TABLE log_storage_config ADD COLUMN last_sync_error       TEXT;
