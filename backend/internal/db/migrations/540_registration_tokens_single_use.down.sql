-- Reverse 540. Drop the single-use columns (SQLite DROP COLUMN table rebuild,
-- precedented in 330/510/511/512/520/530 down). Reverting re-enables the
-- shared-token semantics; consumption history is lost by design.
ALTER TABLE registration_tokens DROP COLUMN label;
ALTER TABLE registration_tokens DROP COLUMN used_at;
ALTER TABLE registration_tokens DROP COLUMN used_by_runner_id;
