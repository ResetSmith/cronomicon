-- 070 Execution update: in-app SSH executor (execution-update.md EX.1/EX.3/EX.4).
-- Adds the job command source, the per-run/per-job executor, and host-key
-- material for strict SSH host-key verification.

-- EX.1 — job executable source. Exactly one of command/script/script_path is
-- set per job (validated at parse time); run_type selects the interpreter.
ALTER TABLE jobs ADD COLUMN command     TEXT;  -- inline one-liner
ALTER TABLE jobs ADD COLUMN script      TEXT;  -- inline multi-line body
ALTER TABLE jobs ADD COLUMN script_path TEXT;  -- repo-relative file, read from the clone

-- EX.3 — executor selection. jobs.executor is the per-job default (NULL ⇒
-- resolve from run_type at trigger time); runs.executor is frozen at trigger.
ALTER TABLE jobs ADD COLUMN executor TEXT CHECK (executor IN ('runner','ssh'));
ALTER TABLE runs ADD COLUMN executor TEXT NOT NULL DEFAULT 'ssh'
    CHECK (executor IN ('runner','ssh'));

-- EX.4 / EX-D3 — strict host-key verification material (known-hosts line, base64
-- authorized-key form, or fingerprint). NULL ⇒ unverified until TOFU-captured.
ALTER TABLE ssh_hosts ADD COLUMN host_key TEXT;
