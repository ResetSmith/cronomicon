-- 260 Cancellable workflow runs (workflow-builder-update.md — WB-S2, D4 soft cancel).
--
-- A soft-cancel stops dispatching further steps, marks not-started children
-- 'skipped', lets in-flight child runs finish, and ends the run as cancelled. The
-- workflow_runs.status CHECK has no 'cancelled' value, and the table can't be
-- rebuilt to add one without disturbing the real runs.workflow_run_id FK (see
-- 170's FK note). So cancellation is recorded as an additive flag column; the raw
-- status stays a CHECK-legal terminal ('failure'), and the serializers map a
-- cancelled run to the display status 'cancelled'. cancelled_at records when.
ALTER TABLE workflow_runs ADD COLUMN cancelled INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workflow_runs ADD COLUMN cancelled_at TEXT;
