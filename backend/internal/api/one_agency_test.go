package api_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// LR-7, LR-54 — a scope, a secret, a variable and an SSH key each belong to
// exactly one agency, or to Global. What two agencies both need is a copy in
// each, or a Global row.

func TestOneAgency_CreationTakesExactlyOne(t *testing.T) {
	h, pool := gateServer(t)
	both := gFinAdmin + "," + gTaxAdmin
	for _, c := range []struct{ name, path, body string }{
		{"scope", "/api/v1/scopes", `{"scope":"two-homes","hosts":["h1"],"agencyIds":["ag:FIN","ag:TAX"]}`},
		{"secret", "/api/v1/env-secrets", `{"key":"TWO_HOMES","source":"stored","scope":"fin-hosts","value":"v","agencyIds":["ag:FIN","ag:TAX"]}`},
		{"variable", "/api/v1/env-vars", `{"key":"TWO_HOMES_V","scope":"fin-hosts","value":"v","agencyIds":["ag:FIN","ag:TAX"]}`},
	} {
		// Even for someone who administers both, and for a global administrator.
		for _, who := range []string{both, gRoot} {
			rec := gateReq(t, h, http.MethodPost, c.path, who, c.body)
			if rec.Code != http.StatusUnprocessableEntity || errCode(rec.Body.Bytes()) != "one_agency" {
				t.Errorf("%s creating a %s in two agencies = %d %s, want 422 one_agency (%s)", who, c.name, rec.Code, errCode(rec.Body.Bytes()), rec.Body)
			}
		}
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM scopes WHERE name='two-homes') + (SELECT COUNT(*) FROM secrets WHERE key='TWO_HOMES') + (SELECT COUNT(*) FROM env_vars WHERE key='TWO_HOMES_V')`); n != 0 {
		t.Fatalf("%d refused rows were created", n)
	}
	// One agency named twice is one agency.
	rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", both, `{"key":"ONE_HOME","source":"stored","scope":"tax-hosts","value":"v","agencyIds":["ag:TAX","ag:TAX"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("naming one agency twice = %d, want 201 (%s)", rec.Code, rec.Body)
	}
	// The agency is the owner (LR-54).
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets s JOIN secret_agencies m ON m.secret_id = s.id
	                         WHERE s.key='ONE_HOME' AND s.owner_agency='ag:TAX' AND m.agency_id='ag:TAX'`); n != 1 {
		t.Error("the new secret is not owned by, and a member of, the one agency named")
	}
}

// A secret is MOVED by setting its agency, owner and all, by someone with the
// permission on both sides. Until 2.3.0 an owned row could not leave its owner:
// it had to be deleted and created again, and a stored secret's value with it.
func TestOneAgency_ASecretMovesWithItsOwner(t *testing.T) {
	h, pool := gateServer(t)
	both := gFinAdmin + "," + gTaxAdmin
	create := func(who, key, scope string) string {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/env-secrets", who, `{"key":"`+key+`","source":"stored","scope":"`+scope+`","value":"v"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s creating %s = %d (%s)", who, key, rec.Code, rec.Body)
		}
		var s struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &s)
		return s.ID
	}
	state := func(id string) string {
		t.Helper()
		var owner, members string
		if err := pool.QueryRow(`SELECT owner_agency, (SELECT group_concat(agency_id, ',') FROM secret_agencies WHERE secret_id = secrets.id) FROM secrets WHERE id = ?`, id).Scan(&owner, &members); err != nil {
			t.Fatal(err)
		}
		return owner + "/" + members
	}
	move := func(who, id, agency string) (int, string) {
		rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", who, `[{"id":"`+id+`","agencyIds":["`+agency+`"]}]`)
		return rec.Code, errCode(rec.Body.Bytes())
	}

	sec := create(gFinAdmin, "DB_PASSWORD", "fin-hosts")
	if got := state(sec); got != "ag:FIN/ag:FIN" {
		t.Fatalf("a secret created by FIN's administrator = %s, want owned by and in FIN", got)
	}
	// One side is not enough, in either direction, and Global is a side.
	for _, c := range []struct{ who, to string }{{gFinAdmin, "ag:TAX"}, {gTaxAdmin, "ag:TAX"}, {gFinAdmin, "global"}, {both, "global"}} {
		if code, _ := move(c.who, sec, c.to); code != http.StatusForbidden {
			t.Errorf("%s moving FIN's secret to %s = %d, want 403", c.who, c.to, code)
		}
	}
	if code, ec := move(both, sec, "ag:FIN\",\"ag:TAX"); code != http.StatusUnprocessableEntity || ec != "one_agency" {
		t.Errorf("putting a secret in two agencies = %d %s, want 422 one_agency", code, ec)
	}
	if got := state(sec); got != "ag:FIN/ag:FIN" {
		t.Fatalf("a refused move changed the secret: %s", got)
	}

	if code, ec := move(both, sec, "ag:TAX"); code != http.StatusOK {
		t.Fatalf("an administrator of both agencies moving the secret = %d %s, want 200", code, ec)
	}
	if got := state(sec); got != "ag:TAX/ag:TAX" {
		t.Errorf("after the move the secret is %s, want owned by and in TAX alone", got)
	}
	// FIN's administrator has no say over it any more; TAX's has.
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/env-secrets/"+sec, gFinAdmin, ""); rec.Code/100 == 2 {
		t.Errorf("FIN's administrator deleted a secret that was moved to TAX (%d)", rec.Code)
	}
	// A global administrator makes it Global's, and takes it back out.
	if code, _ := move(gRoot, sec, "global"); code != http.StatusOK {
		t.Errorf("a global administrator making the secret Global's = %d, want 200", code)
	}
	if got := state(sec); got != "global/global" {
		t.Errorf("after the move to Global the secret is %s", got)
	}

	// A move must not land on a name the target already owns.
	finDup := create(gFinAdmin, "API_TOKEN", "fin-hosts")
	exec := mustExec(t, pool)
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES ('tax-dup', 'API_TOKEN', 'fin-hosts', 'stored', '2026-01-01T00:00:00Z', 'ag:TAX')`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES ('tax-dup', 'ag:TAX')`)
	if code, ec := move(both, finDup, "ag:TAX"); code != http.StatusConflict || ec != "owner_conflict" {
		t.Errorf("moving a secret onto a name TAX already owns in that scope = %d %s, want 409 owner_conflict", code, ec)
	}
	if got := state(finDup); got != "ag:FIN/ag:FIN" {
		t.Errorf("a refused move changed the secret: %s", got)
	}
}

// LR-73 — the SSH credential a run "connects as" is named by label, and labels
// are unique per owner, not across them. The label is resolved as the run's
// agency sees it: its own key before Global's, never another agency's — and a
// label only another agency holds is, to this caller, a label nobody holds.
func TestOneAgency_ARunsConnectAsKeyIsItsOwnAgencys(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	for id, r := range map[string][2]string{
		"k-fin": {"deploy", "ag:FIN"}, "k-tax": {"deploy", "ag:TAX"}, "k-tax-only": {"tax_only", "ag:TAX"}, "k-glob": {"shared", "global"},
	} {
		exec(`INSERT INTO ssh_credentials (id,label,source,owner_agency,created_by,created_at,last_modified_by,last_modified_at)
		      VALUES (?,?,'stored',?,'seed','2026-01-01T00:00:00Z','seed','2026-01-01T00:00:00Z')`, id, r[0], r[1])
		if r[1] != "global" {
			exec(`INSERT INTO ssh_credential_agencies (credential_id, agency_id) VALUES (?,?)`, id, r[1])
		}
	}
	exec(`INSERT INTO scope_hosts (scope_id, host) VALUES ('sc:fin','fin01')`)
	exec(`INSERT INTO jobs (name,source,run_type,scope,command,enabled,created_at,executor)
	      VALUES ('fin-job','cronomicon','bash','fin-hosts','true',1,'2026-01-01T00:00:00Z','ssh')`)
	job := rowID(t, pool, `SELECT rowid FROM jobs WHERE name='fin-job'`)
	run := func(who, label string) *http.Response {
		rec := gateReq(t, h, http.MethodPost, "/api/v1/jobs/"+job+"/run", who, `{"sshCredential":"`+label+`","executor":"ssh"}`)
		return rec.Result()
	}
	// A label only TAX holds, and one nobody holds, get one answer.
	other, missing := run(gFinAdmin, "tax_only"), run(gFinAdmin, "no_such_key")
	if other.StatusCode != http.StatusUnprocessableEntity || missing.StatusCode != other.StatusCode {
		t.Errorf("another agency's label = %d, a missing label = %d; want 422 for both", other.StatusCode, missing.StatusCode)
	}
	// A label FIN and TAX both hold is FIN's key for FIN's run; a Global one is usable.
	if res := run(gFinAdmin, "deploy"); res.StatusCode/100 != 2 {
		t.Errorf("FIN's own key by its label = %d, want 2xx", res.StatusCode)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runs WHERE job_name='fin-job' AND ssh_credential='deploy'`); n != 1 {
		t.Errorf("%d runs carry the chosen credential, want 1", n)
	}
	// Global's key takes a global administrator's permission over it to select.
	if res := run(gFinAdmin, "shared"); res.StatusCode != http.StatusForbidden {
		t.Errorf("FIN's administrator selecting a Global key = %d, want 403 (it is a global administrator's to hand out)", res.StatusCode)
	}
	if res := run(gRoot, "shared"); res.StatusCode/100 != 2 {
		t.Errorf("a global administrator selecting a Global key = %d, want 2xx", res.StatusCode)
	}
}

// A row from before 2.3.0 may still be in two agencies. Settling it — naming
// its one agency — replaces the whole list, so it takes authority over EVERY
// agency it is in: an administrator of one of them cannot take it from the
// other, least of all a secret the other owns.
func TestOneAgency_ASharedRowIsSettledByAllItsAgenciesNotTakenByOne(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	both := gFinAdmin + "," + gTaxAdmin
	// The pre-2.3.0 shapes: a scope in both, a secret both use that TAX owns,
	// and one the upgrade left Global-owned with both as members.
	exec(`INSERT INTO scopes (id,name,source,created_at) VALUES ('sc:both','both-hosts','cronomicon','2026-01-01T00:00:00Z')`)
	exec(`DELETE FROM scope_agencies WHERE scope_id = 'sc:both'`)
	exec(`INSERT INTO scope_agencies (scope_id, agency_id) VALUES ('sc:both','ag:FIN'), ('sc:both','ag:TAX')`)
	exec(`INSERT INTO secrets (id, key, scope, source, created_at, owner_agency) VALUES
	      ('s-tax-owned', 'TAX_OWNED', 'both-hosts', 'stored', '2026-01-01T00:00:00Z', 'ag:TAX'),
	      ('s-shared', 'SHARED', 'both-hosts', 'stored', '2026-01-01T00:00:00Z', 'global')`)
	exec(`DELETE FROM secret_agencies WHERE secret_id IN ('s-tax-owned', 's-shared')`)
	exec(`INSERT INTO secret_agencies (secret_id, agency_id) VALUES
	      ('s-tax-owned','ag:FIN'), ('s-tax-owned','ag:TAX'), ('s-shared','ag:FIN'), ('s-shared','ag:TAX')`)
	before := count(t, pool, `SELECT (SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:both') + (SELECT COUNT(*) FROM secret_agencies WHERE secret_id IN ('s-tax-owned','s-shared'))`)

	for _, c := range []struct{ path, body string }{
		{"/api/v1/scopes/sc:both/agency", `{"agencyId":"ag:FIN"}`},
		{"/api/v1/scope-agencies", `[{"id":"sc:both","agencyIds":["ag:FIN"]}]`},
		{"/api/v1/secret-agencies", `[{"id":"s-tax-owned","agencyIds":["ag:FIN"]}]`},
		{"/api/v1/secret-agencies", `[{"id":"s-shared","agencyIds":["ag:FIN"]}]`},
	} {
		if rec := gateReq(t, h, http.MethodPut, c.path, gFinAdmin, c.body); rec.Code != http.StatusForbidden {
			t.Errorf("FIN's administrator alone settling a shared row on FIN: PUT %s %s = %d, want 403 (%s)", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if after := count(t, pool, `SELECT (SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:both') + (SELECT COUNT(*) FROM secret_agencies WHERE secret_id IN ('s-tax-owned','s-shared'))`); after != before {
		t.Fatalf("a refused move changed membership: %d rows, were %d", after, before)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets WHERE id='s-tax-owned' AND owner_agency='ag:TAX'`); n != 1 {
		t.Fatal("TAX's secret changed owner under FIN's administrator")
	}

	// It may give up its own share: the row is left with the other agency.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/ag:FIN/members", gFinAdmin,
		`{"members":[{"kind":"scope","id":"sc:fin"},{"kind":"scope","id":"sc:both"},{"kind":"secret","id":"s-tax-owned"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("FIN giving up its share of the Global-owned secret = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM secrets s WHERE s.id='s-shared' AND s.owner_agency='ag:TAX'
	                          AND (SELECT group_concat(agency_id) FROM secret_agencies WHERE secret_id = s.id) = 'ag:TAX'`); n != 1 {
		t.Error("the secret FIN gave up is not TAX's, owner and member")
	}
	// Someone who administers both settles the rest, and so does a global administrator.
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/sc:both/agency", both, `{"agencyId":"ag:FIN"}`); rec.Code != http.StatusOK {
		t.Errorf("an administrator of both agencies settling the scope = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/secret-agencies", gRoot, `[{"id":"s-tax-owned","agencyIds":["ag:FIN"]}]`); rec.Code != http.StatusOK {
		t.Errorf("a global administrator settling the secret = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT (SELECT COUNT(*) FROM scope_agencies WHERE scope_id='sc:both') + (SELECT COUNT(*) FROM secret_agencies WHERE secret_id='s-tax-owned')`); n != 2 {
		t.Errorf("after settling, the scope and the secret hold %d agency rows between them, want one each", n)
	}
}
