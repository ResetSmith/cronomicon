-- PP-M8: track raw (pre-redaction) bytes received for each run log stream.
-- The server validates X-Resume-Offset against this column instead of the
-- redacted file size, keeping the resume protocol consistent with the runner
-- agent's raw-byte tracking (avoids 409 divergence when secrets are redacted).
ALTER TABLE runs ADD COLUMN log_raw_offset INTEGER NOT NULL DEFAULT 0;
