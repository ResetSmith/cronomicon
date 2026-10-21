-- 1620_bound_seals (down). The columns go, and with them the reveal locks and
-- the record of which values are bound. It does NOT unbind anything: a value
-- the server re-sealed bound to its row stays bound, and the code before 2.5.1
-- cannot open it. Going back means restoring a backup taken before the upgrade.
ALTER TABLE secrets DROP COLUMN reveal_locked_at;
ALTER TABLE secrets DROP COLUMN reveal_locked_by;
ALTER TABLE secrets DROP COLUMN reveal_locked;
ALTER TABLE ssh_credentials DROP COLUMN seal_format;
ALTER TABLE secrets DROP COLUMN seal_format;
DELETE FROM settings WHERE key = 'upgrade.2.5.1.bindSeals';
