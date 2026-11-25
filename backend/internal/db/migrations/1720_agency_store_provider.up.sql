-- 1720_agency_store_provider (2.5.5 SM-3).
--
-- An agency's connection names its provider (a kind of secret store:
-- 'hashicorp' for HashiCorp Vault and OpenBao, 'aws' for AWS Secrets Manager),
-- holds the provider's credential as ONE sealed settings token (JSON inside,
-- bound to the row: EncryptedSettingsColumns' agencyStore.credential, keyed by
-- agency_id) and the provider's plain settings as JSON (HashiCorp: addr,
-- namespace, caPem; AWS: region, endpoint).
--
-- A migration cannot re-seal, so the rows from before are converted by a boot
-- pass (settings.RunStoreCredentialPass, marker upgrade.2.5.5.storeCredential):
-- it seals role_id_enc and secret_id_enc into credential_enc, copies addr,
-- namespace and ca_pem into settings_json, and empties the old credential
-- columns. addr, namespace and ca_pem keep their values (a HashiCorp
-- connection's address is still what the allowed-hosts check and the health
-- check read); the old columns go in a later release.
ALTER TABLE agency_vaults ADD COLUMN provider TEXT NOT NULL DEFAULT 'hashicorp' CHECK (provider IN ('hashicorp', 'aws'));
ALTER TABLE agency_vaults ADD COLUMN credential_enc TEXT;
ALTER TABLE agency_vaults ADD COLUMN settings_json TEXT NOT NULL DEFAULT '{}';
