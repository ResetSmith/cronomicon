-- 410_ssh_hosts_cronomicon_inv — enforce the (scope_id, hostname) key for cronomicon
-- inventory-imported hosts at the DB level (M5 / import-hosts). The M4 partial
-- index ux_ssh_hosts_inv only covers source='git'; cronomicon import rows had no
-- uniqueness, so two concurrent imports could both insert a duplicate. A partial
-- index scoped to source='cronomicon' AND scope_id IS NOT NULL enforces one imported
-- row per (scope, host) while leaving manual operator overlays (scope_id NULL) —
-- which are intentionally non-unique by hostname — unconstrained.
CREATE UNIQUE INDEX ux_ssh_hosts_cronomicon_inv
    ON ssh_hosts(scope_id, hostname)
    WHERE source='cronomicon' AND scope_id IS NOT NULL;
