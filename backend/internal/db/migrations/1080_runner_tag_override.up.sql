-- 1080_runner_tag_override — RT-2: the OPERATOR layer of the runner pin
-- (the runner-targeting plan, RT-Q7 as re-decided on 2026-08-14).
--
-- 1070 gave every job a single `runner_tag`. RT-Q7 then split the question in
-- two, because the pin has two legitimate owners who need it at different
-- speeds:
--
--   · `jobs.runner_tag` is the DECLARED pin. It joins the job YAML spec and the
--     sync upsert, so a git-source job can say "I must reach the DMZ" in the
--     repository, in review, alongside its siblings `executor`, `target_host`
--     and `ssh_credential`. Git owns it and sync overwrites it, exactly like
--     `description`.
--   · `jobs.runner_tag_override` — THIS column — is the operator layer. The
--     person who discovers a placement problem is holding a running fleet, not
--     a merge request, and for a git-source job the declared pin is unreachable
--     from the app by construction. This column is what they write instead.
--
-- SYNC-PRESERVED BY OMISSION, not by being somewhere else. It stays out of the
-- sync upsert's column list and its ON CONFLICT DO UPDATE SET — precisely the
-- mechanism `jobs.tags` has used since D6 (sync.go:1589) and `scripts.tags`
-- since 280. Nothing else is required: an untouched column keeps its value
-- through an upsert.
--
-- ⚠️ The two columns therefore follow OPPOSITE rules while sitting adjacent on
-- the same table, and both failure modes are silent (RT-G7):
--   · adding this column to the sync upsert wipes every operator repoint in the
--     fleet on the next sync;
--   · leaving `runner_tag` OUT of it makes the YAML key parse and do nothing.
-- TestRunnerTagOverrideSurvivesSync and TestRunnerTagSyncsFromYAML guard the
-- two directions respectively. Do not "make them consistent".
--
-- TRI-STATE, and the distinction is semantic (RT-G8):
--   NULL → no override; defer to jobs.runner_tag
--   ''   → FORCE-UNPINNED; a declared pin is deliberately overridden to nothing
--   'x'  → override the declared pin with x
-- Any code path that normalizes '' to NULL (a NULLIF, a COALESCE, a JSON zero
-- value) silently converts "force-unpinned" into "no opinion" and resurrects
-- the declared pin. NULL for every existing row means this migration changes no
-- behaviour by itself.
ALTER TABLE jobs ADD COLUMN runner_tag_override TEXT;

-- ── Tag matching is CASE-INSENSITIVE (RT-G9) ────────────────────────────────
--
-- Found while wiring RT-2's YAML path, not anticipated by the plan. 1070 created
-- `runner_tags.tag` with the default BINARY collation, so claimRun's
-- `rt.tag = runs.runner_tag` was an exact byte match — while `tagutil.Normalize`
-- de-dupes tags case-INSENSITIVELY and keeps the first casing seen. The two rules
-- disagree, and the disagreement only shows up once something writes a pin:
--
--   · a job declaring `runner_tag: vlan-DMZ` in YAML would never match a fleet
--     tagged `vlan-dmz`, and the run would queue forever with a reason string
--     naming a tag that visibly exists in the Runners view;
--   · two runners could carry `DMZ` and `dmz` as genuinely distinct projection
--     rows, so a pin would reach one of them and not the other, unpredictably.
--
-- Recollating the column fixes both, and is preferable to lower-casing the stored
-- values: the tag stays displayable exactly as the operator typed it, the PK
-- index remains a covering seek (verified by EXPLAIN QUERY PLAN — SEARCH rt USING
-- COVERING INDEX, not a scan), and the PK now REJECTS casing duplicates, which
-- makes the projection agree with tagutil's de-dupe rule instead of contradicting
-- it. No collation is needed on `jobs.runner_tag` / `runs.runner_tag`: SQLite
-- takes the collation from the column operand in the comparison, which is always
-- this one.
--
-- Rebuild rather than ALTER — SQLite cannot change a column's collation in place,
-- and this table is a pure projection of runners.tags, so dropping and re-deriving
-- it costs nothing and cannot lose operator data.
DROP INDEX IF EXISTS idx_runner_tags_tag;
DROP TABLE IF EXISTS runner_tags;

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
