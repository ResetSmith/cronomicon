-- 470 Tags on Env Vars, Secrets, and SSH key credentials (env-vars/ssh-keys tag
-- support). User-authored, SQLite-only tags mirroring scripts.tags (280) and the
-- jobs/schedules/workflows columns (290): operator-managed metadata set via
-- PUT /api/v1/{env-var,env-secret,ssh-credential}-tags/{id} and stored ONLY here.
--
-- Unlike scripts.tags (280) there is NO sync-preservation concern: all three
-- tables are amadeus-owned exceptions to "GitLab is the source of truth"
-- (env_vars/secrets per 004; ssh_credentials per 460), so no Git upsert ever
-- touches them and DEFAULT '[]' simply covers existing rows. For secrets and
-- ssh_credentials the tags are plaintext metadata in their own column — they
-- never enter the AES-256-GCM envelope (ciphertext/nonce/wrapped_dek), so the
-- crypto/reveal/rotate paths are untouched.
ALTER TABLE env_vars ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
ALTER TABLE secrets ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
ALTER TABLE ssh_credentials ADD COLUMN tags TEXT NOT NULL DEFAULT '[]';
