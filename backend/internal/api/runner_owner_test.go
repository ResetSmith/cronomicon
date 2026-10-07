package api_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/ResetSmith/cronomicon/internal/notices"
)

// realHostKey makes a host key and the known_hosts line a runner would upload
// for it, so a test can drive an APPROVAL: the server re-derives the
// fingerprint from the line and refuses a row that does not hold up.
func realHostKey(t *testing.T, host string) (line, keyType, fingerprint string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), key.Type(), ssh.FingerprintSHA256(key)
}

// Phase G3 (LR-58 to LR-64, MA-9 to MA-12, MA-32) — a runner has an owner.
//
// An agent serves exactly the agency that owns it, set by its registration
// token; that agency's administrators enrol and manage it. The cast, on top of
// the two-agency fixture:
//
//	r-fin     FIN's own agent
//	r-tax     TAX's own agent
//	r-global  Global's own agent (what a general-pool runner became)
//	r-legacy  a legacy placement: Global's, serving FIN and TAX since before
//	          2.3.0. No route makes another; it can only be narrowed.

func ownerCast(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin", "fin-agent", "ag:FIN")
	seedBindingRunner(exec, "r-tax", "tax-agent", "ag:TAX")
	seedBindingRunner(exec, "r-global", "global-agent", "")
	exec(`INSERT INTO runners (id,name,status,registered_at,created_at)
	      VALUES ('r-legacy','legacy','online','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`)
	exec(`INSERT INTO runner_agencies (runner_id, agency_id) VALUES ('r-legacy','ag:FIN'), ('r-legacy','ag:TAX')`)
	return h, pool
}

func served(t *testing.T, pool *sql.DB, runnerID string) []string {
	t.Helper()
	rows, err := pool.Query(`SELECT agency_id FROM runner_agencies WHERE runner_id = ? ORDER BY agency_id`, runnerID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

type tokenJSON struct {
	ID         int64  `json:"id"`
	Label      string `json:"label"`
	AgencyID   string `json:"agencyId"`
	AgencyName string `json:"agencyName"`
	Status     string `json:"status"`
	Token      string `json:"token"`
}

// LR-61 — a token names the owner, and minting one takes configureApp on that
// agency. An agency's administrators enrol their own agents; a global
// administrator enrols one for any agency, or for Global.
func TestG3_MintingATokenTakesAuthorityOverItsAgency(t *testing.T) {
	h, pool := ownerCast(t)
	const path = "/api/v1/runners/registration-tokens"

	for _, tc := range []struct {
		name, who, body string
		want            int
		agency          string
	}{
		{"an agency admin, naming their agency", gFinAdmin, `{"agencyId":"ag:FIN","label":"web-01"}`, 201, "ag:FIN"},
		{"an agency admin, naming none: their one agency", gFinAdmin, `{}`, 201, "ag:FIN"},
		{"an agency admin, with no body at all", gFinAdmin, ``, 201, "ag:FIN"},
		{"an agency admin, naming another agency", gFinAdmin, `{"agencyId":"ag:TAX"}`, 403, ""},
		{"an agency admin, naming Global", gFinAdmin, `{"agencyId":"global"}`, 403, ""},
		{"an all-scopes viewer who administers FIN, naming none", gMixed, `{}`, 201, "ag:FIN"},
		{"an all-scopes viewer who administers FIN, naming Global", gMixed, `{"agencyId":"global"}`, 403, ""},
		{"a global admin, naming none: Global", gRoot, `{}`, 201, "global"},
		{"a global admin, naming an agency (MA-3)", gRoot, `{"agencyId":"ag:TAX"}`, 201, "ag:TAX"},
		{"a global admin, naming an agency that does not exist", gRoot, `{"agencyId":"ag:NOPE"}`, 422, ""},
		{"a viewer", gFinViewer, `{"agencyId":"ag:FIN"}`, 403, ""},
		{"an operator", gFinOperator, `{}`, 403, ""},
		{"an agency id that is not a string", gFinAdmin, `{"agencyId":5}`, 422, ""},
		{"a body that is not JSON", gFinAdmin, `{"label":`, 422, ""},
		// The agency that is authorized must be the agency that is stored. Go's
		// JSON decoder matches a field name in any letter case and lets the
		// last match win, so a second spelling of the key must not be able to
		// name one agency to the gate and another to the mint.
		{"another agency under a differently-cased key", gFinAdmin, `{"agencyid":"ag:TAX"}`, 403, ""},
		{"their own agency, then another under a second spelling", gFinAdmin, `{"agencyId":"ag:FIN","AGENCYID":"ag:TAX"}`, 403, ""},
		{"their own agency, then another under a lower-case spelling", gFinAdmin, `{"agencyId":"ag:FIN","agencyid":"ag:TAX"}`, 403, ""},
		{"another agency under a second spelling, then their own", gFinAdmin, `{"agencyid":"ag:TAX","agencyId":"ag:FIN"}`, 201, "ag:FIN"},
		{"Global under a differently-cased key", gFinAdmin, `{"AgencyId":"global"}`, 403, ""},
	} {
		rec := gateReq(t, h, http.MethodPost, path, tc.who, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body)
			continue
		}
		if tc.want != 201 {
			continue
		}
		var tok tokenJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if tok.AgencyID != tc.agency || tok.Token == "" || tok.Status != "pending" {
			t.Errorf("%s: minted %+v, want a pending token for %s", tc.name, tok, tc.agency)
		}
		if strings.Contains(tc.body, "web-01") && tok.Label != "web-01" {
			t.Errorf("%s: the label was lost on the way to the mint: %+v", tc.name, tok)
		}
		// What was stored, not only what was answered.
		var stored string
		if err := pool.QueryRow(`SELECT agency_id FROM registration_tokens WHERE id = ?`, tok.ID).Scan(&stored); err != nil || stored != tc.agency {
			t.Errorf("%s: the token is stored for %q (err %v), want %s", tc.name, stored, err, tc.agency)
		}
	}
	// No refused mint left a row behind, for any agency.
	if n := count(t, pool, `SELECT COUNT(*) FROM registration_tokens WHERE agency_id = 'ag:TAX' AND created_by NOT LIKE 'root-admins%'`); n != 0 {
		t.Errorf("%d token(s) for TAX were minted by someone who does not administer it", n)
	}
}

// LR-36 — the token list and the revoke follow the same authority as the mint.
// The list was open to every signed-in user until 2.3.0.
func TestG3_TokensAreListedAndRevokedByAuthority(t *testing.T) {
	h, _ := ownerCast(t)
	const path = "/api/v1/runners/registration-tokens"
	mint := func(who, agency string) int64 {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, path, who, `{"agencyId":"`+agency+`"}`)
		if rec.Code != 201 {
			t.Fatalf("mint for %s as %s = %d (%s)", agency, who, rec.Code, rec.Body)
		}
		var tok tokenJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &tok)
		return tok.ID
	}
	finTok, taxTok, globalTok := mint(gFinAdmin, "ag:FIN"), mint(gTaxAdmin, "ag:TAX"), mint(gRoot, "global")

	list := func(who string) []int64 {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, path, who, ``)
		if rec.Code != 200 {
			t.Fatalf("%s listing tokens = %d (%s)", who, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "crn_reg_") {
			t.Fatalf("the token list carries a plaintext token: %s", rec.Body)
		}
		var rows []tokenJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		ids := []int64{}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids
	}
	for who, want := range map[string][]int64{
		gFinAdmin:  {finTok},
		gTaxAdmin:  {taxTok},
		gMixed:     {finTok},
		gRoot:      {finTok, taxTok, globalTok},
		gFinViewer: {},
		gAllViewer: {},
	} {
		if got := list(who); !slices.Equal(got, want) {
			t.Errorf("%s sees tokens %v, want %v", who, got, want)
		}
	}

	revoke := func(who string, id int64) int {
		return gateReq(t, h, http.MethodDelete, fmt.Sprintf("%s/%d", path, id), who, ``).Code
	}
	if code := revoke(gTaxAdmin, finTok); code != 403 {
		t.Errorf("TAX revoking FIN's token = %d, want 403", code)
	}
	if code := revoke(gFinAdmin, globalTok); code != 403 {
		t.Errorf("FIN revoking Global's token = %d, want 403", code)
	}
	if code := revoke(gMixed, globalTok); code != 403 {
		t.Errorf("an all-scopes viewer who administers FIN revoking Global's token = %d, want 403", code)
	}
	if got := list(gRoot); len(got) != 3 {
		t.Fatalf("a refused revoke changed the list: %v", got)
	}
	if code := revoke(gFinAdmin, finTok); code != 204 {
		t.Errorf("FIN revoking its own token = %d, want 204", code)
	}
	if code := revoke(gRoot, taxTok); code != 204 {
		t.Errorf("a global admin revoking TAX's token = %d, want 204", code)
	}
	if code := revoke(gFinAdmin, 999999); code != 404 {
		t.Errorf("revoking a token that does not exist = %d, want the handler's 404", code)
	}
}

// LR-59 — every operator write on a runner is its owner's. One assertion per
// wrapped route: the agency's administrator passes the gate on their own agent,
// and is refused on another agency's, on Global's and on a legacy placement
// that serves them.
func TestG3_EveryRunnerRouteIsTheOwners(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	for _, r := range []string{"r-fin", "r-tax", "r-global", "r-legacy"} {
		exec(`INSERT INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at)
		      VALUES (?, ?, 'h.example', 'ssh-ed25519', 'SHA256:x', 'h.example ssh-ed25519 AAAA', '2026-01-01T00:00:00Z')`, "pk-"+r, r)
	}

	// {id} is the runner. DELETE is last: it removes the row the others need.
	routes := []struct{ method, path, body string }{
		{"PUT", "/api/v1/runner-tags/{id}", `{"tags":["a"]}`},
		{"POST", "/api/v1/runners/{id}/owner", `{"agencyId":"{agency}"}`},
		{"POST", "/api/v1/runners/{id}/drain", `{}`},
		{"POST", "/api/v1/runners/{id}/resync", `{}`},
		{"PATCH", "/api/v1/runners/{id}/settings", `{}`},
		{"PUT", "/api/v1/runners/{id}/secret-injection", `{"allow":true}`},
		{"POST", "/api/v1/runners/host-keys/pk-{id}/resolve", `{"approve":false}`},
		{"GET", "/api/v1/runners/{id}/host-keys", ``},
		{"POST", "/api/v1/runners/{id}/host-keys/provide", `{"lines":["h.example ssh-ed25519 AAAA"],"dryRun":true}`},
		{"POST", "/api/v1/runners/{id}/host-keys/1/remove", `{}`},
		{"POST", "/api/v1/runners/{id}/host-keys/1/resend", `{}`},
		{"POST", "/api/v1/runners/{id}/known-hosts/refresh", `{}`},
		{"POST", "/api/v1/runners/{id}/host-keys/carry?from={id}", `{}`},
		{"POST", "/api/v1/runners/{id}/placement", `{"historyId":1}`},
		{"POST", "/api/v1/runners/{id}/placement/dismiss", `{"historyId":1}`},
		{"POST", "/api/v1/runners/{id}/test", `{}`},
		{"DELETE", "/api/v1/runners/{id}", ``},
	}
	// agency is the one the owner route is asked to hand the runner to: the
	// caller's own, so the refusal under test is the runner gate's and not the
	// target side's.
	call := func(who, runner, agency string, i int) int {
		r := routes[i]
		return gateReq(t, h, r.method, strings.ReplaceAll(r.path, "{id}", runner), who,
			strings.ReplaceAll(r.body, "{agency}", agency)).Code
	}

	// Refusals first: none of them may change anything.
	for _, tc := range []struct{ who, agency, runner, why string }{
		{gFinAdmin, "ag:FIN", "r-tax", "another agency's agent"},
		{gFinAdmin, "ag:FIN", "r-global", "Global's agent"},
		{gFinAdmin, "ag:FIN", "r-legacy", "a legacy placement that serves them (it is Global's)"},
		{gMixed, "ag:FIN", "r-global", "Global's agent, as an all-scopes viewer who administers FIN"},
		{gTaxAdmin, "ag:TAX", "r-fin", "another agency's agent"},
		{gFinAdmin, "ag:FIN", "r-does-not-exist", "a runner that does not exist (no oracle)"},
	} {
		for i, r := range routes {
			if tc.runner == "r-does-not-exist" && strings.Contains(r.path, "/host-keys/pk-") {
				continue // that route names a KEY; with no such key it is the handler's own answer
			}
			if code := call(tc.who, tc.runner, tc.agency, i); code != 403 {
				t.Errorf("%s on %s — %s %s = %d, want 403", tc.who, tc.why, r.method, r.path, code)
			}
		}
	}
	for _, r := range []string{"r-fin", "r-tax", "r-global", "r-legacy"} {
		if n := count(t, pool, `SELECT COUNT(*) FROM runners WHERE id = ?`, r); n != 1 {
			t.Fatalf("a refused request removed %s", r)
		}
	}

	// The owner passes the gate on every one of them. What the handler then
	// answers is its own business; it is never the gate's 403.
	for _, tc := range []struct{ who, agency, runner string }{
		{gFinAdmin, "ag:FIN", "r-fin"},
		{gTaxAdmin, "ag:TAX", "r-tax"},
		{gRoot, "global", "r-global"},
		{gRoot, "ag:FIN", "r-legacy"},
	} {
		for i, r := range routes {
			if code := call(tc.who, tc.runner, tc.agency, i); code == 403 || code == 401 {
				t.Errorf("%s on their own %s — %s %s = %d, want past the gate", tc.who, tc.runner, r.method, r.path, code)
			}
		}
		if n := count(t, pool, `SELECT COUNT(*) FROM runners WHERE id = ?`, tc.runner); n != 0 {
			t.Errorf("%s could not deregister their own %s", tc.who, tc.runner)
		}
	}
}

// The fleet-wide pending list shows a caller the keys of the runners they own.
func TestG3_ThePendingKeyListIsByOwner(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	for _, r := range []string{"r-fin", "r-tax", "r-global", "r-legacy"} {
		exec(`INSERT INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at)
		      VALUES (?, ?, 'h.example', 'ssh-ed25519', 'SHA256:x', 'h.example ssh-ed25519 AAAA', '2026-01-01T00:00:00Z')`, "pk-"+r, r)
	}
	for who, want := range map[string][]string{
		gFinAdmin: {"r-fin"},
		gTaxAdmin: {"r-tax"},
		gMixed:    {"r-fin"},
		gRoot:     {"r-fin", "r-global", "r-legacy", "r-tax"},
	} {
		rec := gateReq(t, h, http.MethodGet, "/api/v1/runners/host-keys/pending", who, ``)
		if rec.Code != 200 {
			t.Fatalf("%s = %d (%s)", who, rec.Code, rec.Body)
		}
		var rows []struct {
			RunnerID string `json:"runnerId"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		got := []string{}
		for _, r := range rows {
			got = append(got, r.RunnerID)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s sees pending keys of %v, want %v", who, got, want)
		}
	}
}

// MA-11 through the API: nobody, a global administrator included, can add an
// agency to an agent's serve list or empty it, on either writer. A legacy
// placement can lose an agency (its owner's act: a global administrator's) and
// never gain one.
func TestG3_NobodyWidensAServeList(t *testing.T) {
	h, pool := ownerCast(t)

	put := func(who, runner string, agencies ...string) (int, string) {
		t.Helper()
		body, _ := json.Marshal([]map[string]any{{"runnerId": runner, "agencyIds": agencies}})
		rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", who, string(body))
		return rec.Code, errCode(rec.Body.Bytes())
	}
	for _, tc := range []struct {
		name, who, runner string
		to                []string
		want              int
		code              string
	}{
		{"a global admin adding an agency to an agency's agent", gRoot, "r-fin", []string{"ag:FIN", "ag:TAX"}, 422, "serve_list_fixed"},
		{"a global admin moving an agency's agent", gRoot, "r-fin", []string{"ag:TAX"}, 422, "serve_list_fixed"},
		{"a global admin moving an agency's agent to Global", gRoot, "r-fin", []string{"global"}, 422, "serve_list_fixed"},
		{"a global admin emptying an agent's list", gRoot, "r-fin", []string{}, 422, "agency_required"},
		{"a global admin giving Global's agent an agency", gRoot, "r-global", []string{"ag:FIN"}, 422, "serve_list_fixed"},
		{"the agency's own admin widening their agent", gFinAdmin, "r-fin", []string{"ag:FIN", "ag:TAX"}, 403, "forbidden"},
		{"an agency admin taking another agency's agent", gFinAdmin, "r-tax", []string{"ag:FIN"}, 403, "forbidden"},
		{"an agency admin taking Global's agent", gFinAdmin, "r-global", []string{"ag:FIN"}, 403, "forbidden"},
		{"an agency admin narrowing a legacy placement to themselves", gFinAdmin, "r-legacy", []string{"ag:FIN"}, 403, "forbidden"},
		{"a global admin widening a legacy placement", gRoot, "r-legacy", []string{"ag:FIN", "ag:TAX", "global"}, 422, ""},
	} {
		before := served(t, pool, tc.runner)
		code, ec := put(tc.who, tc.runner, tc.to...)
		if code != tc.want || (tc.code != "" && ec != tc.code) {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, ec, tc.want, tc.code)
		}
		if got := served(t, pool, tc.runner); !slices.Equal(got, before) {
			t.Errorf("%s: a refused write changed the serve list %v -> %v", tc.name, before, got)
		}
	}
	// Re-posting an agent's own list is accepted from its owner: the matrix
	// posts every runner it shows.
	if code, ec := put(gFinAdmin, "r-fin", "ag:FIN"); code != 200 {
		t.Errorf("FIN re-posting its agent's own list = %d %s, want 200", code, ec)
	}

	// The other writer: an agency's member list. TAX's as it stands, plus FIN's
	// agent — refused for a global administrator too, and nothing is written.
	members := func(who, agency string, refs ...[2]string) (int, string) {
		t.Helper()
		ms := []map[string]string{}
		for _, r := range refs {
			ms = append(ms, map[string]string{"kind": r[0], "id": r[1]})
		}
		body, _ := json.Marshal(map[string]any{"members": ms})
		rec := gateReq(t, h, http.MethodPut, "/api/v1/agencies/"+agency+"/members", who, string(body))
		return rec.Code, errCode(rec.Body.Bytes())
	}
	taxNow := [][2]string{{"scope", "sc:tax"}, {"runner", "r-tax"}, {"runner", "r-legacy"}}
	if code, ec := members(gRoot, "ag:TAX", append(slices.Clone(taxNow), [2]string{"runner", "r-fin"})...); code != 422 || ec != "serve_list_fixed" {
		t.Errorf("a global admin adding FIN's agent to TAX's members = %d %s, want 422 serve_list_fixed", code, ec)
	}
	if code, _ := members(gTaxAdmin, "ag:TAX", append(slices.Clone(taxNow), [2]string{"runner", "r-fin"})...); code != 403 {
		t.Errorf("TAX adding FIN's agent to its own members = %d, want 403", code)
	}
	// TAX dropping its own agent would leave it serving nobody.
	if code, ec := members(gTaxAdmin, "ag:TAX", [2]string{"scope", "sc:tax"}, [2]string{"runner", "r-legacy"}); code != 422 || ec != "owner_removal" {
		t.Errorf("TAX dropping its own agent from its members = %d %s, want 422 owner_removal", code, ec)
	}
	// TAX taking the legacy placement off its list is a write on a runner TAX
	// does not own.
	if code, _ := members(gTaxAdmin, "ag:TAX", [2]string{"scope", "sc:tax"}, [2]string{"runner", "r-tax"}); code != 403 {
		t.Errorf("TAX narrowing a legacy placement = %d, want 403 (it is Global's)", code)
	}
	if got := served(t, pool, "r-fin"); !slices.Equal(got, []string{"ag:FIN"}) {
		t.Fatalf("r-fin serves %v after the refused writes, want [ag:FIN]", got)
	}
	if got := served(t, pool, "r-legacy"); !slices.Equal(got, []string{"ag:FIN", "ag:TAX"}) {
		t.Fatalf("r-legacy serves %v after the refused writes, want both", got)
	}
	// A global administrator narrows it, through this writer.
	if code, ec := members(gRoot, "ag:TAX", [2]string{"scope", "sc:tax"}, [2]string{"runner", "r-tax"}); code != 200 {
		t.Fatalf("a global admin narrowing the legacy placement = %d %s, want 200", code, ec)
	}
	if got := served(t, pool, "r-legacy"); !slices.Equal(got, []string{"ag:FIN"}) {
		t.Errorf("after narrowing it serves %v, want [ag:FIN]", got)
	}
	if code, ec := put(gRoot, "r-legacy", "ag:FIN", "ag:TAX"); code != 422 || ec != "serve_list_fixed" {
		t.Errorf("putting TAX back = %d %s, want 422 serve_list_fixed", code, ec)
	}
}

// MA-12 and MA-28: a legacy placement is listed in the inbox as Global's, and
// it is settled by narrowing it to one agency and handing it to that agency —
// the one owner change there is.
func TestG3_ALegacyPlacementIsNoticedAndHandedOver(t *testing.T) {
	h, pool := ownerCast(t)
	ctx := context.Background()
	const key = "legacy_placement/r-legacy"
	check := func() {
		t.Helper()
		if err := notices.RunChecks(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	check()
	n, ok := listNotices(t, h, gRoot)[key]
	if !ok || n.AgencyID != "global" || !strings.Contains(n.Detail, "FIN, TAX") || !strings.Contains(n.Detail, "re-bind") {
		t.Fatalf("the legacy placement's notice = %+v (found %v), want Global's, naming FIN and TAX and the order of the remedy", n, ok)
	}
	// Its own agents and Global's are not notices.
	for k := range listNotices(t, h, gRoot) {
		if strings.HasPrefix(k, "legacy_placement/") && k != key {
			t.Errorf("unexpected legacy_placement notice %s", k)
		}
	}
	if _, sees := listNotices(t, h, gFinAdmin)[key]; sees {
		t.Error("an agency admin sees the legacy placement's notice; it is Global's")
	}

	owner := func(who, runner, agency string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/"+runner+"/owner", who, `{"agencyId":"`+agency+`"}`)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	// Refused while it still serves two, whoever asks; and every other change.
	for _, tc := range []struct {
		name, who, runner, to string
		want                  int
		code                  string
	}{
		{"a legacy placement that still serves two", gRoot, "r-legacy", "ag:FIN", 422, "owner_change_refused"},
		{"agency to Global", gRoot, "r-fin", "global", 422, "owner_change_refused"},
		{"agency to agency", gRoot, "r-fin", "ag:TAX", 422, "owner_change_refused"},
		{"Global's own agent to an agency it does not serve", gRoot, "r-global", "ag:FIN", 422, "owner_change_refused"},
		{"an agency admin taking a legacy placement", gFinAdmin, "r-legacy", "ag:FIN", 403, "forbidden"},
		{"an agency admin giving their agent away", gFinAdmin, "r-fin", "ag:TAX", 403, "forbidden"},
		{"an agency that does not exist", gRoot, "r-legacy", "ag:NOPE", 422, "unknown_agency"},
	} {
		if code, ec := owner(tc.who, tc.runner, tc.to); code != tc.want || ec != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, code, ec, tc.want, tc.code)
		}
	}

	// Narrow it to FIN. It is still a legacy placement (Global's, serving FIN),
	// and the notice now says what settles it.
	body, _ := json.Marshal([]map[string]any{{"runnerId": "r-legacy", "agencyIds": []string{"ag:FIN"}}})
	if rec := gateReq(t, h, http.MethodPut, "/api/v1/runner-agencies", gRoot, string(body)); rec.Code != 200 {
		t.Fatalf("narrowing = %d (%s)", rec.Code, rec.Body)
	}
	check()
	if n := listNotices(t, h, gRoot)[key]; !strings.Contains(n.Detail, "Hand to an agency") {
		t.Errorf("after narrowing to one agency the notice should point at the hand-over: %+v", n)
	}
	if code, _ := owner(gFinAdmin, "r-legacy", "ag:FIN"); code != 403 {
		t.Errorf("FIN handing Global's runner to itself = %d, want 403", code)
	}
	if code, ec := owner(gRoot, "r-legacy", "ag:FIN"); code != 204 {
		t.Fatalf("Global to the one served agency = %d %s, want 204", code, ec)
	}
	check()
	if _, still := listNotices(t, h, gRoot)[key]; still {
		t.Error("the notice did not resolve once the runner was its agency's own")
	}
	// It is FIN's now: FIN's administrator manages it, and TAX's does not.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-legacy/drain", gFinAdmin, `{}`); rec.Code == 403 {
		t.Errorf("FIN cannot manage the runner it was handed: %d (%s)", rec.Code, rec.Body)
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-legacy/drain", gTaxAdmin, `{}`); rec.Code != 403 {
		t.Errorf("TAX draining FIN's runner = %d, want 403", rec.Code)
	}
}

// The runner list says who owns each runner and whether it is a legacy
// placement.
func TestG3_TheRunnerListCarriesTheOwner(t *testing.T) {
	h, _ := ownerCast(t)
	rec := gateReq(t, h, http.MethodGet, "/api/v1/runners", gFinViewer, ``)
	if rec.Code != 200 {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
	}
	var rows []struct {
		ID          string `json:"id"`
		OwnerAgency *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"ownerAgency"`
		LegacyPlacement   bool `json:"legacyPlacement"`
		CanManage         bool `json:"canManage"`
		CanReviewHostKeys bool `json:"canReviewHostKeys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	// A viewer manages nothing and reviews nothing.
	for _, r := range rows {
		if r.CanManage || r.CanReviewHostKeys {
			t.Errorf("a FIN viewer is told they may manage %s (%v) or review its keys (%v)", r.ID, r.CanManage, r.CanReviewHostKeys)
		}
	}
	// The per-row flags answer the routes' own two questions, per caller.
	flags := func(who string) map[string][2]bool {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/runners", who, ``)
		var rs []struct {
			ID                string `json:"id"`
			CanManage         bool   `json:"canManage"`
			CanReviewHostKeys bool   `json:"canReviewHostKeys"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rs); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		out := map[string][2]bool{}
		for _, r := range rs {
			out[r.ID] = [2]bool{r.CanManage, r.CanReviewHostKeys}
		}
		return out
	}
	for who, wantFlags := range map[string]map[string][2]bool{
		gFinAdmin: {"r-fin": {true, true}, "r-tax": {false, false}, "r-global": {false, false}, "r-legacy": {false, true}},
		gTaxAdmin: {"r-fin": {false, false}, "r-tax": {true, true}, "r-global": {false, false}, "r-legacy": {false, true}},
		gMixed:    {"r-fin": {true, true}, "r-tax": {false, false}, "r-global": {false, false}, "r-legacy": {false, true}},
		gRoot:     {"r-fin": {true, true}, "r-tax": {true, true}, "r-global": {true, true}, "r-legacy": {true, true}},
	} {
		got := flags(who)
		for id, w := range wantFlags {
			if got[id] != w {
				t.Errorf("%s on %s: canManage/canReviewHostKeys = %v, want %v", who, id, got[id], w)
			}
		}
	}
	want := map[string][3]string{
		"r-fin":    {"ag:FIN", "FIN", "false"},
		"r-tax":    {"ag:TAX", "TAX", "false"},
		"r-global": {"global", "Global", "false"},
		"r-legacy": {"global", "Global", "true"},
	}
	if len(rows) != len(want) {
		t.Fatalf("%d runners listed, want %d", len(rows), len(want))
	}
	for _, r := range rows {
		w := want[r.ID]
		if r.OwnerAgency == nil || r.OwnerAgency.ID != w[0] || r.OwnerAgency.Name != w[1] || fmt.Sprint(r.LegacyPlacement) != w[2] {
			t.Errorf("%s: owner %+v legacy %v, want %v", r.ID, r.OwnerAgency, r.LegacyPlacement, w)
		}
	}
}

// LR-62 — binding a scope to a runner takes authority over the SCOPE and a
// runner that serves the scope's agency. It no longer takes authority over the
// runner, so an agency can bind its scope to a runner that serves it and is
// Global's.
func TestG3_BindingTakesTheScopeNotTheRunner(t *testing.T) {
	h, pool := ownerCast(t)
	bind := func(who, scope string, runners ...string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"runnerIds": runners})
		rec := gateReq(t, h, http.MethodPut, "/api/v1/scopes/"+scope+"/runners", who, string(body))
		return rec.Code, errCode(rec.Body.Bytes())
	}
	// FIN binds its scope to the legacy placement, which serves FIN and is
	// Global's. Until 2.3.0 this needed authority over the runner.
	if code, ec := bind(gFinAdmin, "sc:fin", "r-legacy", "r-fin"); code != 200 {
		t.Fatalf("FIN binding its scope to a runner that serves FIN = %d %s, want 200", code, ec)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id = 'sc:fin'`); n != 2 {
		t.Errorf("%d bindings written, want 2", n)
	}
	// Not to a runner that does not serve FIN, whoever owns it.
	for _, r := range []string{"r-tax", "r-global"} {
		if code, ec := bind(gFinAdmin, "sc:fin", "r-fin", r); code != 422 || ec != "runner_not_eligible" {
			t.Errorf("FIN binding its scope to %s = %d %s, want 422 runner_not_eligible", r, code, ec)
		}
	}
	// And authority over the scope is still required: owning a runner that
	// serves the agency gives no say over that agency's scope.
	if code, _ := bind(gTaxAdmin, "sc:fin", "r-legacy"); code != 403 {
		t.Errorf("TAX binding FIN's scope = %d, want 403", code)
	}
	if code, _ := bind(gFinAdmin, "sc:shared", "r-global"); code != 403 {
		t.Errorf("FIN binding Global's scope = %d, want 403", code)
	}
	// Replace: authority over every scope the old runner is bound to, and a
	// replacement that serves each. No authority over either runner.
	replace := func(who, from, to string) (int, string) {
		t.Helper()
		rec := gateReq(t, h, http.MethodPost, "/api/v1/scope-runners/replace", who,
			`{"fromRunnerId":"`+from+`","toRunnerId":"`+to+`"}`)
		return rec.Code, errCode(rec.Body.Bytes())
	}
	if code, _ := replace(gTaxAdmin, "r-legacy", "r-tax"); code != 403 {
		t.Errorf("TAX replacing a runner bound to FIN's scope = %d, want 403", code)
	}
	if code, ec := replace(gFinAdmin, "r-legacy", "r-tax"); code != 422 || ec != "runner_not_eligible" {
		t.Errorf("FIN replacing with a runner that does not serve FIN = %d %s, want 422 runner_not_eligible", code, ec)
	}
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin-2", "fin-agent-2", "ag:FIN")
	if code, ec := replace(gFinAdmin, "r-legacy", "r-fin-2"); code != 200 {
		t.Errorf("FIN replacing the legacy placement on its scope with its own agent = %d %s, want 200", code, ec)
	}
}

// LR-63 — the host-key exception. An administrator of an agency that a runner
// SERVES and does not own may review that runner's keys for the hosts of their
// own agency's scopes: scan such a scope, see what the scan found, approve or
// reject it. Nothing else on the runner, and nothing for another agency's
// scope.
func TestG3_TheHostKeyExceptionIsPerScope(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	pending := func(id, runner, host, scopeID, scopeName string) {
		exec(`INSERT INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at, scope_id, scope_name, host_name)
		      VALUES (?, ?, ?, 'ssh-ed25519', 'SHA256:x', ?, '2026-01-01T00:00:00Z', NULLIF(?, ''), NULLIF(?, ''), ?)`,
			id, runner, host, host+" ssh-ed25519 AAAA", scopeID, scopeName, host)
	}
	pending("pk-fin", "r-legacy", "fin1.example", "sc:fin", "fin-hosts")
	pending("pk-tax", "r-legacy", "tax1.example", "sc:tax", "tax-hosts")
	pending("pk-typed", "r-legacy", "typed.example", "", "")

	scan := func(who, runner, body string) int {
		return gateReq(t, h, http.MethodPost, "/api/v1/runners/"+runner+"/keyscan", who, body).Code
	}
	// Their own scope passes the gate and the handler's own checks; the scope
	// has no hosts on file here, which is the handler's 422 and nothing else.
	if code := scan(gFinAdmin, "r-legacy", `{"scopeId":"sc:fin"}`); code != 422 {
		t.Errorf("FIN scanning its own (empty) scope on a runner that serves it = %d, want the handler's 422", code)
	}
	for name, body := range map[string]string{
		"typed hosts":            `{"hosts":["10.0.0.9"]}`,
		"another agency's scope": `{"scopeId":"sc:tax"}`,
		"Global's scope":         `{"scopeId":"sc:shared"}`,
	} {
		if code := scan(gFinAdmin, "r-legacy", body); code != 403 {
			t.Errorf("FIN scanning %s on a runner it does not own = %d, want 403", name, code)
		}
	}
	// No exception on a runner that does not serve them, or on a Global agent
	// that serves only Global.
	if code := scan(gFinAdmin, "r-tax", `{"scopeId":"sc:fin"}`); code != 403 {
		t.Errorf("FIN scanning with TAX's agent = %d, want 403", code)
	}
	if code := scan(gFinAdmin, "r-global", `{"scopeId":"sc:fin"}`); code != 403 {
		t.Errorf("FIN scanning with Global's agent = %d, want 403", code)
	}
	if code := scan(gFinViewer, "r-legacy", `{"scopeId":"sc:fin"}`); code != 403 {
		t.Errorf("a FIN viewer scanning = %d, want 403", code)
	}

	// The pending list: their own scope's keys, and nobody else's.
	list := func(who string) []string {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/runners/r-legacy/host-keys/pending", who, ``)
		if rec.Code != 200 {
			t.Fatalf("%s listing pending keys = %d (%s)", who, rec.Code, rec.Body)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		ids := []string{}
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		slices.Sort(ids)
		return ids
	}
	if got := list(gFinAdmin); !slices.Equal(got, []string{"pk-fin"}) {
		t.Errorf("FIN sees pending keys %v, want its own scope's only", got)
	}
	if got := list(gTaxAdmin); !slices.Equal(got, []string{"pk-tax"}) {
		t.Errorf("TAX sees pending keys %v, want its own scope's only", got)
	}
	if got := list(gRoot); !slices.Equal(got, []string{"pk-fin", "pk-tax", "pk-typed"}) {
		t.Errorf("the owner sees pending keys %v, want all three", got)
	}

	// The decision: another agency's key, a typed host's key, and a batch that
	// mixes one's own with another's are refused whole.
	resolve := func(who string, reject ...string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"reject": reject})
		return gateReq(t, h, http.MethodPost, "/api/v1/runners/r-legacy/host-keys/resolve-batch", who, string(body)).Code
	}
	// 409, the same answer as for a key that is not pending at all: the refusal
	// must not confirm that another agency's key exists.
	for name, ids := range map[string][]string{
		"TAX's key":               {"pk-tax"},
		"a typed host's key":      {"pk-typed"},
		"its own key and TAX's":   {"pk-fin", "pk-tax"},
		"a key that is not there": {"pk-nope"},
	} {
		if code := resolve(gFinAdmin, ids...); code != 409 {
			t.Errorf("FIN deciding %s = %d, want 409", name, code)
		}
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM pending_host_keys WHERE rejected_at IS NOT NULL OR approved_at IS NOT NULL`); n != 0 {
		t.Fatalf("a refused batch decided %d key(s)", n)
	}
	if code := resolve(gFinAdmin, "pk-fin"); code != 200 {
		t.Errorf("FIN rejecting its own scope's key = %d, want 200", code)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM pending_host_keys WHERE id = 'pk-fin' AND rejected_at IS NOT NULL`); n != 1 {
		t.Error("FIN's decision was not recorded")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM pending_host_keys WHERE id <> 'pk-fin' AND (rejected_at IS NOT NULL OR approved_at IS NOT NULL)`); n != 0 {
		t.Error("FIN's decision touched a key that was not its own")
	}

	// Everything else on the runner stays the owner's.
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/api/v1/runners/r-legacy/host-keys", ``},
		{"POST", "/api/v1/runners/r-legacy/host-keys/provide", `{"lines":["fin1.example ssh-ed25519 AAAA"],"dryRun":true}`},
		{"POST", "/api/v1/runners/r-legacy/host-keys/1/remove", `{}`},
		{"POST", "/api/v1/runners/r-legacy/host-keys/1/resend", `{}`},
		{"POST", "/api/v1/runners/r-legacy/known-hosts/refresh", `{}`},
		{"POST", "/api/v1/runners/r-legacy/host-keys/carry?from=r-fin", `{}`},
		// Carrying needs BOTH runners: FIN's own agent as the target does not
		// make a runner FIN does not own a source it may read.
		{"POST", "/api/v1/runners/r-fin/host-keys/carry?from=r-legacy", `{}`},
		{"POST", "/api/v1/runners/r-fin/host-keys/carry?from=r-tax", `{}`},
		{"POST", "/api/v1/runners/host-keys/pk-tax/resolve", `{"approve":false}`},
		{"POST", "/api/v1/runners/host-keys/pk-fin/resolve", `{"approve":false}`},
		{"POST", "/api/v1/runners/r-legacy/drain", `{}`},
	} {
		if code := gateReq(t, h, r.method, r.path, gFinAdmin, r.body).Code; code != 403 {
			t.Errorf("FIN on a runner it does not own — %s %s = %d, want 403", r.method, r.path, code)
		}
	}
}

// A guest adds the first key for a host of their own scope and never changes
// one. The limit on a guest is the scope a key was scanned for, but what an
// approval writes is the runner's trust for that host, whoever else uses it: a
// host can sit in two agencies' scopes, and a bastion serves many. So a key
// that would replace a trusted one is the runner's owner's to decide.
func TestG3_AHostKeyGuestAddsAKeyAndNeverReplacesOne(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	pendingReal := func(id, host string) (fingerprint string) {
		line, keyType, fp := realHostKey(t, host)
		exec(`INSERT OR REPLACE INTO pending_host_keys (id, runner_id, host, key_type, fingerprint, known_hosts_line, scanned_at, scope_id, scope_name, host_name)
		      VALUES (?, 'r-legacy', ?, ?, ?, ?, '2026-01-01T00:00:00Z', 'sc:fin', 'fin-hosts', ?)`, id, host, keyType, fp, line, host)
		return fp
	}
	approve := func(who string, ack bool, ids ...string) (int, string) {
		t.Helper()
		body := map[string]any{"approve": ids}
		if ack {
			body["acknowledgeChanged"] = ids
		}
		b, _ := json.Marshal(body)
		rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-legacy/host-keys/resolve-batch", who, string(b))
		return rec.Code, errCode(rec.Body.Bytes())
	}
	inForce := func(host string) string {
		t.Helper()
		var fp sql.NullString
		_ = pool.QueryRow(`SELECT fingerprint FROM host_key_ledger
		                    WHERE runner_id = 'r-legacy' AND host = ? AND decision = 'approved' AND superseded_at IS NULL`, host).Scan(&fp)
		return fp.String
	}

	// The first key for a host of FIN's scope: FIN's to approve.
	first := pendingReal("pk-new", "fin1.example")
	code, ec := approve(gFinAdmin, false, "pk-new")
	if code != 200 {
		t.Fatalf("FIN approving the first key for a host of its scope = %d %s, want 200", code, ec)
	}
	if got := inForce("fin1.example"); got != first {
		t.Fatalf("in force for fin1.example = %q, want the key FIN approved (%q)", got, first)
	}
	var batch string
	if err := pool.QueryRow(`SELECT batch_id FROM host_key_ledger WHERE runner_id = 'r-legacy' AND host = 'fin1.example'`).Scan(&batch); err != nil {
		t.Fatal(err)
	}

	// The host presents another key later, scanned for FIN's scope again.
	// Replacing what the runner trusts is not a guest's, acknowledged or not.
	second := pendingReal("pk-changed", "fin1.example")
	for _, ack := range []bool{false, true} {
		if code, ec := approve(gFinAdmin, ack, "pk-changed"); code != 403 || ec != "owner_required" {
			t.Errorf("FIN replacing a trusted key (acknowledged=%v) = %d %s, want 403 owner_required", ack, code, ec)
		}
	}
	if got := inForce("fin1.example"); got != first {
		t.Fatalf("a refused replacement changed what the runner trusts: %q, want %q", got, first)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM pending_host_keys WHERE id = 'pk-changed' AND approved_at IS NULL AND rejected_at IS NULL`); n != 1 {
		t.Error("a refused replacement resolved the pending key")
	}
	// The owner decides it, with the acknowledgement the review screen asks for.
	if code, ec := approve(gRoot, true, "pk-changed"); code != 200 {
		t.Fatalf("the owner replacing the key = %d %s, want 200", code, ec)
	}
	if got := inForce("fin1.example"); got != second {
		t.Errorf("in force after the owner's decision = %q, want %q", got, second)
	}

	// The batch record is the runner's owner's to read.
	if code := gateReq(t, h, http.MethodGet, "/api/v1/host-key-batches/"+batch, gRoot, ``).Code; code != 200 {
		t.Errorf("the owner reading the batch = %d, want 200", code)
	}
	for _, who := range []string{gFinAdmin, gTaxAdmin} {
		if code := gateReq(t, h, http.MethodGet, "/api/v1/host-key-batches/"+batch, who, ``).Code; code != 403 {
			t.Errorf("%s reading a batch of a runner Global owns = %d, want 403", who, code)
		}
	}
}

// A deregistered agent's record is still its former owner's: its host-key
// ledger can be read, and its keys carried to the agent that replaces it, by
// the agency that owned it — and by no other agency.
func TestG3_ADeregisteredAgentsRecordStaysItsOwners(t *testing.T) {
	h, pool := ownerCast(t)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/runners/r-fin", gFinAdmin, ``); rec.Code != 204 {
		t.Fatalf("deregister = %d (%s)", rec.Code, rec.Body)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_placement_history WHERE runner_id = 'r-fin' AND owner_agency = 'ag:FIN'`); n != 1 {
		t.Fatalf("the snapshot did not keep the owner (%d rows)", n)
	}
	if code := gateReq(t, h, http.MethodGet, "/api/v1/runners/r-fin/host-keys", gFinAdmin, ``).Code; code == 403 {
		t.Errorf("FIN reading its deregistered agent's ledger = 403, want past the gate")
	}
	if code := gateReq(t, h, http.MethodGet, "/api/v1/runners/r-fin/host-keys", gTaxAdmin, ``).Code; code != 403 {
		t.Errorf("TAX reading FIN's deregistered agent's ledger = %d, want 403", code)
	}
	exec := mustExec(t, pool)
	seedBindingRunner(exec, "r-fin-2", "fin-agent", "ag:FIN")
	if code := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-fin-2/host-keys/carry?from=r-fin", gFinAdmin, `{}`).Code; code == 403 {
		t.Errorf("FIN carrying keys from its deregistered agent to its new one = 403, want past the gate")
	}
	seedBindingRunner(exec, "r-tax-2", "tax-agent-2", "ag:TAX")
	if code := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-tax-2/host-keys/carry?from=r-fin", gTaxAdmin, `{}`).Code; code != 403 {
		t.Errorf("TAX carrying keys from FIN's deregistered agent = %d, want 403", code)
	}
}

// MA-32 through the API: accepting a restore is the runner's owner's, it
// re-points that agency's bindings, and it adds nothing to what the runner
// serves.
func TestG3_RestoreIsTheOwnersAndNeverWidens(t *testing.T) {
	h, pool := ownerCast(t)
	exec := mustExec(t, pool)
	// The legacy placement was bound to a scope of each agency, then removed.
	exec(`INSERT INTO scope_runners (scope_id, runner_id, runner_name, bound_by, bound_at)
	      VALUES ('sc:fin','r-legacy','legacy','t','2026-01-01T00:00:00Z'), ('sc:tax','r-legacy','legacy','t','2026-01-01T00:00:00Z')`)
	if rec := gateReq(t, h, http.MethodDelete, "/api/v1/runners/r-legacy", gRoot, ``); rec.Code != 204 {
		t.Fatalf("deregister = %d (%s)", rec.Code, rec.Body)
	}
	// FIN enrols an agent under the same name.
	seedBindingRunner(exec, "r-new", "legacy", "ag:FIN")

	rec := gateReq(t, h, http.MethodGet, "/api/v1/runners", gFinAdmin, ``)
	var rows []struct {
		ID                  string `json:"id"`
		PlacementSuggestion *struct {
			HistoryID int64    `json:"historyId"`
			Scopes    []string `json:"scopes"`
			Agencies  []struct {
				ID string `json:"id"`
			} `json:"agencies"`
		} `json:"placementSuggestion"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	var histID int64
	for _, r := range rows {
		if r.ID != "r-new" {
			if r.PlacementSuggestion != nil {
				t.Errorf("%s was offered a restore: %+v", r.ID, r.PlacementSuggestion)
			}
			continue
		}
		if r.PlacementSuggestion == nil {
			t.Fatal("FIN's re-enrolled agent was offered nothing")
		}
		s := r.PlacementSuggestion
		if !slices.Equal(s.Scopes, []string{"fin-hosts"}) || len(s.Agencies) != 1 || s.Agencies[0].ID != "ag:FIN" {
			t.Errorf("offer = scopes %v agencies %v, want FIN's scope and FIN alone", s.Scopes, s.Agencies)
		}
		histID = s.HistoryID
	}
	body := fmt.Sprintf(`{"historyId":%d}`, histID)
	if code := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement", gTaxAdmin, body).Code; code != 403 {
		t.Errorf("TAX accepting a restore onto FIN's agent = %d, want 403", code)
	}
	if code := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement/dismiss", gTaxAdmin, body).Code; code != 403 {
		t.Errorf("TAX dismissing a restore offered to FIN's agent = %d, want 403", code)
	}
	// The snapshot still holds TAX's binding, so the offer is TAX's as well and
	// FIN cannot withdraw it for everyone; and a FIN agent that merely carries
	// the name of a runner it has nothing to restore from cannot dismiss at all.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement/dismiss", gFinAdmin, body); rec.Code != 409 || errCode(rec.Body.Bytes()) != "placement_shared" {
		t.Errorf("FIN dismissing an offer TAX shares = %d %s, want 409 placement_shared", rec.Code, errCode(rec.Body.Bytes()))
	}
	seedBindingRunner(exec, "r-tax-new", "legacy", "ag:TAX")
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_placement_history WHERE id = ? AND dismissed_at IS NOT NULL`, histID); n != 0 {
		t.Fatal("a refused dismissal marked the snapshot")
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement", gFinAdmin, body); rec.Code != 204 {
		t.Fatalf("FIN accepting its own agent's restore = %d (%s)", rec.Code, rec.Body)
	}
	// Nothing of FIN's is left in it: FIN can no longer dismiss what is now
	// only TAX's offer, and TAX's own agent still can.
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement/dismiss", gFinAdmin, body); rec.Code != 409 || errCode(rec.Body.Bytes()) != "placement_stale" {
		t.Errorf("FIN dismissing TAX's offer = %d %s, want 409 placement_stale", rec.Code, errCode(rec.Body.Bytes()))
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM runner_placement_history WHERE id = ? AND dismissed_at IS NOT NULL`, histID); n != 0 {
		t.Fatal("FIN dismissed an offer that was only TAX's")
	}
	if got := served(t, pool, "r-new"); !slices.Equal(got, []string{"ag:FIN"}) {
		t.Errorf("after the restore the agent serves %v, want [ag:FIN]: a restore never adds a serve row", got)
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id = 'sc:fin' AND runner_id = 'r-new'`); n != 1 {
		t.Error("FIN's binding was not re-pointed")
	}
	if n := count(t, pool, `SELECT COUNT(*) FROM scope_runners WHERE scope_id = 'sc:tax' AND runner_id = 'r-legacy'`); n != 1 {
		t.Error("TAX's binding moved or vanished; it must stay on the old id until TAX re-points it")
	}
	if rec := gateReq(t, h, http.MethodPost, "/api/v1/runners/r-new/placement", gFinAdmin, body); rec.Code != 409 {
		t.Errorf("accepting twice = %d, want 409 placement_stale", rec.Code)
	}
}

// The retired-pin list on the Scopes page is filtered like the inbox it now
// points at: configureApp on the pin's scope, a global administrator for a job
// with no scope. It listed every pin to every configureApp holder, so the
// banner counted (and named) other agencies' jobs that "Review in Notices"
// then did not show.
func TestG4_TheRetiredPinListIsByAuthority(t *testing.T) {
	h, pool := gateServer(t)
	exec := mustExec(t, pool)
	for _, p := range [][3]string{{"j-fin", "fin-deploy", "fin-hosts"}, {"j-tax", "tax-deploy", "tax-hosts"}, {"j-none", "sweep", ""}} {
		exec(`INSERT INTO retired_runner_pins (job_uid,job_name,job_source,scope,runner_tag,reason,recorded_at)
		      VALUES (?,?,'cronomicon',?,'vlan-dmz','mixed_pins','2026-10-05T00:00:00Z')`, p[0], p[1], p[2])
	}
	list := func(who string) []string {
		t.Helper()
		rec := gateReq(t, h, http.MethodGet, "/api/v1/scope-binding-notices", who, ``)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d (%s)", who, rec.Code, rec.Body)
		}
		var rows []struct {
			JobName string `json:"jobName"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body)
		}
		out := []string{}
		for _, r := range rows {
			out = append(out, r.JobName)
		}
		slices.Sort(out)
		return out
	}
	for who, want := range map[string][]string{
		gFinAdmin: {"fin-deploy"},
		gTaxAdmin: {"tax-deploy"},
		gMixed:    {"fin-deploy"},
		gRoot:     {"fin-deploy", "sweep", "tax-deploy"},
	} {
		if got := list(who); !slices.Equal(got, want) {
			t.Errorf("%s sees retired pins %v, want %v", who, got, want)
		}
	}
	// And the two lists agree, pin for pin: what the banner counts is what the inbox shows.
	for _, who := range []string{gFinAdmin, gRoot} {
		inbox := 0
		for k := range listNotices(t, h, who) {
			if strings.HasPrefix(k, "retired_runner_pin/") {
				inbox++
			}
		}
		if banner := len(list(who)); banner != inbox {
			t.Errorf("%s: the Scopes banner counts %d pins and the inbox shows %d", who, banner, inbox)
		}
	}
}
