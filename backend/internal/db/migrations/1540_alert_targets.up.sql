-- 1540_alert_targets — a push target is a row with an owner, and a rule names
-- the targets it sends to (2.5.0 C4, AL-2; IN-6).
--
-- Until now the targets were one JSON list on the notification_config
-- singleton (apprise_targets), and every push went to every target. They move
-- here as Global's, in their order (position), and every existing rule is
-- linked to every one of them: what each rule pushed to, it still pushes to.
-- A target the list stored disabled moves disabled. The old column is left as
-- it was and read by nothing (the down migration goes back to it).
--
-- The Apprise gateway (apprise_url) and the SMTP relay stay one install-wide
-- setting on notification_config.

CREATE TABLE alert_targets (
    id               TEXT PRIMARY KEY,
    owner_agency     TEXT NOT NULL DEFAULT 'global',
    label            TEXT NOT NULL DEFAULT '',
    service          TEXT NOT NULL DEFAULT '',
    url              TEXT NOT NULL DEFAULT '',
    enabled          INTEGER NOT NULL DEFAULT 1,
    position         INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT,
    created_at       TEXT NOT NULL,
    last_modified_by TEXT,
    last_modified_at TEXT
);
CREATE INDEX idx_alert_targets_owner ON alert_targets(owner_agency, position);

CREATE TABLE alert_rule_targets (
    rule_id   TEXT NOT NULL REFERENCES alert_config(id) ON DELETE CASCADE,
    target_id TEXT NOT NULL REFERENCES alert_targets(id) ON DELETE CASCADE,
    PRIMARY KEY (rule_id, target_id)
);
CREATE INDEX idx_alert_rule_targets_target ON alert_rule_targets(target_id);

-- The stored list has one shape since 1150: an array of
-- {label, service, url, enabled} objects. An element that is not an object
-- was skipped by its one reader (settings.ParseAppriseTargets), and is here.
-- json_extract gives a JSON true as 1; an absent "enabled" read as false.
INSERT INTO alert_targets (id, owner_agency, label, service, url, enabled, position,
                           created_by, created_at, last_modified_by, last_modified_at)
SELECT lower(hex(randomblob(16))), 'global',
       COALESCE(json_extract(e.value, '$.label'), ''),
       COALESCE(json_extract(e.value, '$.service'), ''),
       COALESCE(json_extract(e.value, '$.url'), ''),
       CASE WHEN json_extract(e.value, '$.enabled') = 1 THEN 1 ELSE 0 END,
       e.key,
       nc.last_modified_by, COALESCE(nc.last_modified_at, strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
       nc.last_modified_by, nc.last_modified_at
FROM notification_config nc, json_each(nc.apprise_targets) e
WHERE nc.id = 1 AND json_valid(nc.apprise_targets) AND e.type = 'object';

INSERT INTO alert_rule_targets (rule_id, target_id)
SELECT r.id, t.id FROM alert_config r, alert_targets t;

-- An agency is deleted only when it is empty (1450, 1530): its targets too.
DROP TRIGGER agencies_no_delete_in_use;
CREATE TRIGGER agencies_no_delete_in_use BEFORE DELETE ON agencies
WHEN EXISTS (SELECT 1 FROM scope_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM runner_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secret_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM env_var_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credential_agencies WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM secrets WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM env_vars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_credentials WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM ssh_hosts WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM bastions WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM runners WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM jobs WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM workflows WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM schedules WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM calendars WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM git_repos WHERE agency_id = OLD.id)
  OR EXISTS (SELECT 1 FROM alert_config WHERE owner_agency = OLD.id)
  OR EXISTS (SELECT 1 FROM alert_targets WHERE owner_agency = OLD.id)
BEGIN
    SELECT RAISE(ABORT, 'agency_in_use: the agency still has scopes, runners, secrets, variables, keys, host records, bastions, definitions, calendars, a repository, alert rules or push targets');
END;
