-- Reverse 630. Remove every "*" (AllScopes) grant, reverting those roles to an
-- empty (no allowed=1 rows) state — which, under the pre-A5 code this rollback
-- returns to, once again reads as unrestricted. This also removes any "*" an
-- operator added post-migration via the matrix (indistinguishable from the backfill).
DELETE FROM scope_restrictions WHERE scope = '*';
