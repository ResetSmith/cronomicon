-- 1550_agency_groups — the groups an agency's grants may name (2.5.0 C5, GA-1).
--
-- A grant is `ad_group → role → agency`. An agency's administrators who hold
-- manageRoles there (its delegates, AF-3) write their agency's grants, and
-- until 2.5.0 could name any directory group at all: "Domain Users" included.
-- A global administrator now lists the groups each agency may use, and a
-- delegate's grant must name one of them. Global administrators are not bound.
--
-- The lists start empty (question 83): every existing agency grant names a
-- group no list holds. Those grants keep working (GA-2), the notices inbox
-- lists them, and a delegate may delete one but not edit it. Global has no
-- list: no grant names it (LR-9).
--
-- A group is matched exactly, as login matches a grant's group (question 85).
CREATE TABLE agency_groups (
    agency_id  TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
    ad_group   TEXT NOT NULL CHECK (ad_group <> ''),
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (agency_id, ad_group)
);
