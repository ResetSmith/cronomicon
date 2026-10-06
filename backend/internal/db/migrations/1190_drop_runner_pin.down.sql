-- Reverse 1190 — bring back the pin's storage so a pre-1190 binary can run.
--
-- LOSSY by nature: jobs.runner_tag returns EMPTY. The pins that existed are
-- gone from the column; the ones 1180 converted live on as scope bindings, and
-- the ones it could not are listed in retired_runner_pins, from where an
-- operator could re-enter them by hand. Until then every job is unpinned,
-- which a pre-1190 binary treats as "any eligible runner" — and it does not
-- read scope_runners at all. Check before applying to anything long-lived:
--
--   SELECT sc.name, sr.runner_name FROM scope_runners sr JOIN scopes sc ON sc.id = sr.scope_id;
--
-- The projection is rebuilt from runners.tags exactly as 1080 built it, NOCASE
-- included, so a pin re-entered by hand matches the way it used to.
ALTER TABLE jobs ADD COLUMN runner_tag TEXT;

CREATE TABLE runner_tags (
  runner_id TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  tag       TEXT NOT NULL COLLATE NOCASE,
  PRIMARY KEY (runner_id, tag)
);
CREATE INDEX idx_runner_tags_tag ON runner_tags (tag);

INSERT OR IGNORE INTO runner_tags (runner_id, tag)
SELECT rn.id, je.value
FROM runners rn, json_each(COALESCE(rn.tags, '[]')) je
WHERE je.value IS NOT NULL AND je.value <> '';
