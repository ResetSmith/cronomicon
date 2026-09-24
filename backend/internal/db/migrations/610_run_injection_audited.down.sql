-- Reverse 610. Drop the dispatch-injection audit-once flag.
ALTER TABLE runs DROP COLUMN injection_audited;
