-- Reverse 670. Dropping the join tables loses only MEMBERSHIP rows — never a
-- scope, secret, variable or key. scopes.agency_id is untouched (it is still the
-- live 1:1 binding through Phase 2's dual-write), so a down/up round-trip
-- reconstructs the scope side exactly; operator-assigned membership that has
-- DIVERGED from agency_id since the migration is the one thing a down loses.
DROP TABLE ssh_credential_agencies;
DROP TABLE env_var_agencies;
DROP TABLE secret_agencies;
DROP TABLE scope_agencies;
