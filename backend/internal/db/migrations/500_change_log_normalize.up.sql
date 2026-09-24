-- CC.13 / CC-D2: standardize the change_log optional columns on empty strings.
--
-- The workflow.InsertChangeLog writer previously stored SQL NULLs for target and
-- details, where settings.WriteChangeLog stored empty strings. The unified
-- auditlog writer now always stores empty strings, so normalize the older
-- NULL-bearing rows to match. Every reader already coerces NULL→"" (sql.NullString
-- / nullStrVal), so this changes only the stored representation, not any read.
UPDATE change_log SET target = '' WHERE target IS NULL;
UPDATE change_log SET details = '' WHERE details IS NULL;
