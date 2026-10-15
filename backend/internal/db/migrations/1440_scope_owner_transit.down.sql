-- 1440 down: 1420's triggers as they were.
DROP TRIGGER IF EXISTS scope_owner_on_join;
DROP TRIGGER IF EXISTS scope_owner_on_leave;

CREATE TRIGGER scope_owner_on_join AFTER INSERT ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = NEW.scope_id),
        'global')
     WHERE id = NEW.scope_id;
END;

CREATE TRIGGER scope_owner_on_leave AFTER DELETE ON scope_agencies
BEGIN
    UPDATE scopes SET owner_agency = COALESCE(
        (SELECT CASE WHEN COUNT(*) = 1 THEN MIN(sa.agency_id) END FROM scope_agencies sa WHERE sa.scope_id = OLD.scope_id),
        'global')
     WHERE id = OLD.scope_id;
END;
