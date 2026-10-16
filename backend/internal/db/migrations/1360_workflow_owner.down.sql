-- 1360_workflow_owner (down) — a workflow has no recorded owner again; whose
-- it is, is worked out from its steps' jobs, as before.

ALTER TABLE workflows DROP COLUMN owner_agency;

-- The notice listing workflows whose jobs span agencies is of this column's
-- reading; the older code neither writes nor resolves it (added in 2.4.2).
DELETE FROM notices WHERE kind = 'workflow_spans_agencies';
