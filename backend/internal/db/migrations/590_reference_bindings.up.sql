-- 590 Reference bindings (the vault-integration plan, P1.1 — the
-- least-privilege declaration of which Env Vars references a job/script consumes).
-- A job or script declares the reference rows it needs by (kind, bare name); the
-- dispatch-time resolver (P1.2, internal/runref) injects ONLY the declared
-- references, so two jobs sharing a scope no longer see each other's secrets
-- (D2 = 2B explicit binding).
--
-- Rows keep BARE names — the reference AMADEUS_<SECTION>_<name> is DERIVED at use
-- time (envref, namespace plan D6), never stored; ref_kind selects the section
-- (secret → AMADEUS_SECRET_, var → AMADEUS_VAR_, key → AMADEUS_KEY_).
--
-- Owner is the (kind, source, name) triple mirroring definition_schedules'
-- owner_* model: jobs are dual-source ((source,name) PK) so owner_source
-- disambiguates git vs amadeus; scripts are a single-namespace catalog (PK name),
-- stored with owner_source=''. Bindings are full-replaced per owner (the
-- tags/schedules authoring pattern) — the API DELETEs all rows for an owner then
-- INSERTs the new set in one transaction.
CREATE TABLE reference_bindings (
    owner_kind   TEXT NOT NULL CHECK (owner_kind IN ('job','script')),
    owner_source TEXT NOT NULL DEFAULT '',
    owner_name   TEXT NOT NULL,
    ref_kind     TEXT NOT NULL CHECK (ref_kind IN ('secret','var','key')),
    ref_name     TEXT NOT NULL,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    PRIMARY KEY (owner_kind, owner_source, owner_name, ref_kind, ref_name)
);

-- Dispatch resolves a run's bindings by owner; index the owner triple so that
-- lookup is a covering range scan rather than a full-table scan.
CREATE INDEX idx_reference_bindings_owner
    ON reference_bindings (owner_kind, owner_source, owner_name);

-- Cascade binding cleanup on owner deletion (mirrors the migration-200 AFTER
-- DELETE cascade pattern). Without this, a deleted job/script leaves an orphaned
-- reference grant that a LATER same-named owner would silently inherit — a
-- least-privilege violation once injection is live. Triggers cover BOTH delete
-- paths (in-app compose delete AND git-sync removal) uniformly, so neither
-- handler needs a manual DELETE. Jobs are keyed (source, name); scripts by name
-- (owner_source is '' for every script binding).
CREATE TRIGGER reference_bindings_job_cleanup
AFTER DELETE ON jobs
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'job' AND owner_source = OLD.source AND owner_name = OLD.name;
END;

CREATE TRIGGER reference_bindings_script_cleanup
AFTER DELETE ON scripts
BEGIN
    DELETE FROM reference_bindings
    WHERE owner_kind = 'script' AND owner_name = OLD.name;
END;
