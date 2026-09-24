-- 710_entity_codes — stable per-entity codes for run-log folders
-- (the logging-update plan LU-6). Additive only: one new table plus one
-- new column, so no table rebuild — which in turn means no trigger-recreation
-- risk and no rowid reassignment.
--
-- WHY A TABLE AND NOT A COLUMN. `jobs` and `workflows` have no surrogate key.
-- What the API exposes as `id` is SQLite's implicit rowid, a physical storage
-- address, and it is destroyed by: a sync prune-and-re-add; any table-rebuild
-- migration (170 and 490 both did `INSERT INTO jobs_new SELECT …; DROP TABLE
-- jobs;` without preserving rowids, and the next CHECK-widening migration will
-- do it again); and potentially the nightly VACUUM INTO restore path, since
-- SQLite may renumber rowids for tables with no explicit INTEGER PRIMARY KEY.
-- A directory tree keyed on rowid would silently reshuffle under all three.
--
-- The obvious alternative — add an AUTOINCREMENT column — is ILLEGAL here.
-- AUTOINCREMENT requires the column be INTEGER PRIMARY KEY, there can be exactly
-- one per table, and both tables already have PRIMARY KEY (source, name), which
-- is the entire premise of the dual-source model. Hence a separate table whose
-- sole INTEGER PRIMARY KEY can carry it.
--
-- AUTOINCREMENT is load-bearing, not decoration: it guarantees SQLite never
-- reuses a freed value, so a newly created entity can never inherit a dead
-- one's log folder. That is what makes it safe to leave dead folders in place
-- (LU-Q5(a)) rather than moving or deleting them.
CREATE TABLE entity_codes (
    code       INTEGER PRIMARY KEY AUTOINCREMENT,  -- legal here: sole INTEGER PK
    kind       TEXT NOT NULL,                      -- 'job' | 'workflow'
    source     TEXT NOT NULL,                      -- 'git' | 'amadeus'
    name       TEXT NOT NULL,
    created_at TEXT NOT NULL,
    deleted_at TEXT                                -- NULL = live (LU-Q6(b))
);

-- No CHECK on `kind` deliberately. A CHECK constraint can only be widened by
-- rebuilding the table, and this file exists precisely because rebuilds destroy
-- identity here. The two writers are internal/entitycode's constants.

-- At most one LIVE code per tuple. Historical rows are unconstrained, so the
-- same (kind, source, name) may appear many times over the system's lifetime
-- with a different code each era — that is LU-Q6(b), an entity deleted and later
-- recreated is deliberately a different entity. A plain
-- UNIQUE(kind, source, name) would block the second allocation and is therefore
-- NOT usable.
CREATE UNIQUE INDEX idx_entity_codes_live
    ON entity_codes(kind, source, name) WHERE deleted_at IS NULL;

-- Covers the history lookup (all eras of a tuple) and the live-row SELECT that
-- every allocation starts with.
CREATE INDEX idx_entity_codes_lookup ON entity_codes(kind, source, name);

-- The run's log folder, stamped at enqueue time rather than resolved at
-- log-write time. `runs` references job_name + job_source with NO foreign key,
-- so resolving at write time would break for a job deleted mid-run; stamping at
-- enqueue makes the destination a property of the run.
--
-- NULL is meaningful and permanent for pre-migration runs: their logs are at the
-- flat {logDir}/{traceID}.log path and stay there (LU-Q8(a) — no backfill, the
-- two layouts coexist). The path builder reads NULL as "flat", so no fallback
-- search is needed, and this column must never be back-filled for old runs.
ALTER TABLE runs ADD COLUMN entity_code TEXT;

-- Backfill every existing definition as LIVE. Ordered by name so the assignment
-- is reproducible rather than dependent on physical row order.
INSERT INTO entity_codes (kind, source, name, created_at)
SELECT 'job', source, name, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM jobs ORDER BY source, name;

INSERT INTO entity_codes (kind, source, name, created_at)
SELECT 'workflow', source, name, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
FROM workflows ORDER BY source, name;
