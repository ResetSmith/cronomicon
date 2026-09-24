-- 1150_apprise_targets_object_form — rewrite any pre-rich Apprise target to the
-- object form, so the column has exactly ONE shape on disk
-- (the dd-merge-fixes plan, DM-1).
--
-- apprise_targets (004) is a JSON array. It originally held bare URL strings
-- (["slack://x"]); rich targets ({label,service,url,enabled}) replaced that
-- form, and BOTH readers tolerated both shapes — until v1.5.41 (DD-14) dropped
-- the bare-string branch from the settings reader only. The dispatcher's own
-- copy of the parser kept delivering to bare-string targets, so a row still in
-- the old form showed ZERO targets in Settings while every alert was still
-- being sent, and the first Save of that card rebuilt the column from the empty
-- parsed list and silently stopped delivery.
--
-- v1.5.43 makes both readers share one parser (settings.ParseAppriseTargets),
-- which only works if the data has one shape. This is that backfill: every
-- array element that is a JSON string becomes
-- {"label":"","service":"","url":<string>,"enabled":true} — enabled, because a
-- bare URL was always delivered to. Object elements pass through untouched.
--
-- Two SQLite details this statement depends on, both verified against the
-- bundled 3.53 rather than assumed:
--
--   * The element's type comes from json_each's own `type` COLUMN, never from
--     json_type(value). json_each hands back `value` as a SQL value — for a
--     text element that is the UNQUOTED string (slack://x), which json_type()
--     then tries to parse as a JSON document and fails with "malformed JSON".
--     The first draft of this migration did exactly that and errored on every
--     row it was meant to fix.
--   * The inner json(elem) re-parse is deliberate: json_group_array() inserts a
--     value as JSON only when it carries the JSON subtype, and a CASE does not
--     reliably propagate it, so without the re-parse the array would come back
--     full of QUOTED STRINGS — the exact corruption this migration prevents.
--
-- Guards: LIKE '[%' is a plain text test that cannot itself raise (unlike
-- json_type on a corrupt column) and json_valid keeps an unparseable value
-- diagnosable instead of replacing it with NULL; the EXISTS means a row already
-- in the object form is not rewritten at all, so re-running this on a restored
-- backup is a no-op rather than a reformat.
UPDATE notification_config
SET apprise_targets = (
        SELECT json_group_array(json(elem))
        FROM (
            SELECT CASE
                       WHEN type = 'text'
                           THEN json_object('label', '', 'service', '', 'url', value, 'enabled', json('true'))
                       ELSE value
                   END AS elem
            FROM json_each(notification_config.apprise_targets)
        )
    )
WHERE apprise_targets IS NOT NULL
  AND apprise_targets LIKE '[%'
  AND json_valid(apprise_targets)
  AND EXISTS (
        SELECT 1 FROM json_each(notification_config.apprise_targets)
        WHERE type = 'text'
    );
