-- 530 Runner resync flag (runner-install-update.md Phase 4 — the manual
-- Resync lever, D2 stage 1).
--
-- POST /api/v1/runners/{id}/resync sets this; the next poll from the runner
-- delivers PollControl{Op:"re-register"} and clears it (deliver-once). The
-- agent (protocol >= 4) then re-declares its current local config in place via
-- the runner-key redeclare endpoint — same id, same API key, no orphan row.
-- The endpoint 409s for protocol < 4 rows (the op would be silently ignored),
-- so the flag is only ever set for agents that can act on it.
ALTER TABLE runners ADD COLUMN resync_requested INTEGER NOT NULL DEFAULT 0;
