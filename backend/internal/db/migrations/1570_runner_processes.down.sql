-- 1570_runner_processes (down). The previous binary reads no process value:
-- every started=1 closes the runner's running runs again.
ALTER TABLE runners DROP COLUMN lost_runs_before;
ALTER TABLE runners DROP COLUMN identity_shared_at;
DROP TABLE IF EXISTS runner_processes;
