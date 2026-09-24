-- 630 A5 scope-model fix: behavior-preserving "*" (AllScopes) backfill
-- (the scoping-fix plan §6 Phase 5, DEC-2(i) preserve-then-tighten).
--
-- Before this fix an EMPTY allowed-scopes set meant BOTH "unrestricted (all
-- scopes)" AND "restricted to zero scopes"; every guard read empty as unrestricted.
-- Phase 2 flipped the guards so empty now means ZERO access, and "unrestricted" is
-- the explicit "*" grant. On existing data the common case — a role that was
-- unrestricted simply by having no scope_restrictions rows (a fresh install has an
-- empty table) — would therefore silently flip from ALL to ZERO on deploy and lock
-- operators out.
--
-- To preserve behavior we cannot infer which empty roles were "meant to be zero", so
-- we grant "*" to EVERY built-in role that currently holds NO allowed=1 row (a role
-- with allowed=0 rows but no allowed=1 row resolved to empty ⇒ unrestricted under the
-- old model, so it is backfilled too). Everyone unrestricted today stays unrestricted;
-- everyone with explicit grants keeps their exact scopes. The operator tightens
-- afterward by turning off "All scopes" in the matrix (which now correctly means zero).
--
-- roleValid fixes the role set to admin/approver/operator/viewer; enumerating them
-- makes the backfill complete (a user's role is always one of these).
INSERT INTO scope_restrictions (role, scope, allowed)
SELECT r.role, '*', 1
FROM (
    SELECT 'admin' AS role
    UNION ALL SELECT 'approver'
    UNION ALL SELECT 'operator'
    UNION ALL SELECT 'viewer'
) AS r
WHERE NOT EXISTS (
    SELECT 1 FROM scope_restrictions sr
    WHERE sr.role = r.role AND sr.allowed = 1
);
