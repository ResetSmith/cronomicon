-- Reverse 1080.
--
-- Cheaper to lose than 1070's columns: an override is operator intent that is
-- re-settable from job detail in seconds, and the DECLARED pin it was masking is
-- still in the repository. What a rollback DOES do is silently un-mask every
-- declared pin an operator had deliberately overridden — including the
-- force-unpinned ('') ones, whose jobs go back to being pinned. On a fleet where
-- someone overrode a pin because the declared tag's runners were gone, that
-- means those jobs stop dispatching again.
ALTER TABLE jobs DROP COLUMN runner_tag_override;

-- Restore the 1070 (BINARY-collated) projection so a rollback lands on exactly
-- the schema 1070 defined. Re-derived from runners.tags, which neither migration
-- touches, so nothing is lost — but note that a fleet carrying two casings of the
-- same tag will regain two distinct rows here, and case-insensitive pin matching
-- stops working. See RT-G9.
DROP INDEX IF EXISTS idx_runner_tags_tag;
DROP TABLE IF EXISTS runner_tags;

CREATE TABLE runner_tags (
  runner_id TEXT NOT NULL REFERENCES runners(id) ON DELETE CASCADE,
  tag       TEXT NOT NULL,
  PRIMARY KEY (runner_id, tag)
);
CREATE INDEX idx_runner_tags_tag ON runner_tags (tag);

INSERT OR IGNORE INTO runner_tags (runner_id, tag)
SELECT rn.id, je.value
FROM runners rn, json_each(COALESCE(rn.tags, '[]')) je
WHERE je.value IS NOT NULL AND je.value <> '';
