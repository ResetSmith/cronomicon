-- 960_file_watch_triggers — start a job when a file arrives (ET-D,
-- the prod-features plan §1.2).
--
-- Fortra's pitch for file triggers is that they kill the "pad the schedule and
-- hope the file arrived" pattern. Cronomicon could start a run from cron, a click,
-- a GitLab push, another run's completion (RX) or a service-account token
-- (ET-B) — but nothing that watches a filesystem.
--
-- Additive only: one column and one table. No rebuild.

-- The watch spec, authored ON THE JOB (decision, 2026-08-11).
--
-- Beside `schedule:` and `requestable:` rather than in an operator-owned table,
-- so the trigger is visible to anyone reading the job's YAML and is
-- code-reviewed with it. It joins the other environment-specific job fields
-- (target_host, requires, ssh_user) that already live on the spec — a watched
-- path is no less portable than a pinned host.
--
-- Shape: a JSON array of {path, stableSeconds}. NULL/absent ⇒ this job is not
-- file-triggered, which is every job today.
ALTER TABLE jobs ADD COLUMN watch_json TEXT;

-- The seen ledger — what makes a file fire ONCE.
--
-- Keyed on the FILE's identity (job + path + size + mtime), NOT on the runner
-- that saw it. Two runners sharing an NFS mount both report the same arrival;
-- de-duping per-runner would fire the job twice, which for a job that ingests
-- the file is a double-import. The runner that won the race is recorded for
-- provenance, not for identity.
--
-- Size AND mtime are both in the key so a file REPLACED at the same path fires
-- again — which is the common case for a nightly drop that reuses a filename —
-- while an unchanged file sitting in the directory does not re-fire on every
-- poll.
CREATE TABLE file_watch_sightings (
    id           TEXT PRIMARY KEY,          -- UUIDv7
    job_source   TEXT NOT NULL,
    job_name     TEXT NOT NULL,
    path         TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL,
    mtime        TEXT NOT NULL,             -- RFC3339 UTC, as the agent observed it
    runner_id    TEXT,                      -- who reported it first (provenance only)
    seen_at      TEXT NOT NULL,
    -- The run this arrival started, NULL when the sighting was recorded but the
    -- trigger was refused (a gate said no). A sighting with no run is the
    -- durable answer to "the file landed, why did nothing happen?".
    run_id       TEXT,
    refused_reason TEXT
);

-- The de-dupe. A UNIQUE index rather than a check-then-insert: two runners
-- reporting the same arrival in the same second is the ordinary case, not the
-- race, and the constraint is what makes the second one a no-op rather than a
-- second run.
CREATE UNIQUE INDEX uq_file_watch_sightings
    ON file_watch_sightings(job_source, job_name, path, size_bytes, mtime);

-- The retention reaper's scan, and the "recent arrivals for this job" read.
CREATE INDEX idx_file_watch_sightings_job
    ON file_watch_sightings(job_source, job_name, seen_at);
