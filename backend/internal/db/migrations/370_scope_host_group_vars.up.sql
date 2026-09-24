-- 370_scope_host_group_vars — advisory host_vars / group_vars projection parsed
-- from scopes.raw_inventory (Ansible-inventory M2, §3.3). Feeds the inventory UI
-- and (cap C) the ssh_hosts import. Cascade-on-scope-delete.
CREATE TABLE scope_host_vars (
  scope_id TEXT NOT NULL REFERENCES scopes(id) ON DELETE CASCADE,
  host     TEXT NOT NULL,
  key      TEXT NOT NULL,
  value    TEXT,
  PRIMARY KEY (scope_id, host, key)
);

CREATE TABLE scope_group_vars (
  scope_id   TEXT NOT NULL REFERENCES scopes(id) ON DELETE CASCADE,
  group_name TEXT NOT NULL,
  key        TEXT NOT NULL,
  value      TEXT,
  PRIMARY KEY (scope_id, group_name, key)
);
