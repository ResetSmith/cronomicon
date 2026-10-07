package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// LR-80, LR-81, LR-82 — the Vault paths an agency may name.
//
// The installation has one Vault connection. From 2.2.2 every Vault path was a
// global administrator's to name, which closed the hole (an agency binding
// another's path) and left an agency unable to manage its own Vault-backed
// secrets. A global administrator now assigns each agency its prefixes, and
// the agency writes inside them.

func setPrefixes(t *testing.T, h http.Handler, who, agency string, prefixes ...string) (int, []string, string) {
	t.Helper()
	if prefixes == nil {
		prefixes = []string{} // an explicit empty list clears; a missing one is refused
	}
	body, _ := json.Marshal(map[string]any{"prefixes": prefixes})
	rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/vault-prefixes", who, string(body))
	var out struct {
		Prefixes []string `json:"prefixes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out.Prefixes, errCode(rec.Body.Bytes())
}

func TestVaultPrefixes_AreAssignedByAGlobalAdministratorAndReadByTheAgency(t *testing.T) {
	h, _ := gateServer(t)

	// Stored normalised and without what adds nothing: a repeat, and a prefix
	// that lies inside another of the same save.
	code, got, _ := setPrefixes(t, h, gRoot, "ag:FIN", " /secret/data/fin/ ", "secret/data/fin", "secret/data/fin/apps", "kv/fin")
	if code != http.StatusOK || strings.Join(got, "|") != "kv/fin|secret/data/fin" {
		t.Fatalf("root assigning FIN's prefixes = %d %v, want 200 [kv/fin secret/data/fin]", code, got)
	}
	// Not the agency's own administrator's to assign: which part of the one
	// Vault an agency may point at is the installation's decision.
	for _, who := range []string{gFinAdmin, gMixed, gAllViewer} {
		if code, _, _ := setPrefixes(t, h, who, "ag:FIN", "secret/data"); code != http.StatusForbidden {
			t.Errorf("%s assigning FIN's prefixes = %d, want 403", who, code)
		}
	}
	// Refused whole, with nothing written: anything whose meaning to Vault is in doubt.
	for _, bad := range []string{"secret/data/../data/fin", "secret//fin", "secret/data/%2e%2e", ""} {
		if code, _, ec := setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/fin", bad); code != http.StatusUnprocessableEntity || ec != "validation_failed" {
			t.Errorf("a prefix %q = %d %s, want 422 validation_failed", bad, code, ec)
		}
	}
	if code, _, ec := setPrefixes(t, h, gRoot, "global", "secret"); code != http.StatusUnprocessableEntity || ec != "builtin_agency" {
		t.Errorf("assigning Global a prefix = %d %s, want 422 builtin_agency (Global has no path limit)", code, ec)
	}
	if code, _, _ := setPrefixes(t, h, gRoot, "no-such-agency", "secret"); code != http.StatusNotFound {
		t.Errorf("assigning an unknown agency a prefix = %d, want 404", code)
	}

	read := func(who, agency string) (int, string) {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/agencies/"+agency+"/vault-prefixes", who, "")
		var out struct {
			Prefixes []string `json:"prefixes"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, strings.Join(out.Prefixes, "|")
	}
	for _, who := range []string{gRoot, gFinAdmin} {
		if code, got := read(who, "ag:FIN"); code != http.StatusOK || got != "kv/fin|secret/data/fin" {
			t.Errorf("%s reading FIN's prefixes = %d %q, want the two that survived the refused saves", who, code, got)
		}
	}
	// Another agency's are not everyone's to read, and reach is not authority.
	for _, who := range []string{gTaxAdmin, gFinViewer, gAllViewer} {
		if code, _ := read(who, "ag:FIN"); code != http.StatusForbidden {
			t.Errorf("%s reading FIN's prefixes = %d, want 403", who, code)
		}
	}
	if code, got, _ := setPrefixes(t, h, gRoot, "ag:FIN"); code != http.StatusOK || len(got) != 0 {
		t.Errorf("clearing FIN's prefixes = %d %v, want 200 []", code, got)
	}
}

func TestVaultPrefixes_AnAgencyNamesPathsInsideItsOwnAndNoOthers(t *testing.T) {
	h, pool := gateServer(t)
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/fin")
	setPrefixes(t, h, gRoot, "ag:TAX", "secret/data/tax")

	secret := func(who, key, path, agency string) (int, string, string) {
		body := `{"key":"` + key + `","source":"vault","scope":"fin-hosts","vaultPath":"` + path + `"`
		if agency != "" {
			body += `,"agencyIds":["` + agency + `"]`
		}
		rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", who, body+`}`)
		var s struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return rec.Code, errCode(rec.Body.Bytes()), s.ID
	}
	code, _, finSecret := secret(gFinAdmin, "FIN_DB", "secret/data/fin/db#password", "")
	if code != http.StatusCreated {
		t.Fatalf("FIN's administrator creating a Vault-backed secret inside FIN's prefix = %d, want 201", code)
	}
	for _, c := range []struct{ name, path, want string }{
		{"another agency's path", "secret/data/tax/db#password", "vault_path_not_allowed"},
		{"a sibling that shares the characters, not the segment", "secret/data/fin-audit/db#password", "vault_path_not_allowed"},
		{"the parent of the prefix", "secret/data#password", "vault_path_not_allowed"},
		{"a path that climbs out with ..", "secret/data/fin/../tax/db#password", "validation_failed"},
		{"a path with an encoded ..", "secret/data/fin/%2e%2e/tax/db#password", "validation_failed"},
		{"a doubled slash", "secret/data/fin//db#password", "validation_failed"},
	} {
		if code, ec, _ := secret(gFinAdmin, "FIN_BAD", c.path, ""); code != http.StatusUnprocessableEntity || ec != c.want {
			t.Errorf("FIN naming %s (%s) = %d %s, want 422 %s", c.name, c.path, code, ec, c.want)
		}
	}
	// The rule is about where an agency's secret may point, so it binds a
	// global administrator writing FOR the agency too.
	if code, ec, _ := secret(gRoot, "FIN_BY_ROOT", "secret/data/tax/db#password", "ag:FIN"); code != http.StatusUnprocessableEntity || ec != "vault_path_not_allowed" {
		t.Errorf("a global administrator creating FIN's secret outside FIN's prefix = %d %s, want 422 vault_path_not_allowed", code, ec)
	}
	if code, _, _ := secret(gRoot, "FIN_BY_ROOT", "secret/data/fin/other#k", "ag:FIN"); code != http.StatusCreated {
		t.Errorf("a global administrator creating FIN's secret inside FIN's prefix = %d, want 201", code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE key IN ('FIN_BAD')`); n != 0 {
		t.Fatal("a refused secret was written")
	}

	// An edit: the path sent, or the one the row has when the edit leaves it alone.
	put := func(who, body string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/env-secrets/"+finSecret, who, body)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	if code, ec := put(gFinAdmin, `{"key":"FIN_DB","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/tax/db#password"}`); code != http.StatusUnprocessableEntity || ec != "vault_path_not_allowed" {
		t.Errorf("re-pointing FIN's secret at TAX's path = %d %s, want 422 vault_path_not_allowed", code, ec)
	}
	if code, _ := put(gFinAdmin, `{"key":"FIN_DB","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/fin/db2#password"}`); code != http.StatusOK {
		t.Errorf("re-pointing FIN's secret inside FIN's prefix = %d, want 200", code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id = ? AND vault_ref = 'secret/data/fin/db2#password'`, finSecret); n != 1 {
		t.Error("the allowed edit was not stored, or the refused one was")
	}

	// A move: the secret must lie inside the TARGET agency's paths.
	both := gFinAdmin + "," + gTaxAdmin
	move := func(who, agency string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", who, `[{"id":"`+finSecret+`","agencyIds":["`+agency+`"]}]`)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	if code, ec := move(both, "ag:TAX"); code != http.StatusUnprocessableEntity || ec != "vault_path_not_allowed" {
		t.Errorf("moving a secret under FIN's path into TAX = %d %s, want 422 vault_path_not_allowed", code, ec)
	}
	setPrefixes(t, h, gRoot, "ag:TAX", "secret/data/tax", "secret/data/fin/db2")
	if code, ec := move(both, "ag:TAX"); code != http.StatusOK {
		t.Errorf("moving it once TAX is assigned a path that covers it = %d %s, want 200", code, ec)
	}

	// SSH keys are the same door: what Vault returns is shipped as key material.
	key := func(who, label, ref string) (int, string) {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/ssh/credentials", who, `{"label":"`+label+`","source":"vault","vaultRef":"`+ref+`"}`)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	if code, ec := key(gFinAdmin, "fin_vault_key", "secret/data/tax/keys/deploy#key"); code != http.StatusUnprocessableEntity || ec != "vault_path_not_allowed" {
		t.Errorf("FIN creating a Vault-backed key at TAX's path = %d %s, want 422 vault_path_not_allowed", code, ec)
	}
	if code, ec := key(gFinAdmin, "fin_vault_key", "secret/data/fin/keys/deploy#key"); code != http.StatusCreated {
		t.Errorf("FIN creating a Vault-backed key inside FIN's prefix = %d %s, want 201", code, ec)
	}
}

// LR-81 — the prefix is checked when a row is written or moved, not when a run
// resolves it. Removing a prefix does not revoke what was written under it; the
// inbox says what now sits outside, to the global administrators who assign
// prefixes, until one covers it again.
func TestVaultPrefixes_RemovingOneRaisesANoticeAndRevokesNothing(t *testing.T) {
	h, pool := gateServer(t)
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/fin")
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
		`{"key":"FIN_DB","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/fin/db#password"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d (%s)", rec.Code, rec.Body)
	}
	// A Global Vault-backed secret has no path limit and is never listed.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gRoot, `{"key":"SHARED","source":"vault","vaultPath":"anything/at/all#k"}`); rec.Code != http.StatusCreated {
		t.Fatalf("seed global = %d (%s)", rec.Code, rec.Body)
	}
	const kind = "vault_path_outside_prefix"
	outside := func() map[string]noticeRow {
		t.Helper()
		// The inbox refreshes at most once in 30 seconds; this test changes the
		// world between reads, so it ages the last run.
		out := map[string]noticeRow{}
		for k, n := range listNoticesFresh(t, h, pool, gRoot) {
			if n.Kind == kind {
				out[k] = n
			}
		}
		return out
	}
	if got := outside(); len(got) != 0 {
		t.Fatalf("a secret inside its agency's prefix is listed: %v", got)
	}
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/elsewhere")
	got := outside()
	if len(got) != 1 {
		t.Fatalf("after FIN's prefix was replaced: %v, want FIN's secret listed", got)
	}
	for _, n := range got {
		if n.AgencyID != "global" || !strings.Contains(n.Detail, "FIN_DB") || !strings.Contains(n.Detail, "secret/data/fin/db") || strings.Contains(n.Detail, "password") {
			t.Errorf("the notice = %+v; want it under Global, naming the secret and its path and not the field", n)
		}
	}
	// Nothing was revoked: the row still names its path.
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE key='FIN_DB' AND vault_ref='secret/data/fin/db#password'`); n != 1 {
		t.Error("removing the prefix changed the secret")
	}
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data")
	if got := outside(); len(got) != 0 {
		t.Errorf("after a covering prefix was assigned the notice is still open: %v", got)
	}
}

// A row from before 2.3.0 may be owned by one agency and shared with another.
// The gates around a write pass an administrator of ANY agency the row is in;
// which Vault path it names is its owner's alone. Without that, the sharing
// agency could re-point the owner's Vault-backed key anywhere in the owner's
// Vault subtree, migrate a secret into it, and read the owner's prefixes off
// the refusal.
func TestVaultPrefixes_OnlyTheOwnerNamesASharedRowsPath(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	setPrefixes(t, h, gRoot, "ag:TAX", "secret/data/tax-private")
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/fin")
	// TAX's stored key and TAX's stored secret, each also shared with FIN.
	exec(`INSERT INTO ssh_credentials (id,label,source,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
	      VALUES ('k-tax','tax_deploy','stored','ag:TAX','seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES ('k-tax','ag:TAX'), ('k-tax','ag:FIN')`)
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gTaxAdmin, `{"key":"TAX_DB","source":"stored","scope":"tax-hosts","value":"v"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed secret = %d (%s)", rec.Code, rec.Body)
	}
	var sec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sec)
	// Shared with FIN, in a scope FIN can also write: the pre-2.3.0 shape.
	exec(`UPDATE secrets SET scope = 'fin-hosts' WHERE id = ?`, sec.ID)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES (?, 'ag:FIN')`, sec.ID)

	for _, c := range []struct{ name, method, path, body string }{
		{"re-pointing TAX's key inside TAX's prefix", http.MethodPut, "/api/v1/ssh/credentials/k-tax",
			`{"label":"tax_deploy","source":"vault","vaultRef":"secret/data/tax-private/other#pem"}`},
		{"re-pointing TAX's key outside it", http.MethodPut, "/api/v1/ssh/credentials/k-tax",
			`{"label":"tax_deploy","source":"vault","vaultRef":"secret/data/elsewhere#pem"}`},
		{"migrating TAX's secret into TAX's prefix", http.MethodPost, "/api/v1/env-secrets/" + sec.ID + "/migrate-to-vault",
			`{"vaultPath":"secret/data/tax-private/db#password"}`},
		{"migrating TAX's secret elsewhere", http.MethodPost, "/api/v1/env-secrets/" + sec.ID + "/migrate-to-vault",
			`{"vaultPath":"secret/data/elsewhere#password"}`},
		{"re-sourcing TAX's secret to Vault", http.MethodPut, "/api/v1/env-secrets/" + sec.ID,
			`{"key":"TAX_DB","source":"vault","scope":"fin-hosts","vaultPath":"secret/data/tax-private/db#password"}`},
	} {
		rec := gateReq(t, h, c.method, c.path, gFinAdmin, c.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("FIN's administrator %s = %d, want 403 (%s)", c.name, rec.Code, rec.Body)
		}
		// Whatever the answer, it does not tell FIN where TAX may point.
		if strings.Contains(rec.Body.String(), "tax-private") {
			t.Errorf("the refusal for %s names TAX's Vault prefix: %s", c.name, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM ssh_credentials WHERE id='k-tax' AND source='stored' AND vault_ref IS NULL)
	                             + (SELECT COUNT(*) FROM secrets WHERE id=? AND source='stored' AND vault_ref IS NULL)`, sec.ID); n != 2 {
		t.Fatal("a refused request re-pointed TAX's key or secret")
	}
	// The owner's own administrator is judged on the path, as for any of its rows.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/ssh/credentials/k-tax", gTaxAdmin,
		`{"label":"tax_deploy","source":"vault","vaultRef":"secret/data/elsewhere#pem"}`); rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "vault_path_not_allowed" {
		t.Errorf("TAX's administrator naming a path outside TAX's prefix = %d %s, want 422 vault_path_not_allowed", rec.Code, errCode(rec.Body.Bytes()))
	}
	// And the inbox lists the shared rows, whoever owns them.
	shared := 0
	for _, n := range listNoticesFresh(t, h, pool, gRoot) {
		if n.Kind == "shared_ownership" && strings.Contains(n.Detail, "owned by TAX") && strings.Contains(n.Detail, "FIN, TAX") {
			shared++
		}
	}
	if shared != 2 {
		t.Errorf("%d shared_ownership notices name TAX's two shared rows, want 2", shared)
	}
}

// What is judged is what is stored and sent: a reference is not trimmed into
// shape, and a save that names no list does not clear one.
func TestVaultPrefixes_AreJudgedAsWrittenAndReplacedOnlyOnPurpose(t *testing.T) {
	h, pool := gateServer(t)
	setPrefixes(t, h, gRoot, "ag:FIN", "secret/data/fin")
	for _, path := range []string{"/secret/data/fin/db#k", "secret/data/fin/db/#k", "//secret/data/fin/db#k"} {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", gFinAdmin,
			`{"key":"FIN_SLASH","source":"vault","scope":"fin-hosts","vaultPath":"`+path+`"}`)
		if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "validation_failed" {
			t.Errorf("a path written %q = %d %s, want 422 validation_failed: it would be stored and sent with its slashes", path, rec.Code, errCode(rec.Body.Bytes()))
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE key='FIN_SLASH'`); n != 0 {
		t.Error("a path that needed trimming was stored")
	}
	for _, body := range []string{`{}`, `{"prefix":["secret/data/other"]}`, `{"prefixes":null}`} {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/vault-prefixes", gRoot, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %s = %d, want 422 (%s)", body, rec.Code, rec.Body)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM agency_vault_prefixes WHERE agency_id='ag:FIN' AND prefix='secret/data/fin'`); n != 1 {
		t.Fatal("a save that named no list cleared FIN's prefixes")
	}
}
