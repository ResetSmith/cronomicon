-- Reverse 1170 — bring the three columns back with 003's / 110's shapes.
--
-- LOSSY by nature: the declared types dropped by 1170 are gone. Every row
-- comes back with the bash floor in supported_types and a NULL
-- capability_types; capability_json keeps owner / sidecarPath / errors but has
-- no `types` or `origin`, which a rolled-back binary reads as the bash default.
-- A rolled-back binary re-derives git-source scopes in full on its next sync;
-- cronomicon-source scopes keep the bash floor until an operator edits them.
-- Nothing ever acted on these values, so nothing misbehaves in between.
ALTER TABLE scopes RENAME COLUMN git_meta_json TO capability_json;
ALTER TABLE scopes ADD COLUMN capability_types TEXT;
ALTER TABLE scopes ADD COLUMN supported_types  TEXT NOT NULL DEFAULT '["bash"]';
