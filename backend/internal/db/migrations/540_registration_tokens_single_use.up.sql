-- 540 Single-use registration tokens (runner-install-update.md Phase 7, D6).
--
-- Registration tokens become per-install, first-contact-only credentials with
-- an audit trail: multiple rows may be active at once (mint no longer revokes
-- prior tokens), each row dies on its first successful registration, recording
-- which runner consumed it. Resync (Phase 4) authenticates with the runner's
-- own amt_run_* key, so a dead install token never blocks a resync; only a
-- reaped/deregistered runner needs a fresh token.
--
-- Existing rows (the old shared token) grandfather as unlabeled single-use
-- rows until their 24h expiry. AMADEUS_RUNNER_BOOTSTRAP_TOKEN is unchanged
-- (env-configured, multi-use, no row).
ALTER TABLE registration_tokens ADD COLUMN label TEXT;              -- operator-chosen, e.g. the intended runner name
ALTER TABLE registration_tokens ADD COLUMN used_at TEXT;            -- set atomically on the consuming registration
ALTER TABLE registration_tokens ADD COLUMN used_by_runner_id TEXT;  -- runners.id of the consumer (audit trail; no FK — outlives the runner row)
