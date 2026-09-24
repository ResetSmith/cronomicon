-- Reverse 700 by restoring the columns and re-deriving them from the join tables
-- that superseded them.
--
-- LOSSY BY CONSTRUCTION, and the loss is the point of the whole change: a scalar
-- cannot hold a set. A scope belonging to two agencies collapses to ONE here (the
-- alphabetically first, deterministic rather than arbitrary), and a run whose
-- snapshot named several agencies keeps only the first. The join tables and
-- agencies_json are untouched, so re-running the up migration restores full
-- fidelity — the scalars are the derived values, not the source.
ALTER TABLE runs   ADD COLUMN agency TEXT;
ALTER TABLE scopes ADD COLUMN agency_id TEXT REFERENCES agencies(id);

UPDATE runs
SET agency = (SELECT je.value FROM json_each(COALESCE(runs.agencies_json, '[]')) je
              ORDER BY je.value LIMIT 1)
WHERE COALESCE(agencies_json, '[]') <> '[]';

UPDATE scopes
SET agency_id = (SELECT sa.agency_id FROM scope_agencies sa
                 JOIN agencies a ON a.id = sa.agency_id
                 WHERE sa.scope_id = scopes.id
                 ORDER BY a.name LIMIT 1);
