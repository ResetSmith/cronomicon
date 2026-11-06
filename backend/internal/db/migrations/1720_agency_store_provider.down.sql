-- 1720 down (2.5.5 SM-3, question 222). Lossy by design: a connection the boot
-- pass converted keeps empty role_id_enc / secret_id_enc, so 2.5.4 fails to
-- log in to that agency's Vault and its Vault-sourced rows do not resolve until
-- an administrator enters the credential again. Nothing resolves in the
-- installation's Vault instead (SG-21). An AWS connection has no 2.5.4 meaning
-- and is removed.
DELETE FROM agency_vaults WHERE provider <> 'hashicorp';
ALTER TABLE agency_vaults DROP COLUMN settings_json;
ALTER TABLE agency_vaults DROP COLUMN credential_enc;
ALTER TABLE agency_vaults DROP COLUMN provider;
