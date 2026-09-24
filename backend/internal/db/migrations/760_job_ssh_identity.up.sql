-- CA-7 (the ssh-user plan, Phase B) — declarative per-job SSH identity.
--
-- ssh_user / ssh_credential are the job-spec "connect as" defaults: remote
-- username and ssh_credentials LABEL (CA-Q2, names only). Written by BOTH job
-- sources — git sync (spec.ssh_user / spec.ssh_credential) and the Composer —
-- like target_host, so no SQLite-only preservation is needed. Folded onto
-- runs.ssh_user/ssh_credential (migration 750) at enqueue on every producer;
-- a per-run override wins per field. NULL preserves the per-host resolution.
ALTER TABLE jobs ADD COLUMN ssh_user TEXT;
ALTER TABLE jobs ADD COLUMN ssh_credential TEXT;
