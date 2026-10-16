-- 1490_change_log_acting_agency — a global administrator's change made while
-- working in an agency records both (2.4.4 S3; the 2.5.0 plan's IN-9): "X,
-- acting in Y". NULL for every other change, and for every change before.
ALTER TABLE change_log ADD COLUMN acting_agency TEXT;
