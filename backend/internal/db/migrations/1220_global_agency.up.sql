-- 1220_global_agency — Global becomes a real, built-in agency (LR-8, LR-21, LR-22,
-- LR-26, LR-28; the LR band, v2.3.0).
--
-- Until this migration "global" was the ABSENCE of a membership row, and absence
-- meant a different thing for each kind of entity:
--
--   secret, variable, key   usable by every agency's runs; writes global-admin only
--   runner                  the general pool: claims only runs with no agency
--   scope                   visible to global administrators only
--   run                     agencies_json = '[]': claimable by general-pool runners
--
-- Each of those is now a row that names one agency, 'global'. Three things make
-- that an invariant of the database and not a convention of the code:
--
--   1. BACKFILL. Every scope, runner, secret, variable and key with no membership
--      gets a Global row. Every unowned secret, variable and key gets an owner.
--      Every waiting run with an empty snapshot is stamped Global.
--   2. BIRTH. A trigger gives every NEW scope, runner, secret, variable and key a
--      Global row the moment it is inserted, and every new waiting run with an
--      empty snapshot the Global stamp. There is no instant at which a row
--      exists and belongs to no agency — which is what lets the readers treat
--      "no rows" as an error instead of as a meaning.
--   3. LEAVING. Naming any other agency for an entity takes it out of Global (a
--      trigger removes the Global row), and Global cannot be added beside a named
--      agency (a trigger refuses it). An entity is Global's or an agency's, never
--      both (LR-25) — "*" mixed with named scopes was the same mistake.
--
-- What the triggers deliberately do NOT do is put an entity back in Global when
-- its last named agency is removed. Making something Global is a decision — a
-- Global secret is usable by every agency — so it is never a side effect of a
-- delete. The setters refuse a write that would leave an entity with no agency
-- (LR-26), and a row that reaches that state anyway is reported as an error.
--
-- The code that reads these tables changes in the same release, in the same
-- commit: every `NOT EXISTS (membership)` arm became "is a member of Global".
-- Running this migration under the previous binary, or the new binary without
-- it, breaks dispatch and secret resolution in both directions.

-- ── The catalog ─────────────────────────────────────────────────────────────
ALTER TABLE agencies ADD COLUMN builtin INTEGER NOT NULL DEFAULT 0 CHECK (builtin IN (0, 1));

-- ── The notices inbox (LR-85) ───────────────────────────────────────────────
-- Created here, ahead of its API, because this migration has something to say
-- that only it can know (an agency it had to rename). A notice is a STANDING
-- CONDITION keyed by (kind, subject): the check that finds the condition upserts
-- the row and resolves it when the cause is gone; dismissing only hides it and
-- records who. No foreign key to agencies: a notice about an agency must be able
-- to outlive it, as the host-key ledger outlives its runner.
CREATE TABLE notices (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    agency_id     TEXT NOT NULL,
    subject       TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    resolved_at   TEXT,
    dismissed_at  TEXT,
    dismissed_by  TEXT,
    UNIQUE (kind, subject)
);
CREATE INDEX idx_notices_open ON notices (agency_id, resolved_at, dismissed_at);

-- ── An existing agency already called Global (LR-27) ────────────────────────
-- The name is reserved in every letter case. An agency that holds it is renamed,
-- and — because a run stores its agencies by NAME — the new name is carried onto
-- every run that has not finished, exactly as an operator's rename is
-- (settings.renameAgencyOnWaitingRuns): queued and running rows, their index
-- rows, and parked snapshots, whose AgenciesJSON is a STRING holding the array.
-- All of it runs BEFORE the built-in row is inserted. A notice records the
-- rename; it is the one notice this migration writes, because it is the one
-- fact nothing can work out afterwards.
--
-- If "<name> (renamed)" is itself taken the UNIQUE on agencies.name fails the
-- migration. That is the right outcome for a catalog in that state: rename one
-- of the two by hand and start again.
--
-- A snapshot that is not JSON is left to the waiting-run backfill below (it has
-- no index rows, so it is Global's); json_each on it would abort the migration.
UPDATE runs
   SET agencies_json = (
         SELECT json_group_array(CASE WHEN lower(je.value) = 'global' THEN je.value || ' (renamed)' ELSE je.value END)
           FROM json_each(runs.agencies_json) je)
 WHERE status IN ('queued', 'running')
   AND json_valid(agencies_json)
   AND json_type(agencies_json) = 'array'
   AND EXISTS (SELECT 1 FROM agencies WHERE lower(name) = 'global')
   AND EXISTS (SELECT 1 FROM json_each(runs.agencies_json) je WHERE lower(je.value) = 'global');

UPDATE run_agencies
   SET agency = agency || ' (renamed)'
 WHERE lower(agency) = 'global'
   AND EXISTS (SELECT 1 FROM agencies WHERE lower(name) = 'global')
   AND run_id IN (SELECT id FROM runs WHERE status IN ('queued', 'running'));

-- The parked snapshot must stay a STRING. json_group_array returns a value of
-- JSON subtype, and json_set would embed that as an array where the scheduler
-- decodes a string: every parked run of the renamed agency would be unreadable
-- and marked missed. Concatenating with '' drops the subtype and leaves text.
UPDATE pending_runs
   SET params_json = json_set(params_json, '$.AgenciesJSON', '' || (
         SELECT json_group_array(CASE WHEN lower(je.value) = 'global' THEN je.value || ' (renamed)' ELSE je.value END)
           FROM json_each(json_extract(pending_runs.params_json, '$.AgenciesJSON')) je))
 WHERE params_json IS NOT NULL
   AND json_valid(params_json)
   AND json_type(params_json, '$.AgenciesJSON') = 'text'
   AND json_valid(json_extract(params_json, '$.AgenciesJSON'))
   AND json_type(json_extract(params_json, '$.AgenciesJSON')) = 'array'
   AND EXISTS (SELECT 1 FROM agencies WHERE lower(name) = 'global')
   AND EXISTS (SELECT 1 FROM json_each(json_extract(pending_runs.params_json, '$.AgenciesJSON')) je
                WHERE lower(je.value) = 'global');

INSERT INTO notices (id, kind, agency_id, subject, detail, first_seen_at, last_seen_at)
SELECT lower(hex(randomblob(16))), 'agency_renamed', 'global', id,
       'The agency "' || name || '" was renamed "' || name || ' (renamed)" by the upgrade to 2.3.0: '
       || 'Global is now the built-in agency of the global administrators, and no other agency may hold '
       || 'that name. Its scopes, runners, secrets, grants and waiting runs are unchanged. Rename it to '
       || 'what it should be called.',
       strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
  FROM agencies WHERE lower(name) = 'global';

UPDATE agencies SET name = name || ' (renamed)' WHERE lower(name) = 'global';

-- ── The built-in agency (LR-21) ─────────────────────────────────────────────
INSERT INTO agencies (id, name, description, builtin, created_by, created_at, last_modified_by, last_modified_at)
VALUES ('global', 'Global',
        'The built-in agency of the global administrators. What belongs to no other agency belongs here.',
        1, 'system', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'), 'system', strftime('%Y-%m-%dT%H:%M:%SZ', 'now'));

-- ── Backfill: membership (LR-22) ────────────────────────────────────────────
INSERT INTO scope_agencies (scope_id, agency_id)
SELECT s.id, 'global' FROM scopes s
 WHERE NOT EXISTS (SELECT 1 FROM scope_agencies m WHERE m.scope_id = s.id);

INSERT INTO runner_agencies (runner_id, agency_id)
SELECT r.id, 'global' FROM runners r
 WHERE NOT EXISTS (SELECT 1 FROM runner_agencies m WHERE m.runner_id = r.id);

INSERT INTO secret_agencies (secret_id, agency_id)
SELECT s.id, 'global' FROM secrets s
 WHERE NOT EXISTS (SELECT 1 FROM secret_agencies m WHERE m.secret_id = s.id);

INSERT INTO env_var_agencies (env_var_id, agency_id)
SELECT v.id, 'global' FROM env_vars v
 WHERE NOT EXISTS (SELECT 1 FROM env_var_agencies m WHERE m.env_var_id = v.id);

INSERT INTO ssh_credential_agencies (credential_id, agency_id)
SELECT c.id, 'global' FROM ssh_credentials c
 WHERE NOT EXISTS (SELECT 1 FROM ssh_credential_agencies m WHERE m.credential_id = c.id);

-- ── Backfill: ownership (LR-22, LR-54) ──────────────────────────────────────
-- "No membership" and "owner_agency = ''" were two populations. Migration 670
-- gave every scoped secret and variable a membership row; migration 830 set every
-- owner to ''. So "unowned, member of exactly one agency" is the ordinary state of
-- an upgraded installation, and it is resolved here by what the row has in fact
-- been: that agency's.
--
--   owner ''   members            becomes
--   ─────────  ─────────────────  ───────────────────────────────────────────────
--   ''         Global (just now)  owner Global
--   ''         exactly one        owner THAT agency, unless one of the three
--                                 exceptions below holds; then owner Global,
--                                 members unchanged
--   ''         several            owner Global, members unchanged
--   set        …                  unchanged
--
-- THE RULE THE EXCEPTIONS SERVE: no run may resolve a different row after this
-- migration than it resolved before it. Giving a row an owner moves it from the
-- shared tier into the owned tier, and the resolver (runref.pickOwned) consults
-- the tier BEFORE the scope: an owned row beats every shared one, whatever their
-- scopes. So a row is promoted only where that cannot change an answer.
--
--   (c) THE AGENCY ALREADY OWNS A ROW WITH THIS KEY (label), at any scope. Same
--       scope: the unowned row is unreachable today (the agency's own row always
--       wins) and promoting it would violate UNIQUE (key, scope, owner_agency).
--       Another scope: a run in the unowned row's scope resolves the agency's
--       own row today and would resolve this one tomorrow.
--       For a secret this also looks at the agency's VARIABLES, and for a
--       variable at its secrets: the two share one namespace per owner (a save
--       refuses a secret and a variable with one key, scope and owner), so
--       promoting a secret beside the agency's own same-named variable would
--       leave a pair neither of which could be saved again, not even to rotate
--       its value.
--   (b) THE ROW IS GLOBAL-SCOPED AND ANOTHER UNOWNED ROW WITH ITS KEY STAYS
--       SHARED AND VISIBLE TO THE AGENCY (that row is Global's, or several
--       agencies' including this one). Today the two meet in the shared tier,
--       where the scoped row wins in its own scope and two global-scoped rows are
--       an ambiguity the resolver refuses. Promoted, this row would win both.
--       (A SCOPED row has no such exception: it already wins in its own scope and
--       is invisible in every other.)
--   (d) THE AGENCY SHARES A SCOPE WITH ANOTHER AGENCY and any other row carries
--       this key. A run in a shared scope carries both agencies, so rows that two
--       departments own can meet in one owned tier; with one agency per scope
--       they never do. Deliberately coarse: a scope in several agencies is the
--       state Phase G2 retires, and an administrator resolves these by hand.
--
-- A row an exception holds back is Global-owned with its narrowed member list,
-- which resolves exactly as it did; a boot check lists it with the rows several
-- agencies share (notice kind 'shared_ownership').
--
-- Every test reads ownership and membership AS THEY STOOD BEFORE these updates,
-- from the snapshot tables below. Judged against the live table, the answer
-- would depend on the order the UPDATE visited the rows: of two unowned rows with
-- one key in one agency, whichever came first would be promoted and would then
-- disqualify the other — one in the owned tier, one in the shared, and a run
-- resolving a different row than it did yesterday. `members` and `agency` are
-- the row's member count and (for a row with one member) that member.
CREATE TABLE _mig1220_entangled AS
    SELECT DISTINCT x.agency_id FROM scope_agencies x
      JOIN scope_agencies y ON y.scope_id = x.scope_id AND y.agency_id <> x.agency_id;

CREATE TABLE _mig1220_secrets AS
    SELECT s.id, s.key, COALESCE(s.scope, '') AS scope, s.owner_agency,
           (SELECT COUNT(*) FROM secret_agencies m WHERE m.secret_id = s.id) AS members,
           (SELECT MIN(m.agency_id) FROM secret_agencies m WHERE m.secret_id = s.id) AS agency
      FROM secrets s;
CREATE TABLE _mig1220_env_vars AS
    SELECT v.id, v.key, COALESCE(v.scope, '') AS scope, v.owner_agency,
           (SELECT COUNT(*) FROM env_var_agencies m WHERE m.env_var_id = v.id) AS members,
           (SELECT MIN(m.agency_id) FROM env_var_agencies m WHERE m.env_var_id = v.id) AS agency
      FROM env_vars v;
CREATE TABLE _mig1220_keys AS
    SELECT c.id, c.label, c.owner_agency,
           (SELECT COUNT(*) FROM ssh_credential_agencies m WHERE m.credential_id = c.id) AS members,
           (SELECT MIN(m.agency_id) FROM ssh_credential_agencies m WHERE m.credential_id = c.id) AS agency
      FROM ssh_credentials c;

CREATE INDEX _mig1220_secrets_key ON _mig1220_secrets (key);
CREATE INDEX _mig1220_env_vars_key ON _mig1220_env_vars (key);
CREATE INDEX _mig1220_keys_label ON _mig1220_keys (label);

UPDATE secrets
   SET owner_agency = (SELECT p.agency FROM _mig1220_secrets p WHERE p.id = secrets.id)
 WHERE id IN (
       SELECT p.id FROM _mig1220_secrets p
        WHERE p.owner_agency = '' AND p.members = 1 AND p.agency <> 'global'
          AND NOT EXISTS (SELECT 1 FROM _mig1220_secrets o                       -- (c)
                           WHERE o.key = p.key AND o.owner_agency = p.agency)
          AND NOT EXISTS (SELECT 1 FROM _mig1220_env_vars o
                           WHERE o.key = p.key AND o.owner_agency = p.agency)
          AND NOT (p.scope = '' AND EXISTS (                                     -- (b)
                   SELECT 1 FROM _mig1220_secrets q
                    WHERE q.key = p.key AND q.id <> p.id AND q.owner_agency = ''
                      AND ((q.members = 1 AND q.agency = 'global')
                           OR (q.members > 1 AND EXISTS (
                               SELECT 1 FROM secret_agencies m
                                WHERE m.secret_id = q.id AND m.agency_id = p.agency)))))
          AND NOT (p.agency IN (SELECT agency_id FROM _mig1220_entangled)        -- (d)
                   AND EXISTS (SELECT 1 FROM _mig1220_secrets q WHERE q.key = p.key AND q.id <> p.id)));
UPDATE secrets SET owner_agency = 'global' WHERE owner_agency = '';

UPDATE env_vars
   SET owner_agency = (SELECT p.agency FROM _mig1220_env_vars p WHERE p.id = env_vars.id)
 WHERE id IN (
       SELECT p.id FROM _mig1220_env_vars p
        WHERE p.owner_agency = '' AND p.members = 1 AND p.agency <> 'global'
          AND NOT EXISTS (SELECT 1 FROM _mig1220_env_vars o                      -- (c)
                           WHERE o.key = p.key AND o.owner_agency = p.agency)
          AND NOT EXISTS (SELECT 1 FROM _mig1220_secrets o
                           WHERE o.key = p.key AND o.owner_agency = p.agency)
          AND NOT (p.scope = '' AND EXISTS (                                     -- (b)
                   SELECT 1 FROM _mig1220_env_vars q
                    WHERE q.key = p.key AND q.id <> p.id AND q.owner_agency = ''
                      AND ((q.members = 1 AND q.agency = 'global')
                           OR (q.members > 1 AND EXISTS (
                               SELECT 1 FROM env_var_agencies m
                                WHERE m.env_var_id = q.id AND m.agency_id = p.agency)))))
          AND NOT (p.agency IN (SELECT agency_id FROM _mig1220_entangled)        -- (d)
                   AND EXISTS (SELECT 1 FROM _mig1220_env_vars q WHERE q.key = p.key AND q.id <> p.id)));
UPDATE env_vars SET owner_agency = 'global' WHERE owner_agency = '';

-- A key has no scope and its label is unique per owner, so two unowned keys
-- never share a label: exception (b) cannot arise.
UPDATE ssh_credentials
   SET owner_agency = (SELECT p.agency FROM _mig1220_keys p WHERE p.id = ssh_credentials.id)
 WHERE id IN (
       SELECT p.id FROM _mig1220_keys p
        WHERE p.owner_agency = '' AND p.members = 1 AND p.agency <> 'global'
          AND NOT EXISTS (SELECT 1 FROM _mig1220_keys o                          -- (c)
                           WHERE o.label = p.label AND o.owner_agency = p.agency)
          AND NOT (p.agency IN (SELECT agency_id FROM _mig1220_entangled)        -- (d)
                   AND EXISTS (SELECT 1 FROM _mig1220_keys q WHERE q.label = p.label AND q.id <> p.id)));
UPDATE ssh_credentials SET owner_agency = 'global' WHERE owner_agency = '';

DROP TABLE _mig1220_entangled;
DROP TABLE _mig1220_secrets;
DROP TABLE _mig1220_env_vars;
DROP TABLE _mig1220_keys;

-- ── Backfill: waiting runs (LR-28) ──────────────────────────────────────────
-- A queued or running run with no agency was general-pool work; it is Global's.
-- "No agency" is judged the way the claim judged it until now: by the INDEX
-- (run_agencies), which is what the general-pool arm read. That covers the
-- ordinary '[]' and also a snapshot that is NULL, empty or not JSON — a row the
-- general pool could claim yesterday must be one a Global runner can claim
-- today, not one stranded by its own damage. Finished runs keep what they have:
-- that is history, nothing decides on it, and '[]' is shown as Global. Parked
-- runs need nothing: a parked snapshot of '[]' (or none) becomes a run row with
-- '[]' when it is promoted, and the birth trigger below stamps it then.
UPDATE runs SET agencies_json = '["Global"]'
 WHERE status IN ('queued', 'running')
   AND NOT EXISTS (SELECT 1 FROM run_agencies ra WHERE ra.run_id = runs.id);
INSERT OR IGNORE INTO run_agencies (run_id, agency)
SELECT id, 'Global' FROM runs
 WHERE status IN ('queued', 'running') AND agencies_json = '["Global"]';

-- ── Birth: every new row belongs to Global until it is given an agency ──────
CREATE TRIGGER scopes_born_global AFTER INSERT ON scopes
BEGIN
    INSERT OR IGNORE INTO scope_agencies (scope_id, agency_id) VALUES (NEW.id, 'global');
END;

CREATE TRIGGER runners_born_global AFTER INSERT ON runners
BEGIN
    INSERT OR IGNORE INTO runner_agencies (runner_id, agency_id) VALUES (NEW.id, 'global');
END;

-- The three owned kinds also get their owner. The column DEFAULT is still ''
-- (changing it means rebuilding three tables); the UPDATE here is what makes ''
-- unreachable. If Global already holds this key at this scope the UPDATE trips
-- UNIQUE (key, scope, owner_agency) and the INSERT fails with it, which is the
-- conflict the caller was always told about.
CREATE TRIGGER secrets_born_global AFTER INSERT ON secrets
BEGIN
    UPDATE secrets SET owner_agency = 'global' WHERE id = NEW.id AND owner_agency = '';
    INSERT OR IGNORE INTO secret_agencies (secret_id, agency_id) VALUES (NEW.id, 'global');
END;

CREATE TRIGGER env_vars_born_global AFTER INSERT ON env_vars
BEGIN
    UPDATE env_vars SET owner_agency = 'global' WHERE id = NEW.id AND owner_agency = '';
    INSERT OR IGNORE INTO env_var_agencies (env_var_id, agency_id) VALUES (NEW.id, 'global');
END;

CREATE TRIGGER ssh_credentials_born_global AFTER INSERT ON ssh_credentials
BEGIN
    UPDATE ssh_credentials SET owner_agency = 'global' WHERE id = NEW.id AND owner_agency = '';
    INSERT OR IGNORE INTO ssh_credential_agencies (credential_id, agency_id) VALUES (NEW.id, 'global');
END;

-- A waiting run with no snapshot is Global's. The one writer of runs stamps the
-- snapshot itself; this is for the row it did not write (a parked run promoted
-- from a pre-2.3.0 snapshot, a path that forgot). Terminal rows — skips, missed
-- fires, connection tests — are written finished and are left alone.
CREATE TRIGGER runs_born_global AFTER INSERT ON runs
WHEN COALESCE(NEW.agencies_json, '') IN ('', '[]') AND NEW.status IN ('queued', 'running')
BEGIN
    UPDATE runs SET agencies_json = '["Global"]' WHERE id = NEW.id;
    INSERT OR IGNORE INTO run_agencies (run_id, agency) VALUES (NEW.id, 'Global');
END;

-- ── Leaving: an entity is Global's or an agency's, never both (LR-25) ───────
CREATE TRIGGER scope_agencies_leave_global AFTER INSERT ON scope_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM scope_agencies WHERE scope_id = NEW.scope_id AND agency_id = 'global';
END;
CREATE TRIGGER scope_agencies_no_mix BEFORE INSERT ON scope_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM scope_agencies WHERE scope_id = NEW.scope_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a scope cannot be in Global and in another agency');
END;

CREATE TRIGGER runner_agencies_leave_global AFTER INSERT ON runner_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id = 'global';
END;
CREATE TRIGGER runner_agencies_no_mix BEFORE INSERT ON runner_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM runner_agencies WHERE runner_id = NEW.runner_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a runner cannot serve Global and another agency');
END;

CREATE TRIGGER secret_agencies_leave_global AFTER INSERT ON secret_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM secret_agencies WHERE secret_id = NEW.secret_id AND agency_id = 'global';
END;
CREATE TRIGGER secret_agencies_no_mix BEFORE INSERT ON secret_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM secret_agencies WHERE secret_id = NEW.secret_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a secret cannot be in Global and in another agency');
END;

CREATE TRIGGER env_var_agencies_leave_global AFTER INSERT ON env_var_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM env_var_agencies WHERE env_var_id = NEW.env_var_id AND agency_id = 'global';
END;
CREATE TRIGGER env_var_agencies_no_mix BEFORE INSERT ON env_var_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM env_var_agencies WHERE env_var_id = NEW.env_var_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: a variable cannot be in Global and in another agency');
END;

CREATE TRIGGER ssh_credential_agencies_leave_global AFTER INSERT ON ssh_credential_agencies WHEN NEW.agency_id <> 'global'
BEGIN
    DELETE FROM ssh_credential_agencies WHERE credential_id = NEW.credential_id AND agency_id = 'global';
END;
CREATE TRIGGER ssh_credential_agencies_no_mix BEFORE INSERT ON ssh_credential_agencies
WHEN NEW.agency_id = 'global'
 AND EXISTS (SELECT 1 FROM ssh_credential_agencies WHERE credential_id = NEW.credential_id AND agency_id <> 'global')
BEGIN
    SELECT RAISE(ABORT, 'global_mixed: an SSH key cannot be in Global and in another agency');
END;

-- ── An agency that still holds anything cannot be deleted ───────────────────
-- Every membership table cascades from agencies, so deleting an agency deletes
-- its members' rows — and a scope, runner, secret, variable or key whose ONLY
-- agency that was is left in none, which no reader gives a meaning to and no
-- route can then repair. settings.DeleteAgency has always refused this; it is a
-- read followed by a delete, and the invariant is the database's now, so the
-- refusal is too. Grants and service accounts still cascade: they are access TO
-- the agency, and go with it (the handler refuses while any is live).
CREATE TRIGGER agencies_no_delete_in_use BEFORE DELETE ON agencies
WHEN EXISTS (SELECT 1 FROM scope_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM runner_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secret_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM env_var_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credential_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secrets WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM env_vars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credentials WHERE owner_agency = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables or keys');
END;

-- ── The built-in row is permanent, and no owner is empty ────────────────────
-- An owner is an agency's id. The birth triggers above fill it in on INSERT; an
-- UPDATE cannot empty it again. (No reader gives '' a meaning any more: the
-- resolver would treat such a row as nobody's and no run could use it.)
CREATE TRIGGER secrets_owner_required BEFORE UPDATE OF owner_agency ON secrets WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a secret is owned by an agency, Global at the least');
END;
CREATE TRIGGER env_vars_owner_required BEFORE UPDATE OF owner_agency ON env_vars WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: a variable is owned by an agency, Global at the least');
END;
CREATE TRIGGER ssh_credentials_owner_required BEFORE UPDATE OF owner_agency ON ssh_credentials WHEN NEW.owner_agency = ''
BEGIN
    SELECT RAISE(ABORT, 'owner_required: an SSH key is owned by an agency, Global at the least');
END;

CREATE TRIGGER agencies_builtin_no_delete BEFORE DELETE ON agencies WHEN OLD.builtin = 1
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the Global agency cannot be deleted');
END;
CREATE TRIGGER agencies_builtin_no_rename BEFORE UPDATE OF name, builtin ON agencies
WHEN OLD.builtin = 1 AND (NEW.name <> OLD.name OR NEW.builtin <> 1)
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the Global agency cannot be renamed');
END;
CREATE TRIGGER agencies_global_name_reserved BEFORE INSERT ON agencies
WHEN lower(NEW.name) = 'global' AND NEW.id <> 'global'
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the name Global is reserved');
END;
CREATE TRIGGER agencies_global_name_reserved_update BEFORE UPDATE OF name ON agencies
WHEN lower(NEW.name) = 'global' AND OLD.id <> 'global'
BEGIN
    SELECT RAISE(ABORT, 'builtin_agency: the name Global is reserved');
END;
