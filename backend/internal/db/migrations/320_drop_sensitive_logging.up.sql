-- 320 Remove the dead jobs.sensitive_logging flag (PP-M9). Log redaction is
-- unconditional (runner/redact.go always masks the known secret dictionary); the
-- per-job flag was set on the jobs row, carried into the manifest, and never read
-- by the agent — it changed nothing. Dropped from the schema along with its UI,
-- API, OpenAPI, and GitOps-YAML surfaces. SQLite performs DROP COLUMN as a table
-- rebuild; no index/CHECK references the column elsewhere so the drop is clean.
ALTER TABLE jobs DROP COLUMN sensitive_logging;
