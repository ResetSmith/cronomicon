-- 350_scope_inventory_raw — raw-for-exec + projection status on scopes.
-- Ansible inventory support, milestone M1 (the ansible-inventory plan §3.1, D3).
--
-- raw_inventory is the byte-exact inventory file. For cronomicon-mode ansible runs
-- the manifest ships it to the runner, which writes it to a temp file and runs
-- `ansible-playbook -i`. It is secret-free by invariant: secret-bearing vars are
-- rejected at INGEST in app code (internal/inventory.ValidateSecrets, Path A/D1),
-- never written here.
--
-- projection_status/projection_json are the ADVISORY parsed view (groups/vars).
-- M1 only persists raw + format; the parser that populates the projection and
-- flips projection_status to 'degraded'/'unavailable' lands in M2.
ALTER TABLE scopes ADD COLUMN raw_inventory TEXT;
ALTER TABLE scopes ADD COLUMN inventory_format TEXT;
ALTER TABLE scopes ADD COLUMN projection_status TEXT NOT NULL DEFAULT 'ok'
    CHECK (projection_status IN ('ok','degraded','unavailable'));
ALTER TABLE scopes ADD COLUMN projection_json TEXT;
