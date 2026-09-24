-- 1040_alert_target_uid — AF-4b stage R2-4: an alert rule targets a job by its
-- permanent identity (the rbac2 plan).
--
-- alert_config.job_name (migration 040) is a BARE name with no source column —
-- the last name-keyed edge outside the definition tables themselves, and the
-- only one where the consequence of ambiguity is a rule that quietly watches
-- the wrong department's job. The kind ('job' vs 'workflow') was already
-- separated once before for exactly this reason; the source never was.
--
-- Backfill follows the R2-1 activity rule, because the same constraint applies:
-- with no source column, a name present in BOTH pools cannot be resolved, so it
-- resolves to NULL and the rule keeps matching by name exactly as it does
-- today. A NULL job_uid is not a broken rule — it is a rule that has not been
-- narrowed yet, and the matcher treats it that way.

ALTER TABLE alert_config ADD COLUMN job_uid TEXT;

UPDATE alert_config SET job_uid =
    (SELECT j.uid FROM jobs j
      WHERE j.name = alert_config.job_name
        AND (SELECT COUNT(*) FROM jobs j2 WHERE j2.name = j.name) = 1)
    WHERE job_name IS NOT NULL AND job_name != '';
