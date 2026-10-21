-- 1620_bound_seals (2.5.1 SG2.10, SG2.11; questions 134(a) and 134(b)).
--
-- seal_format: how a stored secret's or SSH key's value is sealed. 1 is the
-- form every value had before 2.5.1, bound to nothing; 2 is bound to its row
-- (secrets.RowBinding), so a sealed value copied into another row does not
-- open there. Every existing row starts at 1; the server re-seals each at its
-- first start (secrets.BindSeals) and from then on refuses a format-1 value.
-- A migration cannot do it: re-sealing needs the KEK. After that start, 2.5.0
-- cannot open these values: THIS UPGRADE IS ONE-WAY. Back up first.
ALTER TABLE secrets ADD COLUMN seal_format INTEGER NOT NULL DEFAULT 1;
ALTER TABLE ssh_credentials ADD COLUMN seal_format INTEGER NOT NULL DEFAULT 1;

-- reveal_locked: an agency made the secret unrevealable (SG-19). One way:
-- nothing turns it off. reveal_locked_by/at record who and when.
ALTER TABLE secrets ADD COLUMN reveal_locked INTEGER NOT NULL DEFAULT 0 CHECK (reveal_locked IN (0, 1));
ALTER TABLE secrets ADD COLUMN reveal_locked_by TEXT;
ALTER TABLE secrets ADD COLUMN reveal_locked_at TEXT;
