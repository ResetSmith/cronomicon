-- Restore the column (inert) so the migration is reversible. Matches the original
-- definition from migration 170: NOT NULL DEFAULT 0, CHECK in {0,1}.
ALTER TABLE jobs ADD COLUMN sensitive_logging INTEGER NOT NULL DEFAULT 0 CHECK (sensitive_logging IN (0,1));
