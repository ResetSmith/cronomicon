-- RB-19 step 4 — drop the two-axis access model.
--
-- `ad_group_mappings` (the verb axis) and `scope_restrictions` (the reach axis)
-- were the whole of authorization until v0.56.5, when RB-15 made `access_grants`
-- authoritative. They have decided nothing since. Migration 810 backfilled grants
-- from them and they were kept deliberately, as the rollback window for the two
-- narrowing releases — a window that only has value while a deployed instance
-- might need to go back.
--
-- Ordering this migration was the hard part, not writing it. `GET /rbac-preflight`
-- READ both tables and was itself the gate on deploying v0.56.4/.5, so dropping
-- them early would have destroyed the tool that authorized the releases this drop
-- is supposed to follow. v0.57.8 retired the report's conversion sections first
-- (RB-19 step 3); this migration is step 4, and it is safe only in that order.
--
-- NOT REVERSIBLE IN THE SENSE THAT MATTERS. See 850_drop_legacy_access.down.sql:
-- the down migration recreates the SHAPE, never the DATA. A grant set cannot be
-- losslessly re-derived into the two-axis form, because that form cannot express
-- per-grant scoping — collapsing grants back into two independent unions is
-- exactly the cross-product leak (viewer@tax + operator@finance ⇒ trigger tax)
-- that access_grants exists to close. Restoring the old tables would re-open it.
--
-- No foreign keys reference either table (checked across the schema), so these are
-- plain drops — no table rebuild, no FK-cascade hazard.

DROP INDEX IF EXISTS idx_ad_group_mappings_group;
DROP TABLE IF EXISTS ad_group_mappings;
DROP TABLE IF EXISTS scope_restrictions;
