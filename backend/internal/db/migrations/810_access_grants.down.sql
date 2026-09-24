-- Reverse 810.
--
-- Safe in THIS release precisely because the table is inert: nothing reads
-- access_grants for an authorization decision until RB-15, so dropping it changes
-- no behavior. That stops being true the release the predicate flips.
--
-- LOSSY once grants are authored by hand: a grant set cannot be losslessly
-- re-derived into the two-axis (ad_group_mappings + scope_restrictions) form,
-- because that form CANNOT express per-grant scoping — restoring it collapses
-- every grant back into the union, which is exactly the cross-product leak this
-- table exists to close.
DROP INDEX IF EXISTS idx_access_grants_dedupe;
DROP INDEX IF EXISTS idx_access_grants_agency;
DROP INDEX IF EXISTS idx_access_grants_group;
DROP TABLE IF EXISTS access_grants;
