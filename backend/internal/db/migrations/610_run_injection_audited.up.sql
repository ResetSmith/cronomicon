-- 610 Dispatch-injection audit-once flag (the vault-integration plan,
-- P1.6). Set when a run's dispatch-time reference injection has been recorded to
-- change_log ("Secrets"/"injected"). The runner manifest can be re-fetched (agent
-- retries), so the injection audit is written exactly once per run, guarded by this
-- flag; the SSH executor sets it too for a uniform "this run's injection is audited"
-- invariant. Defaults 0; only runs that actually inject a reference ever set it.
ALTER TABLE runs ADD COLUMN injection_audited BOOLEAN NOT NULL DEFAULT 0;
