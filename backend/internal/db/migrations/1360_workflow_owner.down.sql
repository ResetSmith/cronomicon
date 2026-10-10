-- 1360_workflow_owner (down) — a workflow has no recorded owner again; whose
-- it is, is worked out from its steps' jobs, as before.

ALTER TABLE workflows DROP COLUMN owner_agency;
