-- 420_agencies — the agency (network-isolation zone) catalog (agency-support.md
-- M1, §3.1). A first-class, operator-managed registry: runners are assigned to
-- agencies (M2, runner_agencies) and a scope binds to the one agency its hosts
-- live in (430, scopes.agency_id), so a job dispatches only to a runner that can
-- reach the right network (M3, hard isolation). Mirrors the env_vars/scopes
-- registry column shape; `name` UNIQUE is the human key AND the dispatch key
-- snapshotted onto runs.agency in M3.
CREATE TABLE agencies (
  id               TEXT PRIMARY KEY,
  name             TEXT NOT NULL UNIQUE,
  description      TEXT,
  created_by       TEXT,
  created_at       TEXT NOT NULL,
  last_modified_by TEXT,
  last_modified_at TEXT
);
