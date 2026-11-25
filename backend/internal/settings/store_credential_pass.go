package settings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/secretstore/hashicorp"
)

// storeCredentialMarker records that every agency connection from before
// 2.5.5 has its credential in credential_enc (SM-3).
const storeCredentialMarker = "upgrade.2.5.5.storeCredential"

// StoreCredentialReport is what RunStoreCredentialPass did.
type StoreCredentialReport struct {
	Converted int
	Failed    int
	// Done is true when the marker was already written: nothing was read.
	Done bool
}

// RunStoreCredentialPass converts each agency connection from before 2.5.5
// (migration 1720): its AppRole role id and secret id, two sealed settings
// tokens, are opened and sealed again as one JSON credential bound to the row
// (agencyStore.credential), its address, namespace and CA are copied into
// settings_json, and the two old tokens are emptied. A migration cannot do
// this: it has no KEK.
//
// Each row is its own UPDATE, guarded on credential_enc still being empty,
// so a pass that stops part-way is finished by the next start. The marker is
// written only when no row failed. It runs at start, after secrets.BindSeals
// (the old tokens are bound by then) and before the server takes requests.
func RunStoreCredentialPass(ctx context.Context, database *sql.DB, cfg *config.Config) (StoreCredentialReport, error) {
	var rep StoreCredentialReport
	var done string
	switch err := database.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, storeCredentialMarker).Scan(&done); {
	case err == nil:
		rep.Done = true
		return rep, nil
	case !errors.Is(err, sql.ErrNoRows):
		return rep, fmt.Errorf("read %s: %w", storeCredentialMarker, err)
	}

	type legacy struct{ agency, addr, namespace, ca, roleEnc, secretEnc string }
	rows, err := database.QueryContext(ctx, `
		SELECT agency_id, addr, namespace, ca_pem, role_id_enc, secret_id_enc
		  FROM agency_vaults WHERE COALESCE(credential_enc, '') = '' ORDER BY agency_id`)
	if err != nil {
		return rep, fmt.Errorf("read agency connections: %w", err)
	}
	var todo []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.agency, &l.addr, &l.namespace, &l.ca, &l.roleEnc, &l.secretEnc); err != nil {
			rows.Close()
			return rep, err
		}
		todo = append(todo, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, l := range todo {
		if err := convertStoreCredential(ctx, database, cfg, l.agency, l.addr, l.namespace, l.ca, l.roleEnc, l.secretEnc, now); err != nil {
			rep.Failed++
			continue
		}
		rep.Converted++
	}
	if rep.Converted > 0 {
		secrets.RedactionSourceChanged()
	}
	if rep.Failed > 0 {
		return rep, nil
	}
	if _, err := database.ExecContext(ctx, `
		INSERT INTO settings (key, value, last_modified_by, last_modified_at) VALUES (?, ?, 'system (2.5.5 upgrade)', ?)
		ON CONFLICT(key) DO NOTHING`, storeCredentialMarker, "converted", now); err != nil {
		return rep, fmt.Errorf("write %s: %w", storeCredentialMarker, err)
	}
	return rep, nil
}

func convertStoreCredential(ctx context.Context, database *sql.DB, cfg *config.Config, agency, addr, namespace, ca, roleEnc, secretEnc, now string) error {
	roleID, err := secrets.DecryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultRoleID, agency), roleEnc)
	if err != nil {
		return fmt.Errorf("open the role id: %w", err)
	}
	secretID, err := secrets.DecryptSetting(cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultSecretID, agency), secretEnc)
	if err != nil {
		return fmt.Errorf("open the secret id: %w", err)
	}
	if roleID == "" || secretID == "" {
		return fmt.Errorf("the connection of %s has no AppRole credential", agency)
	}
	tok, err := sealStoreCredential(cfg, agency, map[string]string{hashicorp.CredRoleID: roleID, hashicorp.CredSecretID: secretID})
	if err != nil {
		return err
	}
	set, err := json.Marshal(map[string]string{hashicorp.SettingAddr: addr, hashicorp.SettingNamespace: namespace, hashicorp.SettingCAPEM: ca})
	if err != nil {
		return err
	}
	_, err = database.ExecContext(ctx, `
		UPDATE agency_vaults SET provider = 'hashicorp', credential_enc = ?, settings_json = ?, role_id_enc = '', secret_id_enc = ''
		 WHERE agency_id = ? AND COALESCE(credential_enc, '') = ''`, tok, string(set), agency)
	return err
}
