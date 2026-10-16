-- 1410_scope_owner (down) — a scope's agency is its membership rows alone again.

DROP TRIGGER IF EXISTS scope_owner_on_join;
DROP TRIGGER IF EXISTS scope_owner_on_leave;
DROP INDEX IF EXISTS idx_scopes_owner_name;
ALTER TABLE scopes DROP COLUMN owner_agency;
