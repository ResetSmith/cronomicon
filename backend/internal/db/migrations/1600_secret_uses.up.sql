-- 1600_secret_uses (2.5.1 SG2.1, SG-10, SG-11).
--
-- secret_uses: one row per stored secret or SSH key a run used, written with
-- the run's dispatch-injection audit row (runref.AuditInjectionOnce) under the
-- same one-time slot, and by the local runner when it connects with a host's
-- or a bastion's key. The change log records a run's references by NAME; this
-- records the ROW, which is what an agency's Usage view and secrets report
-- read. No value is ever recorded.
--
-- No foreign key to secrets, ssh_credentials, runs or runners: the record
-- outlives each of them (the host-key ledger's rule). owner_agency is the
-- row's owner when it was used (an agency id).
--
-- UNIQUE makes the write idempotent: the injection audit's repair path may
-- write a run's uses a second time.
CREATE TABLE secret_uses (
    id           INTEGER PRIMARY KEY,
    at           TEXT NOT NULL,
    trace_id     TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('secret', 'key')),
    row_id       TEXT NOT NULL,
    owner_agency TEXT NOT NULL,
    via          TEXT NOT NULL CHECK (via IN ('binding', 'become', 'override', 'connect_as', 'host_auth', 'bastion_auth')),
    ref_name     TEXT NOT NULL,
    job_uid      TEXT,
    job_name     TEXT,
    runner_id    TEXT,
    runner_name  TEXT,
    UNIQUE (trace_id, kind, row_id, via)
);
CREATE INDEX secret_uses_row_at ON secret_uses (row_id, at);
CREATE INDEX secret_uses_owner_at ON secret_uses (owner_agency, at);

-- The actor's relation to the changed row's agency, judged when the row is
-- written (SG-11): member, global_admin, outside or system. NULL on rows from
-- before 2.5.1.
ALTER TABLE change_log ADD COLUMN actor_relation TEXT;
