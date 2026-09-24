-- Reverse 620. Drop the dispatch-time secret-injection flag.
ALTER TABLE runs DROP COLUMN injects_secret;
