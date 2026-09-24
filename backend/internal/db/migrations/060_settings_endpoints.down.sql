-- Reverse 060
ALTER TABLE gitlab_config DROP COLUMN pat_enc;
ALTER TABLE gitlab_config DROP COLUMN bot_name;
ALTER TABLE gitlab_config DROP COLUMN bot_email;
ALTER TABLE gitlab_config DROP COLUMN write_branch;
ALTER TABLE gitlab_config DROP COLUMN repo_url;
ALTER TABLE gitlab_config DROP COLUMN token_expiry_notify_days;
ALTER TABLE gitlab_config DROP COLUMN webhook_enabled;
ALTER TABLE gitlab_config DROP COLUMN webhook_events_push;
ALTER TABLE gitlab_config DROP COLUMN webhook_events_mr;
ALTER TABLE gitlab_config DROP COLUMN webhook_events_tag;
ALTER TABLE gitlab_config DROP COLUMN webhook_secret_enc;
ALTER TABLE gitlab_config DROP COLUMN webhook_secret_prev_enc;
ALTER TABLE gitlab_config DROP COLUMN webhook_overlap_until;

DROP TABLE IF EXISTS vault_config;
DROP TABLE IF EXISTS log_storage_config;
