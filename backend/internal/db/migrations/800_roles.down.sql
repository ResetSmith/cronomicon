-- Reverse 800.
--
-- LOSSY: any CUSTOM role an operator defined is destroyed, and so is every edit to
-- a built-in role's permissions. Rolling back returns the instance to the
-- compiled-in matrix, so ad_group_mappings rows naming a custom role resolve to no
-- permissions at all (an unknown role contributes nothing to the OR-union) — those
-- users become effectively read-only until the mappings are repointed.
DROP TABLE IF EXISTS roles;
