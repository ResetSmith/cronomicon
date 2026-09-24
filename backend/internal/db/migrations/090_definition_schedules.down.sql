-- Reverse 090.
DROP TRIGGER IF EXISTS trg_def_schedules_workflow_delete;
DROP TRIGGER IF EXISTS trg_def_schedules_job_delete;
ALTER TABLE workflow_runs DROP COLUMN env_json;
ALTER TABLE workflow_runs DROP COLUMN schedule_name;
ALTER TABLE runs DROP COLUMN env_json;
ALTER TABLE runs DROP COLUMN schedule_name;
DROP TABLE IF EXISTS definition_schedules;
