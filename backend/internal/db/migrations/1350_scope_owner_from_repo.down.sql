-- 1350_scope_owner_from_repo (down) — every new scope is born Global's again,
-- and any scope may be moved.
--
-- The scopes that were born an agency's keep the agency they have: it is a
-- membership row like any other, and the version this goes back to treats it
-- as an operator's assignment.

DROP TRIGGER scope_agencies_repo_fixed;

DROP TRIGGER scopes_born_global;
CREATE TRIGGER scopes_born_global AFTER INSERT ON scopes
BEGIN
    INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id) VALUES (NEW.id, 'global');
END;
