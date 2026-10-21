-- 1600_secret_uses (down). Lossy: the uses recorded and the actors' relations
-- go with the table and the column.
ALTER TABLE change_log DROP COLUMN actor_relation;
DROP INDEX IF EXISTS secret_uses_owner_at;
DROP INDEX IF EXISTS secret_uses_row_at;
DROP TABLE IF EXISTS secret_uses;
