package settings

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/agencyid"
	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/sshkeys"
)

// vaultHolding is a fake Vault (AppRole login, token lookup, KV v2 read) that
// answers every read with value. tls makes it an https server, as an agency's
// own Vault must be.
func vaultHolding(t *testing.T, value string, tls bool) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role_id"] != "role" || body["secret_id"] != "secret" {
				http.Error(w, `{"errors":["invalid role or secret id"]}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok-" + value, "lease_duration": 3600}})
		case r.Header.Get("X-Vault-Token") != "tok-"+value:
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.Path, "/auth/token/lookup-self"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": value}}})
		}
	})
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

type agencyVaultFix struct {
	ctx                     context.Context
	pool                    *sql.DB
	cfg                     *config.Config
	sec                     *secrets.Service
	own                     *httptest.Server
	finSecret, globalSecret string
}

// newAgencyVaultFix: an installation Vault holding "installation", an
// agency's own Vault holding "agency" on the allowed hosts (not yet
// connected), the agency FIN, and a Vault-backed secret owned by FIN and one
// owned by Global, both naming the same path.
func newAgencyVaultFix(t *testing.T) *agencyVaultFix {
	t.Helper()
	f := &agencyVaultFix{ctx: context.Background(), pool: newTestDB(t)}
	installation := vaultHolding(t, "installation", false)
	f.own = vaultHolding(t, "agency", true)
	kek := make([]byte, 32)
	_, _ = rand.Read(kek)
	f.cfg = &config.Config{
		SecretKEKEnv: base64.StdEncoding.EncodeToString(kek), SecretKEKVersion: 1,
		VaultAddr: installation.URL, VaultRoleID: "role", VaultSecretID: "secret",
		OutboundAllowPrivate: true, OutboundAllowLoopback: true,
	}
	if _, err := f.pool.ExecContext(f.ctx, `INSERT INTO agencies(id, name, created_at) VALUES('ag-fin', 'FIN', 't')`); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(f.own.URL)
	if err := writeVaultAllowedHosts(f.ctx, f.pool, []string{u.Host}, "t", "t"); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.sec = WireVaultClient(f.ctx, f.pool, f.cfg, secrets.New(f.pool, f.cfg, log), log)
	fin, err := f.sec.Create(f.ctx, secrets.CreateInput{Key: "DB", Source: "vault", VaultPath: "secret/data/x#value", OwnerAgency: "ag-fin"}, "t")
	if err != nil {
		t.Fatal(err)
	}
	glob, err := f.sec.Create(f.ctx, secrets.CreateInput{Key: "DB", Source: "vault", VaultPath: "secret/data/x#value", OwnerAgency: agencyid.Global}, "t")
	if err != nil {
		t.Fatal(err)
	}
	f.finSecret, f.globalSecret = fin.ID, glob.ID
	return f
}

func (f *agencyVaultFix) connect(t *testing.T) {
	t.Helper()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.own.Certificate().Raw}))
	rt, err := PrepareAgencyVault(f.ctx, f.pool, f.cfg, "ag-fin", AgencyVaultInput{Addr: f.own.URL, RoleID: "role", SecretID: "secret", CAPEM: &ca})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutAgencyVault(f.ctx, f.pool, f.cfg, "ag-fin", rt, "t"); err != nil {
		t.Fatal(err)
	}
}

func (f *agencyVaultFix) reveal(t *testing.T, id string) (string, error) {
	t.Helper()
	return f.sec.Reveal(f.ctx, id)
}

// SG-21: a row resolves in its OWNER's Vault. Before FIN connects one, FIN's
// secret reads the installation's; after, FIN's reads FIN's and Global's still
// reads the installation's.
func TestSG3_ARowResolvesInItsOwnersVault(t *testing.T) {
	f := newAgencyVaultFix(t)
	if v, err := f.reveal(t, f.finSecret); err != nil || v != "installation" {
		t.Fatalf("FIN's secret before FIN connects a Vault = %q, %v; want the installation's", v, err)
	}
	f.connect(t)
	if v, err := f.reveal(t, f.finSecret); err != nil || v != "agency" {
		t.Fatalf("FIN's secret after = %q, %v; want FIN's own Vault's", v, err)
	}
	if v, err := f.reveal(t, f.globalSecret); err != nil || v != "installation" {
		t.Fatalf("Global's secret after = %q, %v; want the installation's", v, err)
	}
}

// SG-21 for SSH keys: FIN's Vault-backed key is read in FIN's Vault, Global's
// in the installation's.
func TestSG3_AKeyResolvesInItsOwnersVault(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	for _, k := range []struct{ id, owner, want string }{{"k-fin", "ag-fin", "agency"}, {"k-global", agencyid.Global, "installation"}} {
		if _, err := f.pool.ExecContext(f.ctx, `INSERT INTO ssh_credentials(id, label, source, vault_ref, owner_agency, created_by, created_at, last_modified_by, last_modified_at)
			VALUES(?, ?, 'vault', 'secret/data/k#value', ?, 't', 't', 't', 't')`, k.id, k.id, k.owner); err != nil {
			t.Fatal(err)
		}
		got, found, err := sshkeys.ResolveMaterialByID(f.ctx, f.pool, f.cfg, f.sec.Vault(), k.id)
		if err != nil || !found || got != k.want {
			t.Errorf("%s = %q, %v, %v; want %q", k.id, got, found, err, k.want)
		}
	}
}

// SG-21: an agency whose own connection cannot be read gets an error, never
// the installation's Vault.
func TestSG3_AnUnreadableConnectionNeverFallsBack(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	if _, err := f.pool.ExecContext(f.ctx, `UPDATE agency_vaults SET role_id_enc = 'not-a-token' WHERE agency_id = 'ag-fin'`); err != nil {
		t.Fatal(err)
	}
	v, err := f.reveal(t, f.finSecret)
	if err == nil || v == "installation" {
		t.Fatalf("FIN's secret with an unreadable connection = %q, %v; want an error and no installation value", v, err)
	}
	if !errors.Is(err, secrets.ErrVaultUnavailable) {
		t.Errorf("err = %v, want ErrVaultUnavailable", err)
	}
}

// SG-22: the allowed hosts are asked on save and every time a client is
// built, so a host taken off the list stops being reached (and is not
// replaced by the installation's Vault).
func TestSG3_AllowedHostsOnSaveAndOnUse(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	if err := writeVaultAllowedHosts(f.ctx, f.pool, []string{"vault.example.com"}, "t", "t"); err != nil {
		t.Fatal(err)
	}
	if v, err := f.reveal(t, f.finSecret); err == nil || v != "" {
		t.Fatalf("FIN's secret with its host off the list = %q, %v; want an error", v, err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.own.Certificate().Raw}))
	_, err := PrepareAgencyVault(f.ctx, f.pool, f.cfg, "ag-fin", AgencyVaultInput{Addr: f.own.URL, RoleID: "role", SecretID: "secret", CAPEM: &ca})
	if !errors.Is(err, ErrVaultURLNotAllowed) {
		t.Fatalf("save with the host off the list: %v, want ErrVaultURLNotAllowed", err)
	}
	if err := writeVaultAllowedHosts(f.ctx, f.pool, nil, "t", "t"); err != nil {
		t.Fatal(err)
	}
	if err := CheckVaultURL(f.ctx, f.pool, "https://vault.example.com"); !errors.Is(err, ErrVaultURLNotAllowed) {
		t.Errorf("an empty list allowed a host: %v", err)
	}
	if err := CheckVaultURL(f.ctx, f.pool, "http://vault.example.com"); !errors.Is(err, ErrVaultURLNotAllowed) {
		t.Errorf("http was accepted: %v", err)
	}
	if _, err := PrepareAgencyVault(f.ctx, f.pool, f.cfg, agencyid.Global, AgencyVaultInput{Addr: f.own.URL}); !errors.Is(err, ErrGlobalUsesInstallationVault) {
		t.Errorf("Global was given an own Vault: %v", err)
	}
}

// SG-27: disconnecting is refused while the agency owns Vault-backed rows.
func TestSG3_DisconnectRefusedWhileInUse(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	if _, err := DeleteAgencyVault(f.ctx, f.pool, "ag-fin", "t"); !errors.Is(err, ErrAgencyVaultInUse) {
		t.Fatalf("disconnect with a Vault-backed secret: %v, want ErrAgencyVaultInUse", err)
	}
	if _, err := f.pool.ExecContext(f.ctx, `DELETE FROM secrets WHERE id = ?`, f.finSecret); err != nil {
		t.Fatal(err)
	}
	found, err := DeleteAgencyVault(f.ctx, f.pool, "ag-fin", "t")
	if err != nil || !found {
		t.Fatalf("disconnect with none left = %v, %v", found, err)
	}
	if v, err := f.reveal(t, f.globalSecret); err != nil || v != "installation" {
		t.Errorf("Global's secret = %q, %v", v, err)
	}
}

// SG3.2: the client cache keeps one client per connection: a changed secret
// id drops the client built for the old one, and two agencies do not share
// or evict each other's.
func TestSG3_TheCacheKeepsOneClientPerConnection(t *testing.T) {
	f := newAgencyVaultFix(t)
	clients := func(agency string) int {
		vaultClientMu.Lock()
		defer vaultClientMu.Unlock()
		n := 0
		for k := range vaultClientCache {
			if strings.HasPrefix(k, "agency\x00"+agency+"\x00"+f.own.URL) {
				n++
			}
		}
		return n
	}
	build := func(agency, secret string) {
		t.Helper()
		if _, err := AgencyVaultClient(f.ctx, f.pool, f.cfg, AgencyVaultRuntime{Agency: agency, Addr: f.own.URL, RoleID: "role", SecretID: secret}); err != nil {
			t.Fatal(err)
		}
	}
	build("ag-fin", "s1")
	build("ag-tax", "s1")
	build("ag-fin", "s2")
	if n := clients("ag-fin"); n != 1 {
		t.Errorf("FIN's clients after a changed secret id = %d, want 1", n)
	}
	if n := clients("ag-tax"); n != 1 {
		t.Errorf("TAX's client = %d, want 1 (untouched by FIN's change)", n)
	}
}

// SG-28: agency_vault_unreachable is raised under the agency when its Vault
// does not answer, and resolved when it does.
func TestSG3_UnreachableNotice(t *testing.T) {
	f := newAgencyVaultFix(t)
	f.connect(t)
	open := func() int {
		var n int
		if err := f.pool.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM notices WHERE kind = ? AND agency_id = 'ag-fin' AND resolved_at IS NULL`,
			KindAgencyVaultUnreachable).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := CheckAgencyVaults(f.ctx, f.pool, f.cfg); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 0 {
		t.Fatalf("open notices with the Vault answering = %d", n)
	}
	// A wrong secret id: the login is refused.
	if _, err := f.pool.ExecContext(f.ctx, `DELETE FROM agency_vaults`); err != nil {
		t.Fatal(err)
	}
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.own.Certificate().Raw}))
	rt, err := PrepareAgencyVault(f.ctx, f.pool, f.cfg, "ag-fin", AgencyVaultInput{Addr: f.own.URL, RoleID: "role", SecretID: "wrong", CAPEM: &ca})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutAgencyVault(f.ctx, f.pool, f.cfg, "ag-fin", rt, "t"); err != nil {
		t.Fatal(err)
	}
	if err := CheckAgencyVaults(f.ctx, f.pool, f.cfg); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 1 {
		t.Fatalf("open notices with the login refused = %d, want 1", n)
	}
	f.connect(t)
	if err := CheckAgencyVaults(f.ctx, f.pool, f.cfg); err != nil {
		t.Fatal(err)
	}
	if n := open(); n != 0 {
		t.Fatalf("open notices once it answers again = %d", n)
	}
}
