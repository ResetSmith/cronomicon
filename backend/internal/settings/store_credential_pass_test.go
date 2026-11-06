package settings

import (
	"encoding/pem"
	"errors"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/secrets"
)

// connectAsBefore writes FIN's connection as 2.5.4 did: the role id and the
// secret id sealed into their own columns, no credential_enc.
func (f *agencyVaultFix) connectAsBefore(t *testing.T) {
	t.Helper()
	role, err := secrets.EncryptSetting(f.cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultRoleID, "ag-fin"), "role")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secrets.EncryptSetting(f.cfg, secrets.SettingsBinding(secrets.LabelAgencyVaultSecretID, "ag-fin"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.own.Certificate().Raw}))
	if _, err := f.pool.ExecContext(f.ctx, `
		INSERT INTO agency_vaults(agency_id, addr, namespace, role_id_enc, secret_id_enc, ca_pem, created_by, created_at, last_modified_by, last_modified_at)
		VALUES('ag-fin', ?, 'ns', ?, ?, ?, 't', 't', 't', 't')`, f.own.URL, role, sec, ca); err != nil {
		t.Fatal(err)
	}
}

// SM-3: a 2.5.4 connection is read as before until the pass runs, converted
// by the pass (one sealed credential, the settings copied, the old tokens
// emptied), read the same after, and the pass is done once.
func TestSM3_TheBootPassConvertsAConnectionFromBefore(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connectAsBefore(t)
	if v, err := f.reveal(t, f.finSecret); err != nil || v != "agency" {
		t.Fatalf("before the pass: FIN's secret = %q, %v; want its own Vault's", v, err)
	}

	rep, err := RunStoreCredentialPass(f.ctx, f.pool, f.cfg)
	if err != nil || rep.Converted != 1 || rep.Failed != 0 || rep.Done {
		t.Fatalf("pass = %+v, %v; want one converted", rep, err)
	}
	var provider, cred, settingsJSON, role, sec string
	if err := f.pool.QueryRowContext(f.ctx, `SELECT provider, COALESCE(credential_enc, ''), settings_json, role_id_enc, secret_id_enc FROM agency_vaults WHERE agency_id = 'ag-fin'`).
		Scan(&provider, &cred, &settingsJSON, &role, &sec); err != nil {
		t.Fatal(err)
	}
	if provider != "hashicorp" || cred == "" || role != "" || sec != "" {
		t.Fatalf("after the pass: provider %q, credential set %v, old tokens %q %q; want hashicorp, set, both empty", provider, cred != "", role, sec)
	}
	open, err := openStoreCredential(f.cfg, "ag-fin", cred)
	if err != nil || open["roleId"] != "role" || open["secretId"] != "secret" {
		t.Fatalf("credential = %v, %v; want the two AppRole values", open, err)
	}
	if _, err := secrets.DecryptSetting(f.cfg, secrets.SettingsBinding(secrets.LabelAgencyStoreCredential, "ag-other"), cred); err == nil {
		t.Error("the credential opened under another agency's binding: it must be bound to its row")
	}
	rt, found, err := ReadAgencyVaultRuntime(f.ctx, f.pool, f.cfg, "ag-fin")
	if err != nil || !found || rt.Addr != f.own.URL || rt.Namespace != "ns" || len(rt.CAPEM) == 0 {
		t.Fatalf("runtime after = %+v, %v, %v; want the address, namespace and CA carried", rt, found, err)
	}

	if v, err := f.reveal(t, f.finSecret); err != nil || v != "agency" {
		t.Fatalf("after the pass: FIN's secret = %q, %v; want its own Vault's", v, err)
	}
	if rep, err := RunStoreCredentialPass(f.ctx, f.pool, f.cfg); err != nil || !rep.Done || rep.Converted != 0 {
		t.Fatalf("second pass = %+v, %v; want done, nothing read", rep, err)
	}
}

// SM-3: a row the pass cannot open is counted, left as it was, and no
// marker is written, so the next start tries again.
func TestSM3_AFailedRowLeavesNoMarker(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connectAsBefore(t)
	if _, err := f.pool.ExecContext(f.ctx, `UPDATE agency_vaults SET secret_id_enc = 'not-a-token' WHERE agency_id = 'ag-fin'`); err != nil {
		t.Fatal(err)
	}
	rep, err := RunStoreCredentialPass(f.ctx, f.pool, f.cfg)
	if err != nil || rep.Failed != 1 || rep.Converted != 0 {
		t.Fatalf("pass = %+v, %v; want one failed", rep, err)
	}
	var n int
	_ = f.pool.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM settings WHERE key = ?`, storeCredentialMarker).Scan(&n)
	if n != 0 {
		t.Fatal("the marker was written after a failure")
	}
	var role string
	_ = f.pool.QueryRowContext(f.ctx, `SELECT role_id_enc FROM agency_vaults WHERE agency_id = 'ag-fin'`).Scan(&role)
	if role == "" {
		t.Fatal("a row that failed was changed")
	}
}

// SM-1, SG-21: a connection naming a provider this build cannot build is an
// error for its rows, never the installation's Vault.
func TestSM3_AnUnknownProviderNeverFallsBack(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	if _, err := f.pool.ExecContext(f.ctx, `UPDATE agency_vaults SET provider = 'aws' WHERE agency_id = 'ag-fin'`); err != nil {
		t.Fatal(err)
	}
	v, err := f.reveal(t, f.finSecret)
	if err == nil || v == "installation" {
		t.Fatalf("FIN's secret = %q, %v; want an error and no installation value", v, err)
	}
	if !errors.Is(err, secrets.ErrVaultUnavailable) {
		t.Errorf("err = %v, want ErrVaultUnavailable", err)
	}
}
