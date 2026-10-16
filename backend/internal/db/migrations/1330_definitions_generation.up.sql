-- 1330_definitions_generation — a counter that every sync of every repository
-- advances, for the scheduler to reload on (2.4.0, GR-29; Phase R3).
--
-- The scheduler holds the timed entries in memory and reloads them when the
-- definitions may have changed. Half of how it knows was the commit of the
-- last sync: one commit, because there was one repository. With a repository
-- per agency there is no one commit. A sync of ANY repository adds one to this
-- counter in its own transaction, and the scheduler reloads when the counter
-- is not the one it loaded at. (The other half, a fingerprint of the tables,
-- notices what the app writes; it is unchanged.)
CREATE TABLE definitions_generation (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    n  INTEGER NOT NULL DEFAULT 0
);
INSERT INTO definitions_generation (id, n) VALUES (1, 0);
