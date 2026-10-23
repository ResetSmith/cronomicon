-- 1620_bound_seals (down). The columns go, and with them the reveal locks and
-- the record of which values are bound. It does NOT unbind anything: a value
-- the server re-sealed bound to its row stays bound, and neither the code
-- before 2.5.1 nor 2.5.1 itself after an up again can open it (the re-added
-- seal_format reads 1, unbound, for every row). Going back means restoring a
-- backup taken before the upgrade; this step exists for the migration tests.
ALTER TABLE secrets DROP COLUMN reveal_locked_at;
ALTER TABLE secrets DROP COLUMN reveal_locked_by;
ALTER TABLE secrets DROP COLUMN reveal_locked;
ALTER TABLE ssh_credentials DROP COLUMN seal_format;
ALTER TABLE secrets DROP COLUMN seal_format;
DELETE FROM settings WHERE key = 'upgrade.2.5.1.bindSeals';
