-- 130 down: remove the runner inventory mode and the token→runner binding.
ALTER TABLE runner_tokens DROP COLUMN runner_id;
ALTER TABLE runners DROP COLUMN inventory;
