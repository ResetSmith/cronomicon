-- Reverse 1040. Rules fall back to matching purely by name, which is what they
-- did before this migration and what a NULL job_uid already means today.
ALTER TABLE alert_config DROP COLUMN job_uid;
