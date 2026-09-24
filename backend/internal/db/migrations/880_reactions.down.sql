-- Reverse 880.
--
-- ⚠️ Dropping `reactions` destroys the authored edges themselves — not a
-- derived cache. A rollback silently decouples every definition that was
-- reacting to another, and nothing downstream fires again until they are
-- re-authored. There is no "off" state to fall back to the way 870's bindings
-- fell back to "fires whenever its schedule matches": a reaction IS the
-- coupling, so removing it removes the behaviour entirely.
--
-- The delivery log goes with it. Previously fired reactions keep their real
-- runs in History (the runs rows survive, including reacted_to_run_id's value
-- until that column is dropped below), but the record of what was CONSIDERED
-- and suppressed — the "we did not cascade on the holiday, deliberately" trail
-- — does not survive.
DROP INDEX IF EXISTS idx_reaction_deliveries_fired;
DROP INDEX IF EXISTS idx_reaction_deliveries_src;
DROP TABLE IF EXISTS reaction_deliveries;

DROP TRIGGER IF EXISTS trg_reactions_workflow_delete;
DROP TRIGGER IF EXISTS trg_reactions_job_delete;
DROP INDEX IF EXISTS idx_reactions_owner;
DROP INDEX IF EXISTS idx_reactions_on;
DROP TABLE IF EXISTS reactions;

ALTER TABLE pending_runs DROP COLUMN origin_env_json;
ALTER TABLE pending_runs DROP COLUMN reaction_depth;
ALTER TABLE pending_runs DROP COLUMN origin_ref;
ALTER TABLE pending_runs DROP COLUMN origin_kind;

DROP INDEX IF EXISTS idx_workflow_runs_reacted_to;
DROP INDEX IF EXISTS idx_runs_reacted_to;

ALTER TABLE workflow_runs  DROP COLUMN reacted_to_run_id;
ALTER TABLE workflow_runs  DROP COLUMN reaction_depth;
ALTER TABLE runs           DROP COLUMN reacted_to_run_id;
ALTER TABLE runs           DROP COLUMN reaction_depth;
