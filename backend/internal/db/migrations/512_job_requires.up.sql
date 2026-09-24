-- 512 Job requirement tokens (ansible-update.md Phase 3 — vault opt-in, RX.13;
-- the general `spec.requires` mechanism §5, whose claim-gating lands in Phase 4).
--
-- A job may declare requirement tokens (jobs.requires_json, a JSON array) — the
-- first consumer is `vault`, which sets the checkout manifest's UsesVault so the
-- agent passes --vault-password-file from its OWN config (the password never
-- travels — D1). The token is snapshotted onto the run at enqueue
-- (runs.requires_json, the runs.agency precedent) so Phase 4 can claim-gate
-- against it without a mid-run retarget.
--
-- Both plain nullable/defaulted ADD COLUMN (no rebuild). Follows the
-- prompts_json / env_passthrough precedent for the synced column.
ALTER TABLE jobs ADD COLUMN requires_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE runs ADD COLUMN requires_json TEXT;
