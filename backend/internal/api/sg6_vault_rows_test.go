package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ResetSmith/cronomicon/internal/config"
	"github.com/ResetSmith/cronomicon/internal/secrets"
	"github.com/ResetSmith/cronomicon/internal/settings"
)

// 2.5.1 SG6.2 — what the pre-release review found about Vault rows (the
// review's A4, A5, A6): a row stays in the Vault it names a path in.

// sg6Vault is a fake agency Vault that counts its AppRole logins and its
// sys/health reads, and answers 503 (sealed) to sys/health while sealed is set.
type sg6Vault struct {
	*httptest.Server
	logins, health atomic.Int32
	sealed         atomic.Bool
}

func newSG6Vault(t *testing.T, value string) *sg6Vault {
	t.Helper()
	v := &sg6Vault{}
	v.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/auth/approle/login"):
			v.logins.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["role_id"] != sg3RoleID || body["secret_id"] != sg3SecretID {
				http.Error(w, `{"errors":["invalid role or secret id"]}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "tok", "lease_duration": 3600}})
		case r.URL.Path == "/v1/sys/health":
			v.health.Add(1)
			if r.Header.Get("X-Vault-Token") != "" {
				http.Error(w, `{"errors":["sys/health is asked without a token"]}`, http.StatusBadRequest)
				return
			}
			if v.sealed.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"sealed":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"value": value}}})
		}
	}))
	t.Cleanup(v.Close)
	return v
}

// sg6Env is the two-agency cast with agency Vaults allowed on the fakes' host.
type sg6Env struct {
	h    http.Handler
	cfg  *config.Config
	pool *sql.DB
	sec  *secrets.Service
}

func newSG6Env(t *testing.T, vaults ...*sg6Vault) *sg6Env {
	t.Helper()
	e := &sg6Env{}
	e.h, e.pool = gateServerWith(t, func(c *config.Config) {
		c.OutboundAllowPrivate, c.OutboundAllowLoopback = true, true
		e.cfg = c
	})
	var hosts []string
	for _, v := range vaults {
		u, _ := url.Parse(v.URL)
		hosts = append(hosts, `"`+u.Host+`"`)
	}
	mustExec(t, e.pool)(`INSERT INTO settings(key, value, last_modified_by, last_modified_at) VALUES('vaultAllowedHosts', ?, 't', 't')`,
		"["+strings.Join(hosts, ",")+"]")
	e.sec = secrets.New(e.pool, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return e
}

// connect gives agency its own Vault at v, as who; secretID "" is the right one.
func (e *sg6Env) connectBody(v *sg6Vault, secretID string) string {
	if secretID == "" {
		secretID = sg3SecretID
	}
	ca, _ := json.Marshal(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: v.Certificate().Raw})))
	return `{"addr":"` + v.URL + `","roleId":"` + sg3RoleID + `","secretId":"` + secretID + `","caPem":` + string(ca) + `}`
}

func (e *sg6Env) connect(t *testing.T, agency, who string, v *sg6Vault) {
	t.Helper()
	if rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/"+agency+"/vault", who, e.connectBody(v, "")); rec.Code != http.StatusOK {
		t.Fatalf("connecting %s's Vault = %d %s", agency, rec.Code, rec.Body)
	}
}

func (e *sg6Env) wired() *secrets.Service {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return settings.WireVaultClient(context.Background(), e.pool, e.cfg, secrets.New(e.pool, e.cfg, log), log)
}

// vaultSecret creates a Vault-sourced secret owned by owner, in owner alone
// (or in members, a row held for several), and answers its id.
func (e *sg6Env) vaultSecret(t *testing.T, key, path, owner string, members ...string) string {
	t.Helper()
	s, err := e.sec.Create(context.Background(), secrets.CreateInput{Key: key, Source: "vault", VaultPath: path, OwnerAgency: owner}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) == 0 {
		members = []string{owner}
	}
	exec := mustExec(t, e.pool)
	exec(`DELETE FROM secret_agencies WHERE secret_id = ?`, s.ID)
	for _, m := range members {
		exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, ?)`, s.ID, m)
	}
	return s.ID
}

func (e *sg6Env) owner(t *testing.T, table, id string) string {
	t.Helper()
	var o string
	if err := e.pool.QueryRow(`SELECT owner_agency FROM `+table+` WHERE id = ?`, id).Scan(&o); err != nil {
		t.Fatal(err)
	}
	return o
}

// A4: an agency's prefixes are the paths it may name in the installation's
// Vault, or, with a Vault of its own, a limit it set itself. Disconnecting its
// Vault must not leave its own limit standing as an assignment in the
// installation's Vault: the review's FIN set "secret/data" in its own Vault,
// disconnected, and then named TAX's path in the installation's.
func TestSG6_ADisconnectTakesThePrefixesWithIt(t *testing.T) {
	fin := newSG6Vault(t, sg3Value)
	e := newSG6Env(t, fin)
	e.connect(t, "ag:FIN", gFinAdmin, fin)
	if rec := gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault-prefixes", gFinAdmin, `{"prefixes":["secret/data"]}`); rec.Code != http.StatusOK {
		t.Fatalf("FIN's own prefixes = %d %s", rec.Code, rec.Body)
	}
	if rec := gateReq(t, e.h, http.MethodDelete, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("disconnect = %d %s", rec.Code, rec.Body)
	}
	rec := gateReq(t, e.h, http.MethodGet, "/api/v1/agencies/ag:FIN/vault-prefixes", gFinAdmin, "")
	var got struct {
		Prefixes []string `json:"prefixes"`
		OwnVault bool     `json:"ownVault"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Prefixes) != 0 || got.OwnVault {
		t.Errorf("after the disconnect FIN's prefixes = %s, want none and no own Vault", rec.Body)
	}
	rec = gateReq(t, e.h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"TAX_DB_STOLEN","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`)
	if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "vault_path_not_allowed" {
		t.Errorf("FIN names TAX's path in the installation's Vault after its disconnect = %d %s, want 422 vault_path_not_allowed", rec.Code, rec.Body)
	}
	var detail string
	_ = e.pool.QueryRow(`SELECT details FROM change_log WHERE action = 'vault-disconnected' AND agency_id = 'ag:FIN'`).Scan(&detail)
	if !strings.Contains(detail, "secret/data") {
		t.Errorf("the disconnect's change-log row = %q, want it to name the prefixes it cleared", detail)
	}

	// An agency on the installation's Vault has no connection to disconnect,
	// and the prefixes a global administrator gave it stay.
	setPrefixes(t, e.h, gRoot, "ag:TAX", "secret/data/tax")
	if rec := gateReq(t, e.h, http.MethodDelete, "/api/v1/agencies/ag:TAX/vault", gTaxAdmin, ""); rec.Code != http.StatusNotFound {
		t.Errorf("TAX disconnecting a Vault it does not have = %d, want 404", rec.Code)
	}
	if p, _ := settings.ListVaultPrefixes(context.Background(), e.pool, "ag:TAX"); len(p) != 1 || p[0] != "secret/data/tax" {
		t.Errorf("TAX's assigned prefixes after a refused disconnect = %v, want [secret/data/tax]", p)
	}

	// Refused while the agency owns Vault rows: nothing is cleared.
	e.connect(t, "ag:FIN", gFinAdmin, fin)
	setPrefixes(t, e.h, gFinAdmin, "ag:FIN", "secret/data/fin")
	e.vaultSecret(t, "FIN_DB", "secret/data/fin/db#value", "ag:FIN")
	if rec := gateReq(t, e.h, http.MethodDelete, "/api/v1/agencies/ag:FIN/vault", gFinAdmin, ""); rec.Code != http.StatusConflict {
		t.Fatalf("disconnect while in use = %d, want 409", rec.Code)
	}
	if p, _ := settings.ListVaultPrefixes(context.Background(), e.pool, "ag:FIN"); len(p) != 1 {
		t.Errorf("FIN's prefixes after a refused disconnect = %v, want them kept", p)
	}
}

// A5: a Vault-backed row names a path in its owner's Vault. A move to an owner
// on another Vault reads the same path somewhere else: refused with the rows
// listed, unless the request confirms it; a confirmed move says so in the
// change log. Both routes, both directions (own → installation is refused
// outright since 2.5.3, CA-4), own to own, keys too, and the removal that
// settles a held row on its last member.
func TestSG6_AMoveAcrossVaultsIsConfirmed(t *testing.T) {
	fin, tax := newSG6Vault(t, "FIN-VALUE"), newSG6Vault(t, "TAX-VALUE")
	e := newSG6Env(t, fin, tax)
	e.connect(t, "ag:FIN", gFinAdmin, fin)
	exec := mustExec(t, e.pool)
	repair := func(route, id, agency string, confirm bool) *httptest.ResponseRecorder {
		b := `{"repair":true,"reason":"sg6.2","assignments":[{"id":"` + id + `","agencyIds":["` + agency + `"]}]`
		if confirm {
			b += `,"confirmVaultChange":true`
		}
		return gateReq(t, e.h, http.MethodPut, "/api/v1/"+route, gRoot, b+`}`)
	}
	refused := func(what string, rec *httptest.ResponseRecorder, id string) {
		t.Helper()
		if rec.Code != http.StatusConflict || errCode(rec.Body.Bytes()) != "vault_changes" || !strings.Contains(rec.Body.String(), id) {
			t.Errorf("%s unconfirmed = %d %s, want 409 vault_changes naming %s", what, rec.Code, rec.Body, id)
		}
	}
	confirmedInLog := func(what, id string) {
		t.Helper()
		var n int
		_ = e.pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'moved' AND details LIKE ? AND details LIKE '%vault-change=confirmed%'`,
			"%id="+id+"%").Scan(&n)
		if n != 2 {
			t.Errorf("%s: %d change-log rows mark the confirmed Vault change, want 2 (one each side)", what, n)
		}
	}

	// Own → installation, through the per-kind route: until 2.5.3 refused
	// unconfirmed (409 vault_changes) and moved when confirmed. Since CA-4
	// nothing moves into Central, confirmed or not, and nothing is logged.
	centralRefused := func(what string, rec *httptest.ResponseRecorder) {
		t.Helper()
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "central_runs_nothing" {
			t.Errorf("%s = %d %s, want 422 central_runs_nothing", what, rec.Code, rec.Body)
		}
	}
	s1 := e.vaultSecret(t, "FIN_DB", "secret/data/fin/db#value", "ag:FIN")
	centralRefused("FIN's own-Vault secret to Global, unconfirmed", repair("secret-agencies", s1, "global", false))
	centralRefused("FIN's own-Vault secret to Global, confirmed", repair("secret-agencies", s1, "global", true))
	if o := e.owner(t, "secrets", s1); o != "ag:FIN" {
		t.Fatalf("a refused move changed the owner to %s", o)
	}
	var logged int
	_ = e.pool.QueryRow(`SELECT COUNT(*) FROM change_log WHERE action = 'moved' AND details LIKE ?`, "%id="+s1+"%").Scan(&logged)
	if logged != 0 {
		t.Errorf("the refused move into Central left %d change-log rows", logged)
	}

	// Installation → own, through both routes.
	s2 := e.vaultSecret(t, "GLOBAL_DB", "secret/data/global/db#value", "global")
	refused("Global's secret to FIN (per kind)", repair("secret-agencies", s2, "ag:FIN", false), s2)
	members := func(confirm bool, list ...string) *httptest.ResponseRecorder {
		b := `{"repair":true,"reason":"sg6.2","members":[` + strings.Join(list, ",") + `]`
		if confirm {
			b += `,"confirmVaultChange":true`
		}
		return gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gRoot, b+`}`)
	}
	finMembers := func(extra ...string) []string {
		rows, err := e.pool.Query(`
			SELECT 'secret', secret_id FROM secret_agencies WHERE agency_id = 'ag:FIN'
			UNION ALL SELECT 'env-var', env_var_id FROM env_var_agencies WHERE agency_id = 'ag:FIN'
			UNION ALL SELECT 'ssh-credential', credential_id FROM ssh_credential_agencies WHERE agency_id = 'ag:FIN'
			UNION ALL SELECT 'scope', scope_id FROM scope_agencies WHERE agency_id = 'ag:FIN'
			UNION ALL SELECT 'runner', runner_id FROM runner_agencies WHERE agency_id = 'ag:FIN'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var k, id string
			_ = rows.Scan(&k, &id)
			out = append(out, `{"kind":"`+k+`","id":"`+id+`"}`)
		}
		return append(out, extra...)
	}
	add := `{"kind":"secret","id":"` + s2 + `"}`
	refused("Global's secret to FIN (agency members)", members(false, finMembers(add)...), s2)
	if o := e.owner(t, "secrets", s2); o != "global" {
		t.Fatalf("a refused members write changed the owner to %s", o)
	}
	if rec := members(true, finMembers(add)...); rec.Code != http.StatusOK {
		t.Fatalf("confirmed members write = %d %s", rec.Code, rec.Body)
	}
	if o := e.owner(t, "secrets", s2); o != "ag:FIN" {
		t.Errorf("after the confirmed members write the owner is %s", o)
	}
	confirmedInLog("installation → own (members)", s2)
	if v, err := e.wired().Reveal(context.Background(), s2); err != nil || v != "FIN-VALUE" {
		t.Errorf("the moved secret reads %q %v from FIN's Vault", v, err)
	}

	// Own → own.
	e.connect(t, "ag:TAX", gTaxAdmin, tax)
	s3 := e.vaultSecret(t, "FIN_API", "secret/data/fin/api#value", "ag:FIN")
	refused("FIN's own-Vault secret to TAX's own Vault", repair("secret-agencies", s3, "ag:TAX", false), s3)

	// A key, own → installation.
	exec(`INSERT INTO ssh_credentials (id,label,source,vault_ref,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
	      VALUES ('k-fin-vault','fin_vault_key','vault','secret/data/fin/key#private_key','ag:FIN','t','2026-01-01T00:00:00Z','t','2026-01-01T00:00:00Z')`)
	exec(`DELETE FROM ssh_credential_agencies WHERE credential_id = 'k-fin-vault'`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-fin-vault','ag:FIN')`)
	// (409 vault_changes until 2.5.3; since CA-4 the move into Central is refused.)
	centralRefused("FIN's own-Vault key to Global", repair("ssh-credential-agencies", "k-fin-vault", "global", false))

	// Same Vault on both sides: no confirmation. Global → OPS, both on the
	// installation's Vault, inside OPS's prefixes.
	exec(`INSERT INTO agencies (id, name, created_at) VALUES ('ag:OPS', 'OPS', '2026-01-01T00:00:00Z')`)
	setPrefixes(t, e.h, gRoot, "ag:OPS", "secret/data/ops")
	s4 := e.vaultSecret(t, "OPS_DB", "secret/data/ops/db#value", "global")
	if rec := repair("secret-agencies", s4, "ag:OPS", false); rec.Code != http.StatusOK {
		t.Errorf("a move within the installation's Vault = %d %s, want 200 with no confirmation", rec.Code, rec.Body)
	}

	// A held row (Global's, listing FIN and TAX) settles on FIN when TAX takes
	// it off its list: that moves it from the installation's Vault into FIN's.
	setPrefixes(t, e.h, gFinAdmin, "ag:FIN", "secret/data/held")
	s5 := e.vaultSecret(t, "HELD_DB", "secret/data/held/db#value", "global", "ag:FIN", "ag:TAX")
	taxMembers := func(confirm bool) *httptest.ResponseRecorder {
		rows, err := e.pool.Query(`
			SELECT 'secret', secret_id FROM secret_agencies WHERE agency_id = 'ag:TAX' AND secret_id <> ?
			UNION ALL SELECT 'env-var', env_var_id FROM env_var_agencies WHERE agency_id = 'ag:TAX'
			UNION ALL SELECT 'ssh-credential', credential_id FROM ssh_credential_agencies WHERE agency_id = 'ag:TAX'
			UNION ALL SELECT 'scope', scope_id FROM scope_agencies WHERE agency_id = 'ag:TAX'
			UNION ALL SELECT 'runner', runner_id FROM runner_agencies WHERE agency_id = 'ag:TAX'`, s5)
		if err != nil {
			t.Fatal(err)
		}
		var list []string
		for rows.Next() {
			var k, id string
			_ = rows.Scan(&k, &id)
			list = append(list, `{"kind":"`+k+`","id":"`+id+`"}`)
		}
		rows.Close()
		b := `{"repair":true,"reason":"sg6.2","members":[` + strings.Join(list, ",") + `]`
		if confirm {
			b += `,"confirmVaultChange":true`
		}
		return gateReq(t, e.h, http.MethodPut, "/api/v1/agencies/ag:TAX/members", gRoot, b+`}`)
	}
	refused("a held row settling on FIN", taxMembers(false), s5)
	if o := e.owner(t, "secrets", s5); o != "global" {
		t.Fatalf("a refused removal settled the held row on %s", o)
	}
	if rec := taxMembers(true); rec.Code != http.StatusOK {
		t.Fatalf("confirmed removal = %d %s", rec.Code, rec.Body)
	}
	if o := e.owner(t, "secrets", s5); o != "ag:FIN" {
		t.Errorf("after the confirmed removal the held row's owner is %s, want ag:FIN", o)
	}
	confirmedInLog("held row settling", s5)
}

// A6: the health check of an agency's own Vault asks sys/health and never
// logs in; the review counted a fresh AppRole login (a token, a secret-id use)
// per check, up to twice a minute.
func TestSG6_TheHealthCheckDoesNotLogIn(t *testing.T) {
	fin := newSG6Vault(t, sg3Value)
	e := newSG6Env(t, fin)
	e.connect(t, "ag:FIN", gFinAdmin, fin)
	ctx := context.Background()
	before := fin.logins.Load()
	for range 5 {
		if err := settings.CheckAgencyVaults(ctx, e.pool, e.cfg); err != nil {
			t.Fatal(err)
		}
	}
	if n := fin.logins.Load() - before; n != 0 {
		t.Errorf("5 health checks logged in %d times, want 0", n)
	}
	if n := fin.health.Load(); n != 5 {
		t.Errorf("5 health checks asked sys/health %d times, want 5", n)
	}
	open := func() string {
		var d string
		_ = e.pool.QueryRow(`SELECT detail FROM notices WHERE kind = ? AND subject = 'ag:FIN' AND resolved_at IS NULL`,
			settings.KindAgencyVaultUnreachable).Scan(&d)
		return d
	}
	if d := open(); d != "" {
		t.Errorf("a healthy Vault has an open notice: %s", d)
	}
	fin.sealed.Store(true)
	if err := settings.CheckAgencyVaults(ctx, e.pool, e.cfg); err != nil {
		t.Fatal(err)
	}
	if d := open(); !strings.Contains(d, "sealed") {
		t.Errorf("a sealed Vault's notice = %q, want it to say sealed", d)
	}
	fin.sealed.Store(false)
	if err := settings.CheckAgencyVaults(ctx, e.pool, e.cfg); err != nil {
		t.Fatal(err)
	}
	if d := open(); d != "" {
		t.Errorf("the notice stayed open once the Vault answered: %s", d)
	}
}

// A6: a Test with proposed credentials logs in with a client of its own. It
// used the cached one: with the stored credentials it dropped the live
// client's token, and with others it took the live client's cache slot, so
// the next read logged in again.
func TestSG6_ATestLeavesTheLiveClient(t *testing.T) {
	fin := newSG6Vault(t, sg3Value)
	e := newSG6Env(t, fin)
	e.connect(t, "ag:FIN", gFinAdmin, fin)
	ctx := context.Background()
	id := e.vaultSecret(t, "FIN_DB", "secret/data/fin/db#value", "ag:FIN")
	w := e.wired()
	if v, err := w.Reveal(ctx, id); err != nil || v != sg3Value {
		t.Fatalf("first read = %q %v", v, err)
	}
	before := fin.logins.Load()
	test := func(body string, wantOK bool) {
		t.Helper()
		rec := gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/test", gFinAdmin, body)
		var out struct {
			OK bool `json:"ok"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.OK != wantOK {
			t.Fatalf("test = %d %s, want ok=%t", rec.Code, rec.Body, wantOK)
		}
	}
	u := fin.URL
	test(`{"addr":"`+u+`"}`, true) // the stored credentials
	test(e.connectBody(fin, "wrong-secret-id"), false)
	for range 3 {
		if v, err := w.Reveal(ctx, id); err != nil || v != sg3Value {
			t.Fatalf("read after the tests = %q %v", v, err)
		}
	}
	if n := fin.logins.Load() - before; n != 2 {
		t.Errorf("two Tests and three reads logged in %d times, want 2 (the Tests' own)", n)
	}
	// The preview, the same.
	rec := gateReq(t, e.h, http.MethodPost, "/api/v1/agencies/ag:FIN/vault/preview", gFinAdmin, e.connectBody(fin, "wrong-secret-id"))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	mid := fin.logins.Load()
	if _, err := w.Reveal(ctx, id); err != nil {
		t.Fatal(err)
	}
	if fin.logins.Load() != mid {
		t.Errorf("a read after a preview with other credentials logged in again")
	}
}
