package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
)

// mintToken drives HandleMintRegistrationToken directly (the route's
// requirePerm guard is middleware; identity is attribution-only here).
func mintToken(t *testing.T, svc *Service, label string) (id int64, plaintext string) {
	t.Helper()
	body := "{}"
	if label != "" {
		b, _ := json.Marshal(map[string]string{"label": label})
		body = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/registration-tokens",
		strings.NewReader(body))
	rec := httptest.NewRecorder()
	svc.HandleMintRegistrationToken(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	if resp.Token == "" || !strings.HasPrefix(resp.Token, tokenPrefix) {
		t.Fatalf("mint returned no usable plaintext: %q", resp.Token)
	}
	return resp.ID, resp.Token
}

func registerWith(t *testing.T, svc *Service, token, name string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"name":%q,"os":"Linux","capabilities":["bash"],"version":"1.0","protocolVersion":%d}`, name, runnerproto.ProtocolVersion)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/register", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	svc.HandleRegisterRunner(rec, req)
	return rec
}

// TestMintListRevoke: mint returns plaintext once with the label; the list
// never carries plaintext and reflects status; revoke kills an unused token
// (and registration with it then fails); revoking a used token is 409.
func TestMintListRevoke(t *testing.T) {
	svc := newTestService(t)

	id1, tok1 := mintToken(t, svc, "web-01")
	id2, tok2 := mintToken(t, svc, "")

	// Minting must NOT revoke other tokens (multiple active rows, Phase 7).
	list := listTokens(t, svc)
	if len(list) != 2 {
		t.Fatalf("list: got %d rows, want 2", len(list))
	}
	for _, item := range list {
		if item.Status != "pending" {
			t.Errorf("token %d status = %q, want pending (mint must not revoke siblings)", item.ID, item.Status)
		}
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), tok1) || strings.Contains(string(raw), tok2) {
		t.Fatalf("list response leaked a plaintext token")
	}
	// Label round-trips.
	var found bool
	for _, item := range list {
		if item.ID == id1 && item.Label != nil && *item.Label == "web-01" {
			found = true
		}
	}
	if !found {
		t.Errorf("minted label not present in list: %s", raw)
	}

	// Revoke the unused token 2 → registration with it fails.
	if got := revokeToken(t, svc, id2).Code; got != http.StatusNoContent {
		t.Fatalf("revoke unused: got %d, want 204", got)
	}
	if got := registerWith(t, svc, tok2, "r-revoked").Code; got != http.StatusUnauthorized {
		t.Errorf("register with revoked token: got %d, want 401", got)
	}

	// Use token 1, then revoking it is 409 (the row is the audit record).
	if got := registerWith(t, svc, tok1, "web-01").Code; got != http.StatusCreated {
		t.Fatalf("register with minted token: got %d, want 201", got)
	}
	rec := revokeToken(t, svc, id1)
	if rec.Code != http.StatusConflict {
		t.Errorf("revoke used token: got %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}

	// Unknown id → 404.
	if got := revokeToken(t, svc, 99999).Code; got != http.StatusNotFound {
		t.Errorf("revoke unknown: got %d, want 404", got)
	}

	// A present-but-malformed body is rejected (422), not silently minted
	// unlabeled — an empty body stays fine (covered by the unlabeled mint above).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/registration-tokens",
		strings.NewReader(`{"label": web-01}`))
	rec = httptest.NewRecorder()
	svc.HandleMintRegistrationToken(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("malformed mint body: got %d, want 422", rec.Code)
	}
}

// TestSingleUseConsumption: the first registration consumes the token and
// records the audit trail on both sides; the second registration fails with
// the distinct token_used error naming the consuming runner — and distinct
// from the expired error.
func TestSingleUseConsumption(t *testing.T) {
	svc := newTestService(t)
	id, tok := mintToken(t, svc, "db-runner")

	rec := registerWith(t, svc, tok, "db-runner-01")
	if rec.Code != http.StatusCreated {
		t.Fatalf("first register: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		Runner struct {
			ID                  string `json:"id"`
			RegistrationTokenID *int64 `json:"registrationTokenId"`
		} `json:"runner"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.Runner.RegistrationTokenID == nil || *created.Runner.RegistrationTokenID != id {
		t.Errorf("runner provenance registrationTokenId = %v, want %d", created.Runner.RegistrationTokenID, id)
	}

	// Token row records the consumer.
	var usedAt, usedBy string
	if err := svc.db.QueryRow(`SELECT used_at, used_by_runner_id FROM registration_tokens WHERE id=?`, id).
		Scan(&usedAt, &usedBy); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if usedAt == "" || usedBy != created.Runner.ID {
		t.Errorf("audit trail: used_at=%q used_by=%q, want consumer %q", usedAt, usedBy, created.Runner.ID)
	}
	// The list resolves the runner name.
	list := listTokens(t, svc)
	if len(list) != 1 || list[0].Status != "active" ||
		list[0].UsedByRunnerName == nil || *list[0].UsedByRunnerName != "db-runner-01" {
		t.Errorf("list after use: %+v, want status active by db-runner-01", list)
	}

	// Second registration with the same token: distinct error naming the runner.
	rec = registerWith(t, svc, tok, "db-runner-02")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second register: got %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "token_used") ||
		!strings.Contains(rec.Body.String(), "db-runner-01") {
		t.Errorf("second register error should carry token_used + consumer name, got: %s", rec.Body.String())
	}
	// No second runner row.
	var n int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&n)
	if n != 1 {
		t.Errorf("runners count = %d after double-use attempt, want 1", n)
	}

	// Expired is a DIFFERENT distinct error.
	_, tokExp := mintToken(t, svc, "late")
	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := svc.db.Exec(`UPDATE registration_tokens SET expires_at=? WHERE used_at IS NULL`, past); err != nil {
		t.Fatalf("expire token: %v", err)
	}
	rec = registerWith(t, svc, tokExp, "late-runner")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "token_expired") {
		t.Errorf("expired register: got %d %s, want 401 token_expired", rec.Code, rec.Body.String())
	}
}

// TestSingleUseRace: N concurrent registrations presenting the same token —
// exactly one wins (the two-runners-one-token race guard is a single atomic
// UPDATE inside the registration transaction).
func TestSingleUseRace(t *testing.T) {
	svc := newTestService(t)
	_, tok := mintToken(t, svc, "contended")

	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = registerWith(t, svc, tok, fmt.Sprintf("racer-%d", i)).Code
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			wins++
		} else if c != http.StatusUnauthorized {
			t.Errorf("unexpected race status %d (want 201 once, 401 otherwise)", c)
		}
	}
	if wins != 1 {
		t.Fatalf("race: %d registrations won with one token, want exactly 1 (codes %v)", wins, codes)
	}
	var rows int
	_ = svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("race: %d runner rows created from one token, want 1", rows)
	}
}

// TestBootstrapTokenUnchanged: the env bootstrap token stays multi-use and
// records no provenance/consumption (Phase 7 leaves it alone).
func TestBootstrapTokenUnchanged(t *testing.T) {
	svc := newTestService(t)

	for i := range 3 {
		rec := registerWith(t, svc, "test-bootstrap-token", fmt.Sprintf("boot-%d", i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("bootstrap register #%d: got %d, want 201 (multi-use)", i, rec.Code)
		}
		var created struct {
			Runner struct {
				RegistrationTokenID *int64 `json:"registrationTokenId"`
			} `json:"runner"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		if created.Runner.RegistrationTokenID != nil {
			t.Errorf("bootstrap registration must carry no token provenance, got %v",
				*created.Runner.RegistrationTokenID)
		}
	}
}

// listTokens reads every token the way the route does before it filters by
// authority (api.handleListRegistrationTokens). It goes through JSON so the
// "never plaintext" assertions see what a client would.
func listTokens(t *testing.T, svc *Service) []registrationTokenInfo {
	t.Helper()
	all, err := svc.RegistrationTokens(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	b, err := json.Marshal(all)
	if err != nil {
		t.Fatalf("encode list: %v", err)
	}
	if strings.Contains(string(b), tokenPrefix) {
		t.Fatalf("the token list carries a plaintext token: %s", b)
	}
	var out []registrationTokenInfo
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return out
}

// revokeToken drives HandleRevokeRegistrationToken.
func revokeToken(t *testing.T, svc *Service, id int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete,
		fmt.Sprintf("/api/v1/runners/registration-tokens/%d", id), nil)
	req.SetPathValue("id", fmt.Sprintf("%d", id))
	rec := httptest.NewRecorder()
	svc.HandleRevokeRegistrationToken(rec, req)
	return rec
}

// ── LR-61 / LR-33: the token names the owner ────────────────────────────────

func seedAgency(t *testing.T, svc *Service, id, name string) {
	t.Helper()
	if _, err := svc.db.Exec(
		`INSERT INTO agencies (id, name, created_at) VALUES (?, ?, '2026-10-07T00:00:00Z')`, id, name); err != nil {
		t.Fatalf("seed agency: %v", err)
	}
}

// mintTokenFor mints a token for an agency. The API layer has authorized the
// agency by the time the service sees it; here it is given directly.
func mintTokenFor(t *testing.T, svc *Service, agencyID string) (int64, string, *httptest.ResponseRecorder) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"agencyId": agencyID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/runners/registration-tokens", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	svc.HandleMintRegistrationToken(rec, req)
	var resp struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.ID, resp.Token, rec
}

func registeredID(t *testing.T, rec *httptest.ResponseRecorder) (runnerID, apiKey string) {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d, want 201 (body %s)", rec.Code, rec.Body)
	}
	var out struct {
		Runner struct {
			ID string `json:"id"`
		} `json:"runner"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	return out.Runner.ID, out.Token
}

// An agent enrolled with an agency's token is owned by that agency and serves
// exactly it. It never serves Global, and it declared none of this itself.
func TestRegistrationSetsTheOwnerAndTheOneServeRow(t *testing.T) {
	svc := newTestService(t)
	seedAgency(t, svc, "ag-tax", "Tax")

	id, tok, rec := mintTokenFor(t, svc, "ag-tax")
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d (body %s)", rec.Code, rec.Body)
	}
	var minted registrationTokenInfo
	_ = json.Unmarshal(rec.Body.Bytes(), &minted)
	if minted.AgencyID != "ag-tax" || minted.AgencyName != "Tax" || minted.Status != "pending" {
		t.Errorf("mint response = %+v, want agency ag-tax/Tax and status pending (the list's word for an unused token)", minted)
	}

	runnerID, _ := registeredID(t, registerWith(t, svc, tok, "tax-agent"))
	var owner string
	if err := svc.db.QueryRow(`SELECT owner_agency FROM runners WHERE id = ?`, runnerID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "ag-tax" {
		t.Errorf("owner_agency = %q, want ag-tax", owner)
	}
	if got := servedBy(t, svc, runnerID); len(got) != 1 || got[0] != "ag-tax" {
		t.Errorf("the agent serves %v, want exactly its owner", got)
	}
	for _, tk := range listTokens(t, svc) {
		if tk.ID == id && (tk.AgencyID != "ag-tax" || tk.Status != "active") {
			t.Errorf("listed token = %+v, want agency ag-tax and status active", tk)
		}
	}

	// A token with no agency named is Global's, as is the bootstrap token's
	// agent: Global-owned, serving Global.
	_, gtok := mintToken(t, svc, "")
	gid, _ := registeredID(t, registerWith(t, svc, gtok, "global-agent"))
	bid, _ := registeredID(t, registerWith(t, svc, "test-bootstrap-token", "bootstrap-agent"))
	for _, rid := range []string{gid, bid} {
		if err := svc.db.QueryRow(`SELECT owner_agency FROM runners WHERE id = ?`, rid).Scan(&owner); err != nil {
			t.Fatal(err)
		}
		if got := servedBy(t, svc, rid); owner != "global" || len(got) != 1 || got[0] != "global" {
			t.Errorf("runner %s: owner %q serves %v, want Global and exactly Global", rid, owner, got)
		}
	}
}

// LR-33: a token for an agency that does not exist is not minted, and a token
// whose agency was deleted afterwards enrols nothing — no fallback into Global,
// no runner row, and the token is not consumed.
func TestRegistrationRefusesADeletedAgencyAndConsumesNothing(t *testing.T) {
	svc := newTestService(t)
	if _, _, rec := mintTokenFor(t, svc, "ag-nowhere"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mint for an unknown agency = %d, want 422 (body %s)", rec.Code, rec.Body)
	}

	seedAgency(t, svc, "ag-gone", "Gone")
	id, tok, rec := mintTokenFor(t, svc, "ag-gone")
	if rec.Code != http.StatusCreated {
		t.Fatalf("mint = %d (body %s)", rec.Code, rec.Body)
	}
	if _, err := svc.db.Exec(`DELETE FROM agencies WHERE id = 'ag-gone'`); err != nil {
		t.Fatalf("delete agency: %v", err)
	}

	rec = registerWith(t, svc, tok, "orphan-agent")
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "agency_gone") {
		t.Fatalf("register with a token for a deleted agency = %d %s, want 401 agency_gone", rec.Code, rec.Body)
	}
	var runners int
	if err := svc.db.QueryRow(`SELECT COUNT(*) FROM runners`).Scan(&runners); err != nil || runners != 0 {
		t.Errorf("%d runner rows after a refused registration (err %v), want 0", runners, err)
	}
	var usedAt *string
	if err := svc.db.QueryRow(`SELECT used_at FROM registration_tokens WHERE id = ?`, id).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt != nil {
		t.Errorf("the refused registration consumed the token (used_at = %q)", *usedAt)
	}
	for _, tk := range listTokens(t, svc) {
		if tk.ID == id && tk.AgencyName != "" {
			t.Errorf("a token for a deleted agency lists agency name %q, want empty", tk.AgencyName)
		}
	}
}

// Runner API keys are revoked by runner id. Names are self-declared and not
// unique, so revoking by name (as both paths did) cut off every runner that
// shared one — with agencies enrolling their own agents, another agency's.
func TestDeregisteringOneOfTwoSameNamedRunnersLeavesTheOthersKey(t *testing.T) {
	for _, path := range []string{"operator", "reaper"} {
		t.Run(path, func(t *testing.T) {
			svc := newTestService(t)
			seedAgency(t, svc, "ag-tax", "Tax")
			seedAgency(t, svc, "ag-fin", "Fin")
			_, taxTok, _ := mintTokenFor(t, svc, "ag-tax")
			_, finTok, _ := mintTokenFor(t, svc, "ag-fin")
			taxID, _ := registeredID(t, registerWith(t, svc, taxTok, "worker-01"))
			finID, _ := registeredID(t, registerWith(t, svc, finTok, "worker-01"))

			live := func(runnerID string) int {
				var n int
				if err := svc.db.QueryRow(
					`SELECT COUNT(*) FROM runner_tokens WHERE runner_id = ? AND revoked_at IS NULL`, runnerID).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			if live(taxID) != 1 || live(finID) != 1 {
				t.Fatalf("live keys before: tax=%d fin=%d, want 1 and 1", live(taxID), live(finID))
			}

			if path == "operator" {
				req := httptest.NewRequest(http.MethodDelete, "/api/v1/runners/"+taxID, nil)
				req.SetPathValue("id", taxID)
				rec := httptest.NewRecorder()
				svc.HandleDeregisterRunner(rec, req)
				if rec.Code != http.StatusNoContent {
					t.Fatalf("deregister = %d (body %s)", rec.Code, rec.Body)
				}
			} else {
				svc.deregisterRunner(context.Background(), taxID, "worker-01")
			}
			if live(taxID) != 0 {
				t.Errorf("the deregistered runner still holds %d live key(s)", live(taxID))
			}
			if live(finID) != 1 {
				t.Errorf("deregistering Tax's worker-01 revoked Fin's: %d live key(s), want 1", live(finID))
			}
		})
	}
}

// A renamed runner (a re-declare changes the name, not the key's created_by)
// must still lose its key when it is deregistered.
func TestDeregisteringARenamedRunnerRevokesItsKey(t *testing.T) {
	svc := newTestService(t)
	_, tok := mintToken(t, svc, "")
	rid, _ := registeredID(t, registerWith(t, svc, tok, "first-name"))
	if _, err := svc.db.Exec(`UPDATE runners SET name = 'second-name' WHERE id = ?`, rid); err != nil {
		t.Fatal(err)
	}
	svc.deregisterRunner(context.Background(), rid, "second-name")
	var n int
	if err := svc.db.QueryRow(
		`SELECT COUNT(*) FROM runner_tokens WHERE runner_id = ? AND revoked_at IS NULL`, rid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a renamed runner kept %d live key(s) after deregistration", n)
	}
}
