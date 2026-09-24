-- Reverse 910. Dropping the table takes its index with it.
--
-- Every service-account credential is destroyed by this migration: the token
-- hashes live nowhere else and the plaintexts were never stored, so a
-- down-then-up leaves every integration unauthenticated and needing fresh
-- tokens. That is the correct behavior for a credential store — a reversible
-- secret is not a secret — but it is the reason this down is not routine.
DROP INDEX IF EXISTS idx_service_accounts_active;
DROP TABLE IF EXISTS service_accounts;
