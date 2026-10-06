-- Reverse 1160 — drop the scope tags column. LOSSY by nature: the tags exist
-- nowhere else (they are never in Git), so a rollback discards them.
ALTER TABLE scopes DROP COLUMN tags;
