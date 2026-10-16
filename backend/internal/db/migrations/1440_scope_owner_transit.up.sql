-- 1440_scope_owner_transit — the owner triggers tolerate a membership rewrite
-- (2.4.0 Phase I2, from its review; §0a, IN-5).
--
-- 1410's triggers keep scopes.owner_agency equal to the membership: the one
-- agency, Global's for none or several. A membership is rewritten by deleting
-- every row and inserting the new one, and between the two the scope has no
-- membership, so the leave trigger made it Global's for one statement. Since
-- 1420 a name is unique within an agency, and when Global holds a scope of
-- that name the UPDATE violated UNIQUE (owner_agency, name) and the DELETE
-- failed: an agency's administrator could not create `web` while Global had
-- `web`, which is exactly what I2 allows.
--
-- The triggers now UPDATE OR IGNORE: a step that would collide leaves the
-- owner as it was, and the last step of the rewrite sets it right. The
-- writers set the owner the membership will mean BEFORE rewriting it, so
-- that a name the target agency already has is refused there, as an error
-- that says so, and the ignored step can never be the final one.
DROP TRIGGER IF EXISTS scope_owner_on_join;
DROP TRIGGER IF EXISTS scope_owner_on_leave;

CREATE TRIGGER scope_owner_on_join AFTER INSERT ON scope_agencies
BEGIN
    UPDATE OR IGNORE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = NEW.scope_id),
        'global')
     WHERE id = NEW.scope_id;
END;

CREATE TRIGGER scope_owner_on_leave AFTER DELETE ON scope_agencies
BEGIN
    UPDATE OR IGNORE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = OLD.scope_id),
        'global')
     WHERE id = OLD.scope_id;
END;
