-- 130 runner inventory mode + token→runner binding (runners-update.md R1, D8 + R1.4).
--
-- D8 — per-runner inventory canonicality: a runner registers as either
-- 'cronomicon' (manifest carries fully-resolved scope_hosts→ssh_hosts targets;
-- fits T-a) or 'local' (manifest carries only the scope name; the agent
-- resolves hosts against its own inventory; fits T-b network-isolated segments).
ALTER TABLE runners ADD COLUMN inventory TEXT NOT NULL DEFAULT 'cronomicon'
    CHECK (inventory IN ('cronomicon','local'));

-- R1.4 — bind a runner API key to its owning runner so the manifest + log
-- endpoints can authorize by run ownership (run.runner_id == caller.runnerId).
-- Set at registration when the per-runner amt_run_* key row is inserted.
-- Nullable: registration-token rows (registration_tokens table) are separate and
-- legacy runner_tokens rows predate the binding.
ALTER TABLE runner_tokens ADD COLUMN runner_id TEXT;
