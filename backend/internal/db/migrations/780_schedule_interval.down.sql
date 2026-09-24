-- Reverse the Phase 2 interval column.
--
-- NOTE: an interval-driven or one-shot entry stores cron = '', so after this
-- migration such rows carry no firing rule at all and simply never fire — the
-- same inert state as a 'Manual' schedule. That is the honest outcome: without
-- the column there is no way to express what they meant, and silently
-- inventing a cron for them would fire jobs at times nobody chose.
ALTER TABLE definition_schedules DROP COLUMN interval;
ALTER TABLE schedules DROP COLUMN interval;
