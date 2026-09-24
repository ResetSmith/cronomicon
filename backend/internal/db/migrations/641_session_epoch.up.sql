-- SU-5: a bumpable global session epoch. Every OIDC session cookie is stamped with
-- the epoch at login; the epoch is incremented whenever RBAC changes (AD-group
-- mappings / scope grants), so all pre-change sessions fail the epoch check and
-- must re-authenticate — server-side revocation without a per-request DB resolve.
CREATE TABLE auth_session_epoch (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    epoch INTEGER NOT NULL DEFAULT 0
);
INSERT INTO auth_session_epoch (id, epoch) VALUES (1, 0);
