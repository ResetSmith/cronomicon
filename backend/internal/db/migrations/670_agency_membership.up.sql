-- 670_agency_membership — one membership axis for scopes, secrets, variables and
-- SSH keys (the agencies plan Phase 2, T2.1/T2.2).
--
-- Today four entity types carry isolation by four different mechanisms on two
-- unrelated axes: runners are M:N with agencies (mig. 440), scopes are 1:1
-- (mig. 430), secrets and variables are keyed by SCOPE and are not on the agency
-- axis at all, and SSH keys have no isolation whatsoever. Answering "will this job
-- get this secret?" requires holding five facts at once. These four tables put
-- every one of them on the SAME axis, shaped exactly like runner_agencies:
-- composite PK, ON DELETE CASCADE on both sides.
--
-- NOTHING READS THESE TABLES YET. Phase 2 is deliberately inert: the tables exist
-- and are backfilled, but dispatch (claimRun) and reference resolution (runref)
-- still run the old predicates, so this migration cannot change the behavior of a
-- deployed installation. Phase 3 switches the predicates, and only after the
-- T2.12 pre-flight report has been read against real data.
--
-- Membership is OPERATOR-OWNED, exactly like scopes.agency_id (mig. 430): the
-- GitLab sync must neither insert nor delete rows in scope_agencies, or an
-- operator's assignment would be silently dropped on every pull. See the sibling
-- comment in gitlab/sync.go's upsertScopes.

CREATE TABLE scope_agencies (
  scope_id  TEXT NOT NULL REFERENCES scopes(id)   ON DELETE CASCADE,
  agency_id TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
  PRIMARY KEY (scope_id, agency_id)
);

CREATE TABLE secret_agencies (
  secret_id TEXT NOT NULL REFERENCES secrets(id)  ON DELETE CASCADE,
  agency_id TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
  PRIMARY KEY (secret_id, agency_id)
);

CREATE TABLE env_var_agencies (
  env_var_id TEXT NOT NULL REFERENCES env_vars(id) ON DELETE CASCADE,
  agency_id  TEXT NOT NULL REFERENCES agencies(id) ON DELETE CASCADE,
  PRIMARY KEY (env_var_id, agency_id)
);

CREATE TABLE ssh_credential_agencies (
  credential_id TEXT NOT NULL REFERENCES ssh_credentials(id) ON DELETE CASCADE,
  agency_id     TEXT NOT NULL REFERENCES agencies(id)        ON DELETE CASCADE,
  PRIMARY KEY (credential_id, agency_id)
);

-- The agency side of each table is the direction the delete guard and the Phase-4
-- membership matrix scan ("what belongs to DSS?"). The composite PK already indexes
-- the entity side.
CREATE INDEX idx_scope_agencies_agency          ON scope_agencies (agency_id);
CREATE INDEX idx_secret_agencies_agency         ON secret_agencies (agency_id);
CREATE INDEX idx_env_var_agencies_agency        ON env_var_agencies (agency_id);
CREATE INDEX idx_ssh_credential_agencies_agency ON ssh_credential_agencies (agency_id);

-- ── Backfill (T2.2) ─────────────────────────────────────────────────────────
--
-- The governing rule is that this migration must be BEHAVIOR-PRESERVING, which
-- fixes every case below:
--
--   scopes          → the existing 1:1 agency_id becomes exactly one row.
--   scoped secrets  → inherit the agency of the scope they name. A secret in scope
--   scoped vars       'prod' is reachable today exactly by runs in 'prod', which
--                     dispatch to 'prod's agency — so that agency is the one
--                     membership that changes nothing.
--   UNSCOPED rows   → get NO rows at all. This is the important one: an unscoped
--                     (global) secret is visible from every scope today, and under
--                     AG-Q1(b) an EMPTY membership set is what preserves that. Do
--                     not "helpfully" give global rows membership in every agency;
--                     that is a different predicate with a different failure mode.
--   SSH keys        → no rows. ssh_credentials has no scope column, so there is no
--                     behavior-preserving mapping to infer, and AG-Q5's tightening
--                     is deliberately deferred to Phase 3 behind the T2.12 report.
--   scope with no   → no rows (nothing to inherit; the general pool).
--   agency
--
-- A secret whose scope names a scope row that no longer exists, or whose scope has
-- no agency, simply gets no row — the JOIN drops it. That is the same "no
-- membership ⇒ visible as today" state as an unscoped row, which is correct: such
-- a secret is not reachable from any agency-bound scope today either.

INSERT INTO scope_agencies (scope_id, agency_id)
SELECT id, agency_id FROM scopes WHERE agency_id IS NOT NULL;

INSERT INTO secret_agencies (secret_id, agency_id)
SELECT s.id, sc.agency_id
FROM secrets s
JOIN scopes sc ON sc.name = s.scope
WHERE COALESCE(s.scope, '') <> '' AND sc.agency_id IS NOT NULL;

INSERT INTO env_var_agencies (env_var_id, agency_id)
SELECT v.id, sc.agency_id
FROM env_vars v
JOIN scopes sc ON sc.name = v.scope
WHERE COALESCE(v.scope, '') <> '' AND sc.agency_id IS NOT NULL;
