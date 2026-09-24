-- 360_scope_groups — advisory group projection parsed from scopes.raw_inventory
-- (Ansible-inventory M2, §3.2). Normalized like scope_hosts, cascade-on-scope-
-- delete. Group/host names written here are charset-validated at ingest (§4.5) so
-- they are safe to feed `ansible --limit`. The projection is ADVISORY: execution
-- always uses the raw inventory; these tables drive the UI and (M3) targeting.
CREATE TABLE scope_groups (
  scope_id TEXT NOT NULL REFERENCES scopes(id) ON DELETE CASCADE,
  name     TEXT NOT NULL,
  PRIMARY KEY (scope_id, name)
);

CREATE TABLE scope_group_hosts (
  scope_id   TEXT NOT NULL,
  group_name TEXT NOT NULL,
  host       TEXT NOT NULL,
  PRIMARY KEY (scope_id, group_name, host),
  FOREIGN KEY (scope_id, group_name) REFERENCES scope_groups(scope_id, name) ON DELETE CASCADE
);

CREATE TABLE scope_group_children (        -- [group:children] nesting
  scope_id TEXT NOT NULL,
  parent   TEXT NOT NULL,
  child    TEXT NOT NULL,
  PRIMARY KEY (scope_id, parent, child),
  FOREIGN KEY (scope_id, parent) REFERENCES scope_groups(scope_id, name) ON DELETE CASCADE
);
