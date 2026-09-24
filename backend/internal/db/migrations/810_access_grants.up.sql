-- 810_access_grants — one grant table replacing the two-axis model (RB-12).
--
-- ⚠️ INERT IN THIS RELEASE. The table is created and backfilled; NOTHING reads it
-- for an authorization decision. That is deliberate and copied from the agencies
-- program: a deployed installation cannot change behavior on upgrade, the RB-4
-- pre-flight can be read against REAL data, and the predicate flips one release
-- later (RB-15). Merging the two would mean discovering the tightening in
-- production instead of in a report.
--
-- WHY THIS TABLE EXISTS. Authorization ran on two axes resolved INDEPENDENTLY and
-- both OR-unioned: permissions over roles, scopes over roles. A user in
-- tax-viewers (viewer→tax) AND finance-operators (operator→finance) resolved to
-- permissions {trigger} and scopes {tax, finance} — and could therefore trigger
-- TAX jobs. The axes never met, so any multi-role user got the strongest verb
-- applied to the widest scope set. Invisible with one department; the default
-- outcome with two dozen. A grant keeps the association the flat lists destroy:
-- THIS role, HERE.
--
-- THE "WHERE" IS AN AGENCY OR "*", NOTHING ELSE (RB-Q1). No single-scope shape:
-- the agency list IS the department taxonomy, so a narrow need gets a deliberate
-- narrow agency — self-documenting in a way a loose per-scope grant is not — and
-- the UI is an agency dropdown plus one checkbox rather than a shape selector.
-- Scope rename/delete consequently never touch this table at all.
CREATE TABLE access_grants (
    id               TEXT PRIMARY KEY,           -- UUIDv7
    ad_group         TEXT NOT NULL,              -- matched EXACTLY, as login does
    role             TEXT NOT NULL,
    agency_id        TEXT REFERENCES agencies(id) ON DELETE CASCADE,
    all_scopes       INTEGER NOT NULL DEFAULT 0 CHECK (all_scopes IN (0,1)),
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT,
    -- Exactly one shape per row: an agency, or the "*" sentinel. This is the line
    -- that keeps A5's tri-state intact while adding the agency shape, and it makes
    -- a row readable without a join.
    CHECK ((agency_id IS NOT NULL) + all_scopes = 1)
);

-- Login-path lookup, mirroring idx_ad_group_mappings_group.
CREATE INDEX idx_access_grants_group ON access_grants(ad_group);
-- The delete-guard / matrix scan direction, matching the 670 convention.
CREATE INDEX idx_access_grants_agency ON access_grants(agency_id);

-- Dedupe. Must be an EXPRESSION index with COALESCE, not a plain UNIQUE
-- constraint: SQLite treats NULLs as DISTINCT in UNIQUE, so a bare constraint
-- would happily accept two identical "*"-shaped rows (both with agency_id NULL).
CREATE UNIQUE INDEX idx_access_grants_dedupe
    ON access_grants (ad_group, role, COALESCE(agency_id,''), all_scopes);

-- ── Backfill: a CONVERSION, not a copy ──────────────────────────────────────
--
-- Today's scope_restrictions rows are scope-shaped and grants cannot be, so each
-- named-scope restriction is re-expressed through the agencies containing that
-- scope. This is NOT behavior-preserving in either direction, and both directions
-- are enumerated by GET /rbac-preflight (RB-4) so an operator sees them BEFORE the
-- predicate flips:
--
--   WIDENING — the agencies containing the restricted scope also hold other
--   scopes, so the converted grant reaches more than the row it replaces. Fixed by
--   curating the agency taxonomy, not by editing grants.
--
--   ORPHAN DROP — a restricted scope belonging to NO agency produces no grant, so
--   that access disappears. The pre-flight reports these as a hard blocker.
--
-- Multi-role users additionally narrow BY DESIGN — that is the leak being closed.
-- INSERT OR IGNORE so the dedupe index makes this safe to re-run.

-- Unrestricted rows convert to a "*"-shaped grant, losslessly.
INSERT OR IGNORE INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at)
SELECT lower(hex(randomblob(16))), m.ad_group, m.role, NULL, 1, 'migration:810', datetime('now')
FROM ad_group_mappings m
JOIN scope_restrictions r ON r.role = m.role AND r.allowed = 1 AND r.scope = '*';

-- Named-scope rows become one grant per agency containing that scope. A role that
-- also holds "*" is already unrestricted, so its named rows add nothing — the
-- WHERE NOT EXISTS keeps the conversion from emitting redundant agency grants
-- beside a "*" grant that subsumes them.
INSERT OR IGNORE INTO access_grants (id, ad_group, role, agency_id, all_scopes, created_by, created_at)
SELECT DISTINCT lower(hex(randomblob(16))), m.ad_group, m.role, sa.agency_id, 0, 'migration:810', datetime('now')
FROM ad_group_mappings m
JOIN scope_restrictions r ON r.role = m.role AND r.allowed = 1 AND r.scope <> '*'
JOIN scopes s            ON s.name = r.scope
JOIN scope_agencies sa   ON sa.scope_id = s.id
WHERE NOT EXISTS (
    SELECT 1 FROM scope_restrictions u
    WHERE u.role = m.role AND u.allowed = 1 AND u.scope = '*'
);

-- A mapping whose role has ZERO allowed scopes produces NO grant rows, matching
-- today's "empty row ⇒ zero scope access" (A5). That equivalence is deliberate and
-- is reported by the pre-flight as grantlessMappings so it is visibly intended.
