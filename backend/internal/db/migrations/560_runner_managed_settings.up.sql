-- 560 Server-managed runner settings (runner-install-update-2.md Phase 4, D3/D5).
--
-- Operational knobs the operator edits on the runner's row in the UI and that
-- ride to the agent over the existing poll channel (additive within protocol
-- v4). managed_settings is a JSON object of tri-state overrides — a key present
-- means "the server has an opinion, override the agent's local value"; a key
-- absent means "no opinion, keep the local (declared) value". Managed keys:
-- maxConcurrent, sandboxMemoryMax, sandboxCpuQuota, sandboxTasksMax,
-- allowCheckout, checkoutRepos, capabilityMask (subtract-only — narrows the
-- runner's claimed capabilities, never widens).
--
-- settings_version bumps on every write; the agent echoes the version it has
-- APPLIED as the poll's settingsVersion param, and settings_acked_version
-- records it — so the UI can show a pending-ack state and the server stops
-- re-sending once acked == version. NULL managed_settings = no server opinion
-- on anything (the default).
ALTER TABLE runners ADD COLUMN managed_settings TEXT;
ALTER TABLE runners ADD COLUMN settings_version INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runners ADD COLUMN settings_acked_version INTEGER NOT NULL DEFAULT 0;
